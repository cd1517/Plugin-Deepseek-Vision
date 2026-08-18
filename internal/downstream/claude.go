package downstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/safety"
)

// claudeAdapter implements the Anthropic Messages wire format.  It is kept
// separate from the OpenAI planners so a rewrite never has to round-trip a
// request through a different protocol shape.
type claudeAdapter struct{}

func (claudeAdapter) Protocol() Protocol { return ProtocolClaude }

func (claudeAdapter) Discover(body []byte, options ...Options) (Plan, error) {
	return discoverClaude(body, options...)
}

type claudePlan struct {
	original                  []byte
	root                      any
	options                   Options
	images                    []Image
	groups                    []claudePromptGroup
	inputItems                int
	allowCodexAttachmentPaths bool
}

// claudePromptGroup carries the protocol-specific path in addition to the
// common PromptGroup consumed by the interceptor.
type claudePromptGroup struct {
	PromptGroup
	container claudeContainerLocation
}

type claudeContainerLocation struct {
	messageIndex    int
	toolResultIndex int // -1 for the message's direct content array
}

type claudeImageLocation struct {
	kind      locationKind
	message   int
	container int // direct content or tool_result block index in message content
	block     int // image index inside the selected content array
	globalPos int
}

type claudePendingImage struct {
	Image
	location claudeImageLocation
}

func discoverClaude(body []byte, options ...Options) (*claudePlan, error) {
	opt := Options{}.normalized()
	if len(options) > 0 {
		opt = options[0].normalized()
	}
	if len(body) > opt.MaxBodyBytes {
		return nil, limitExceeded(LimitRequestBody, len(body), opt.MaxBodyBytes, "request body exceeds configured limit", "body")
	}

	root, err := decodeJSON(body)
	if err != nil {
		return nil, malformed("request body is not valid JSON", "body")
	}
	object, ok := root.(map[string]any)
	if !ok {
		return nil, malformed("Anthropic Messages request must be a JSON object", "body")
	}
	plan := &claudePlan{
		original: append([]byte(nil), body...),
		root:     root,
		options:  opt,
	}
	rawMessages, exists := object["messages"]
	if !exists {
		return plan, nil
	}
	messages, ok := rawMessages.([]any)
	if !ok {
		return nil, malformed("messages must be an array", "messages")
	}
	plan.inputItems = len(messages)

	var userContext userContextIndex
	var pending []claudePendingImage
	globalPos := 0

	for messageIndex, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			return nil, malformed("messages items must be objects", fmt.Sprintf("messages[%d]", messageIndex))
		}
		role, _ := message["role"].(string)
		rawContent, exists := message["content"]
		if !exists {
			continue
		}
		if text, ok := rawContent.(string); ok {
			// Anthropic's string shorthand cannot carry an image attachment, but
			// visible user text still participates in focus fallback for images in
			// later turns.
			globalPos++
			if role == "user" {
				userContext.recordTurn(messageIndex, positionedUserText{pos: globalPos, text: text})
			}
			continue
		}
		content, ok := rawContent.([]any)
		if !ok {
			return nil, malformed("message.content must be a string or array", fmt.Sprintf("messages[%d].content", messageIndex))
		}

		var directImages []claudePendingImage
		var directText []string
		for blockIndex, rawBlock := range content {
			globalPos++
			block, ok := rawBlock.(map[string]any)
			if !ok {
				return nil, malformed("message.content blocks must be objects", fmt.Sprintf("messages[%d].content[%d]", messageIndex, blockIndex))
			}
			typeName, _ := block["type"].(string)
			if role == "user" && typeName != "tool_result" {
				// Pure tool_result messages are protocol continuations, not ordinary
				// user turns. Any other block establishes a user-context boundary,
				// even when it carries no text.
				userContext.recordTurn(messageIndex)
			}
			if typeName == "text" {
				if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
					directText = append(directText, text)
					if role == "user" {
						userContext.recordTurn(messageIndex, positionedUserText{pos: globalPos, text: text})
					}
				}
			}
			if typeName == "image" {
				image, err := discoverClaudeImage(block, claudeImageLocation{
					kind: locationContent, message: messageIndex, container: -1, block: blockIndex, globalPos: globalPos,
				}, len(pending)+1, opt)
				if err != nil {
					return nil, err
				}
				directImages = append(directImages, image)
				pending = append(pending, image)
			}
			if typeName != "tool_result" {
				continue
			}
			rawToolContent, exists := block["content"]
			if !exists {
				continue
			}
			if _, ok := rawToolContent.(string); ok {
				continue
			}
			toolContent, ok := rawToolContent.([]any)
			if !ok {
				return nil, malformed("tool_result.content must be a string or array", fmt.Sprintf("messages[%d].content[%d].content", messageIndex, blockIndex))
			}
			var toolImages []claudePendingImage
			var toolText []string
			for toolBlockIndex, rawToolBlock := range toolContent {
				globalPos++
				toolBlock, ok := rawToolBlock.(map[string]any)
				if !ok {
					return nil, malformed("tool_result.content blocks must be objects", fmt.Sprintf("messages[%d].content[%d].content[%d]", messageIndex, blockIndex, toolBlockIndex))
				}
				toolType, _ := toolBlock["type"].(string)
				if toolType == "text" {
					if text, ok := toolBlock["text"].(string); ok && strings.TrimSpace(text) != "" {
						toolText = append(toolText, text)
					}
				}
				if toolType != "image" {
					continue
				}
				image, err := discoverClaudeImage(toolBlock, claudeImageLocation{
					kind: locationFunctionOutput, message: messageIndex, container: blockIndex, block: toolBlockIndex, globalPos: globalPos,
				}, len(pending)+1, opt)
				if err != nil {
					return nil, err
				}
				toolImages = append(toolImages, image)
				pending = append(pending, image)
			}
			if len(toolImages) == 0 {
				continue
			}
			prompt := joinClaudePrompt(toolText, joinClaudeText(directText), userContext.nearest(messageIndex, toolImages[0].location.globalPos, opt.MaxFocusChars, false))
			group := claudePromptGroup{
				PromptGroup: PromptGroup{ID: len(plan.groups) + 1, InputIndex: messageIndex, Source: "tool_result", Prompt: prompt, locationKind: locationFunctionOutput},
				container:   claudeContainerLocation{messageIndex: messageIndex, toolResultIndex: blockIndex},
			}
			for i := range toolImages {
				toolImages[i].FocusHint = prompt
				group.Images = append(group.Images, toolImages[i].Image)
			}
			plan.groups = append(plan.groups, group)
		}

		// A message's direct images share one VLM call.  Keep the group prompt
		// stable for every image in that message, including text after an image.
		if len(directImages) > 0 {
			prompt := joinClaudePrompt(directText, userContext.nearest(messageIndex, directImages[0].location.globalPos, opt.MaxFocusChars, false))
			group := claudePromptGroup{
				PromptGroup: PromptGroup{ID: len(plan.groups) + 1, InputIndex: messageIndex, Source: "content", Prompt: prompt, locationKind: locationContent},
				container:   claudeContainerLocation{messageIndex: messageIndex, toolResultIndex: -1},
			}
			for i := range directImages {
				directImages[i].FocusHint = prompt
				group.Images = append(group.Images, directImages[i].Image)
			}
			plan.groups = append(plan.groups, group)
		}

	}
	// User text fallback is resolved after the complete history is scanned so
	// an image in an earlier turn can use the nearest visible instruction from
	// either side of the traversal.
	for i := range plan.groups {
		group := &plan.groups[i]
		container, ok := claudeContainerFromRoot(root, group.container)
		if !ok || len(group.Images) == 0 {
			continue
		}
		var primary []string
		var containing []string
		if group.container.toolResultIndex >= 0 {
			for _, rawBlock := range container {
				block, ok := rawBlock.(map[string]any)
				if !ok {
					continue
				}
				if typeName, _ := block["type"].(string); typeName == "text" {
					if text, ok := block["text"].(string); ok {
						primary = append(primary, text)
					}
				}
			}
			if messageContent, ok := claudeMessageContentFromRoot(root, group.container.messageIndex); ok {
				for _, rawBlock := range messageContent {
					block, ok := rawBlock.(map[string]any)
					if !ok {
						continue
					}
					if typeName, _ := block["type"].(string); typeName == "text" {
						if text, ok := block["text"].(string); ok {
							containing = append(containing, text)
						}
					}
				}
			}
		} else {
			for _, rawBlock := range container {
				block, ok := rawBlock.(map[string]any)
				if !ok {
					continue
				}
				if typeName, _ := block["type"].(string); typeName == "text" {
					if text, ok := block["text"].(string); ok {
						primary = append(primary, text)
					}
				}
			}
		}
		group.Prompt = truncateRunes(joinClaudePrompt(primary, joinClaudeText(containing), userContext.nearest(group.InputIndex, group.Images[0].location.globalPos, opt.MaxFocusChars, false)), opt.MaxFocusChars)
		for j := range group.Images {
			group.Images[j].FocusHint = group.Prompt
			for k := range pending {
				if pending[k].Number == group.Images[j].Number {
					pending[k].FocusHint = group.Prompt
				}
			}
		}
	}
	// The direct-message group is emitted after nested tool groups while a
	// message is scanned, but image numbers must follow wire traversal order.
	// Global positions are assigned during the single depth-first scan above,
	// so sorting is deterministic even when a tool_result precedes a direct
	// image in the same message.
	sort.SliceStable(pending, func(i, j int) bool {
		return pending[i].location.globalPos < pending[j].location.globalPos
	})
	numbersByPosition := make(map[int]int, len(pending))
	for i := range pending {
		pending[i].Number = i + 1
		numbersByPosition[pending[i].location.globalPos] = pending[i].Number
	}
	sort.SliceStable(plan.groups, func(i, j int) bool {
		return plan.groups[i].Images[0].location.globalPos < plan.groups[j].Images[0].location.globalPos
	})
	for i := range plan.groups {
		plan.groups[i].ID = i + 1
		for j := range plan.groups[i].Images {
			if number, ok := numbersByPosition[plan.groups[i].Images[j].location.globalPos]; ok {
				plan.groups[i].Images[j].Number = number
			}
		}
	}

	for i := range pending {
		plan.images = append(plan.images, pending[i].Image)
	}
	uniqueReferences := make(map[string]struct{}, len(plan.images))
	for _, image := range plan.images {
		uniqueReferences[image.Reference] = struct{}{}
	}
	if len(uniqueReferences) > opt.MaxImages {
		return nil, imageCountExceeded(len(uniqueReferences), opt.MaxImages, summarizeClaudeImages(plan.images, plan.inputItems), plan.images)
	}
	allowPaths, err := annotateClaudeReanalysis(object, plan.groups, opt, &userContext)
	if err != nil {
		return nil, err
	}
	plan.allowCodexAttachmentPaths = allowPaths
	for i := range plan.groups {
		plan.groups[i].Prompt = sanitizeAttachmentText(plan.groups[i].Prompt, false)
		if plan.groups[i].Tool != nil {
			plan.groups[i].Tool.Focus = plan.groups[i].Prompt
		}
	}
	return plan, nil
}

func discoverClaudeImage(block map[string]any, location claudeImageLocation, number int, opt Options) (claudePendingImage, error) {
	rawSource, exists := block["source"]
	if !exists {
		return claudePendingImage{}, malformed("image requires source", claudeLocationPath(location)+".source")
	}
	source, ok := rawSource.(map[string]any)
	if !ok {
		return claudePendingImage{}, malformed("image.source must be an object", claudeLocationPath(location)+".source")
	}
	typeName, ok := source["type"].(string)
	if !ok || strings.TrimSpace(typeName) == "" {
		return claudePendingImage{}, malformed("image.source.type must be a string", claudeLocationPath(location)+".source.type")
	}
	var reference string
	switch strings.ToLower(typeName) {
	case "base64":
		mediaType, mediaOK := source["media_type"].(string)
		data, dataOK := source["data"].(string)
		if !mediaOK || !dataOK {
			return claudePendingImage{}, malformed("base64 image source requires media_type and data strings", claudeLocationPath(location)+".source")
		}
		reference = "data:" + mediaType + ";base64," + data
	case "url":
		urlValue, urlOK := source["url"].(string)
		if !urlOK {
			return claudePendingImage{}, malformed("url image source requires a url string", claudeLocationPath(location)+".source.url")
		}
		reference = urlValue
	default:
		return claudePendingImage{}, unsupported("image source type is not supported", claudeLocationPath(location)+".source.type")
	}
	if err := safety.ValidateImageReference(reference, opt.MaxReferenceBytes); err != nil {
		if errors.Is(err, safety.ErrImageReferenceTooLarge) {
			return claudePendingImage{}, limitExceeded(LimitImageReference, len(reference), opt.MaxReferenceBytes, "image reference exceeds configured limit", claudeLocationPath(location))
		}
		return claudePendingImage{}, unsupported("image reference must be a valid HTTP(S) URL or data:image URI", claudeLocationPath(location))
	}
	return claudePendingImage{Image: Image{
		Number: number, Reference: reference, InputIndex: location.message, BlockIndex: location.block,
		Source: claudeLocationSource(location.kind), location: imageLocation{kind: location.kind, input: location.message, block: location.block, globalPos: location.globalPos},
	}, location: location}, nil
}

func claudeLocationSource(kind locationKind) string {
	if kind == locationFunctionOutput {
		return "tool_result"
	}
	return "content"
}

func claudeLocationPath(location claudeImageLocation) string {
	if location.kind == locationFunctionOutput {
		return fmt.Sprintf("messages[%d].content[%d].content[%d]", location.message, location.container, location.block)
	}
	return fmt.Sprintf("messages[%d].content[%d]", location.message, location.block)
}

func joinClaudeText(text []string) string {
	var nonEmpty []string
	for _, value := range text {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			nonEmpty = append(nonEmpty, trimmed)
		}
	}
	return strings.Join(nonEmpty, "\n\n")
}

func joinClaudePrompt(primary []string, fallback ...string) string {
	if text := joinClaudeText(primary); text != "" {
		return text
	}
	for _, value := range fallback {
		if text := strings.TrimSpace(value); text != "" {
			return text
		}
	}
	return ""
}

func summarizeClaudeImages(images []Image, inputItems int) ImageCountDetails {
	details := ImageCountDetails{InputItems: inputItems, ImageBlocks: len(images), LastImageItemIndex: -1}
	inputIndexes := make(map[int]struct{})
	references := make(map[string]struct{})
	for _, image := range images {
		inputIndexes[image.InputIndex] = struct{}{}
		references[image.Reference] = struct{}{}
		if image.InputIndex > details.LastImageItemIndex {
			details.LastImageItemIndex = image.InputIndex
		}
		switch image.Source {
		case "content":
			details.ContentImages++
		case "tool_result":
			details.FunctionOutputImages++
		}
	}
	for _, image := range images {
		if image.InputIndex == details.LastImageItemIndex {
			details.LastImageItemBlocks++
		}
	}
	details.ImageInputItems = len(inputIndexes)
	details.UniqueImageReferences = len(references)
	details.DuplicateImageBlocks = details.ImageBlocks - details.UniqueImageReferences
	details.EarlierImageBlocks = details.ImageBlocks - details.LastImageItemBlocks
	return details
}

func (p *claudePlan) Protocol() Protocol { return ProtocolClaude }

func (p *claudePlan) InputItemCount() int {
	if p == nil {
		return 0
	}
	return p.inputItems
}

func (p *claudePlan) ImageCountDetails() ImageCountDetails {
	if p == nil {
		return ImageCountDetails{LastImageItemIndex: -1}
	}
	return summarizeClaudeImages(p.images, p.inputItems)
}

func (p *claudePlan) Images() []Image {
	if p == nil || len(p.images) == 0 {
		return nil
	}
	images := make([]Image, len(p.images))
	copy(images, p.images)
	for i := range images {
		images[i].location = imageLocation{}
	}
	return images
}

func (p *claudePlan) Groups() []PromptGroup {
	if p == nil || len(p.groups) == 0 {
		return nil
	}
	groups := make([]PromptGroup, len(p.groups))
	for i := range p.groups {
		groups[i] = p.groups[i].PromptGroup
		if p.groups[i].Tool != nil {
			tool := *p.groups[i].Tool
			groups[i].Tool = &tool
		}
		groups[i].locationKind = 0
		groups[i].Images = append([]Image(nil), p.groups[i].Images...)
		for j := range groups[i].Images {
			groups[i].Images[j].location = imageLocation{}
		}
	}
	return groups
}

func (p *claudePlan) HasImages() bool { return p != nil && len(p.images) > 0 }

// RewriteGroupsText replaces each Claude image with a text marker and appends
// one joint analysis to the same content container.  The original tree is
// never mutated; all path and result checks happen before the rewritten body
// is returned, preserving the planner's all-or-nothing contract.
func (p *claudePlan) RewriteGroupsText(results []string) ([]byte, error) {
	if p == nil {
		return nil, plannerError(ErrorMalformedRequest, 400, "nil response plan", "body")
	}
	if len(p.groups) == 0 {
		return append([]byte(nil), p.original...), nil
	}
	if len(results) != len(p.groups) {
		return nil, plannerError(ErrorMissingResult, 502, "one VLM result is required for each prompt group", "messages")
	}
	texts := make([]string, len(results))
	for i, result := range results {
		text := strings.TrimSpace(redactGroupText(result, p.images))
		if text == "" {
			return nil, plannerError(ErrorInvalidResult, 502, fmt.Sprintf("VLM group result %d is empty", i+1), "messages")
		}
		if p.options.MaxResultChars > 0 && runeLen(text) > p.options.MaxResultChars {
			return nil, limitExceeded(LimitVLMResult, runeLen(text), p.options.MaxResultChars, "VLM result exceeds configured limit", "messages")
		}
		texts[i] = text
	}

	root, err := cloneJSON(p.root)
	if err != nil {
		return nil, plannerError(ErrorRewriteVerification, 500, "failed to clone request body", "body")
	}
	object, ok := root.(map[string]any)
	if !ok {
		return nil, malformed("Anthropic Messages request must be a JSON object", "body")
	}
	for i, group := range p.groups {
		container, err := claudeGroupContent(object, group.container)
		if err != nil {
			return nil, err
		}
		updated, replaced, err := rewriteClaudeGroup(container, group, texts[i])
		if err != nil {
			return nil, err
		}
		if p.options.ClaudeToolResultSingleBlock && group.container.toolResultIndex >= 0 {
			updated = coalesceClaudeToolResultTextBlocks(updated)
		}
		if replaced != len(group.Images) {
			return nil, plannerError(ErrorRewriteVerification, 500, "not all discovered images were rewritten", claudeContainerPath(group.container))
		}
		if err := setClaudeGroupContent(object, group.container, updated); err != nil {
			return nil, err
		}
	}
	redactAnalyzedAttachmentPaths(root, attachmentPathsToRedact(root, p.allowCodexAttachmentPaths))
	sanitizeAttachmentTree(root, p.allowCodexAttachmentPaths)

	body, err := json.Marshal(root)
	if err != nil {
		return nil, plannerError(ErrorRewriteVerification, 500, "rewritten request could not be encoded", "body")
	}
	check, err := discoverClaude(body, p.options)
	if err != nil || check.HasImages() {
		return nil, plannerError(ErrorRewriteVerification, 500, "rewritten request still contains an image", "body")
	}
	return body, nil
}

func claudeGroupContent(root map[string]any, location claudeContainerLocation) ([]any, error) {
	messages, ok := root["messages"].([]any)
	if !ok || location.messageIndex < 0 || location.messageIndex >= len(messages) {
		return nil, plannerError(ErrorRewriteVerification, 500, "message location changed before rewrite", claudeContainerPath(location))
	}
	message, ok := messages[location.messageIndex].(map[string]any)
	if !ok {
		return nil, plannerError(ErrorRewriteVerification, 500, "message location changed before rewrite", claudeContainerPath(location))
	}
	content, ok := message["content"].([]any)
	if !ok {
		return nil, plannerError(ErrorRewriteVerification, 500, "message content location changed before rewrite", claudeContainerPath(location))
	}
	if location.toolResultIndex < 0 {
		return content, nil
	}
	if location.toolResultIndex >= len(content) {
		return nil, plannerError(ErrorRewriteVerification, 500, "tool_result location changed before rewrite", claudeContainerPath(location))
	}
	toolResult, ok := content[location.toolResultIndex].(map[string]any)
	if !ok {
		return nil, plannerError(ErrorRewriteVerification, 500, "tool_result location changed before rewrite", claudeContainerPath(location))
	}
	toolContent, ok := toolResult["content"].([]any)
	if !ok {
		return nil, plannerError(ErrorRewriteVerification, 500, "tool_result content location changed before rewrite", claudeContainerPath(location))
	}
	return toolContent, nil
}

func claudeContainerFromRoot(root any, location claudeContainerLocation) ([]any, bool) {
	object, ok := root.(map[string]any)
	if !ok {
		return nil, false
	}
	messages, ok := object["messages"].([]any)
	if !ok || location.messageIndex < 0 || location.messageIndex >= len(messages) {
		return nil, false
	}
	message, ok := messages[location.messageIndex].(map[string]any)
	if !ok {
		return nil, false
	}
	content, ok := message["content"].([]any)
	if !ok {
		return nil, false
	}
	if location.toolResultIndex < 0 {
		return content, true
	}
	if location.toolResultIndex >= len(content) {
		return nil, false
	}
	toolResult, ok := content[location.toolResultIndex].(map[string]any)
	if !ok {
		return nil, false
	}
	toolContent, ok := toolResult["content"].([]any)
	return toolContent, ok
}

func claudeMessageContentFromRoot(root any, messageIndex int) ([]any, bool) {
	object, ok := root.(map[string]any)
	if !ok {
		return nil, false
	}
	messages, ok := object["messages"].([]any)
	if !ok || messageIndex < 0 || messageIndex >= len(messages) {
		return nil, false
	}
	message, ok := messages[messageIndex].(map[string]any)
	if !ok {
		return nil, false
	}
	content, ok := message["content"].([]any)
	return content, ok
}

func setClaudeGroupContent(root map[string]any, location claudeContainerLocation, updated []any) error {
	messages, ok := root["messages"].([]any)
	if !ok || location.messageIndex < 0 || location.messageIndex >= len(messages) {
		return plannerError(ErrorRewriteVerification, 500, "message location changed before rewrite", claudeContainerPath(location))
	}
	message, ok := messages[location.messageIndex].(map[string]any)
	if !ok {
		return plannerError(ErrorRewriteVerification, 500, "message location changed before rewrite", claudeContainerPath(location))
	}
	if location.toolResultIndex < 0 {
		message["content"] = updated
		return nil
	}
	content, ok := message["content"].([]any)
	if !ok || location.toolResultIndex >= len(content) {
		return plannerError(ErrorRewriteVerification, 500, "tool_result location changed before rewrite", claudeContainerPath(location))
	}
	toolResult, ok := content[location.toolResultIndex].(map[string]any)
	if !ok {
		return plannerError(ErrorRewriteVerification, 500, "tool_result location changed before rewrite", claudeContainerPath(location))
	}
	toolResult["content"] = updated
	return nil
}

func rewriteClaudeGroup(blocks []any, group claudePromptGroup, analysis string) ([]any, int, error) {
	updated := append([]any(nil), blocks...)
	imageIndex := 0
	for blockIndex, rawBlock := range updated {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			continue
		}
		typeName, _ := block["type"].(string)
		if typeName != "image" {
			continue
		}
		if imageIndex >= len(group.Images) {
			return nil, 0, plannerError(ErrorRewriteVerification, 500, "prompt group image count changed before rewrite", claudeBlockPath(group.container, blockIndex))
		}
		marker := map[string]any{
			"type": "text",
			"text": imageReplacementMarker(group.PromptGroup, imageIndex+1),
		}
		if cacheControl, exists := block["cache_control"]; exists {
			marker["cache_control"] = cacheControl
		}
		updated[blockIndex] = marker
		imageIndex++
	}
	if imageIndex != len(group.Images) {
		return nil, 0, plannerError(ErrorRewriteVerification, 500, "prompt group image locations changed before rewrite", claudeContainerPath(group.container))
	}
	updated = append(updated, map[string]any{"type": "text", "text": RenderGroupResult(group.PromptGroup, analysis)})
	return updated, imageIndex, nil
}

// coalesceClaudeToolResultTextBlocks supports downstream models that only
// consume tool_result.content[0]. It keeps every text fragment in traversal
// order, moves the merged text to the first block, and retains all non-text
// blocks in their original relative order.
func coalesceClaudeToolResultTextBlocks(blocks []any) []any {
	texts := make([]string, 0, len(blocks))
	nonText := make([]any, 0, len(blocks))
	var cacheControl any
	hasCacheControl := false
	for _, rawBlock := range blocks {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			nonText = append(nonText, rawBlock)
			continue
		}
		if !hasCacheControl {
			if value, exists := block["cache_control"]; exists {
				cacheControl = value
				hasCacheControl = true
			}
		}
		if typeName, _ := block["type"].(string); typeName == "text" {
			if value, ok := block["text"].(string); ok {
				texts = append(texts, value)
				continue
			}
		}
		nonText = append(nonText, rawBlock)
	}
	merged := map[string]any{"type": "text", "text": strings.Join(texts, "\n\n")}
	if hasCacheControl {
		merged["cache_control"] = cacheControl
	}
	return append([]any{merged}, nonText...)
}

func claudeContainerPath(location claudeContainerLocation) string {
	if location.toolResultIndex < 0 {
		return fmt.Sprintf("messages[%d].content", location.messageIndex)
	}
	return fmt.Sprintf("messages[%d].content[%d].content", location.messageIndex, location.toolResultIndex)
}

func claudeBlockPath(location claudeContainerLocation, block int) string {
	return fmt.Sprintf("%s[%d]", claudeContainerPath(location), block)
}
