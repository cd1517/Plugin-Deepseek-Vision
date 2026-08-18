package downstream

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestClaudeDirectAndToolResultDiscoveryAndRewrite(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet","messages":[` +
		`{"role":"user","content":"earlier string"},` +
		`{"role":"user","content":[` +
		`{"type":"text","text":"Describe both attachments.","keep":true},` +
		`{"type":"image","source":{"type":"url","url":"https://example.com/one.png"},"cache_control":{"type":"ephemeral"}},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAECAw=="}}` +
		`],"message_extra":{"keep":true}},` +
		`{"role":"assistant","content":[` +
		`{"type":"tool_result","tool_use_id":"tool-1","is_error":false,"tool_extra":"keep","content":[` +
		`{"type":"text","text":"Tool screenshot"},` +
		`{"type":"image","source":{"type":"url","url":"https://example.com/tool.png"}}` +
		`]}` +
		`]}` +
		`]}`)
	plan, err := discoverClaude(body)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Protocol() != ProtocolClaude || !plan.HasImages() {
		t.Fatalf("unexpected plan: protocol=%q images=%v", plan.Protocol(), plan.Images())
	}
	images := plan.Images()
	if len(images) != 3 || images[0].Number != 1 || images[2].Number != 3 {
		t.Fatalf("images = %#v", images)
	}
	if images[1].Reference != "data:image/png;base64,AAECAw==" {
		t.Fatalf("base64 reference = %q", images[1].Reference)
	}
	if images[0].FocusHint != "Describe both attachments." || images[2].FocusHint != "Tool screenshot" {
		t.Fatalf("focus hints = %#v", images)
	}
	details := plan.ImageCountDetails()
	wantDetails := ImageCountDetails{
		InputItems: 3, ImageInputItems: 2, ImageBlocks: 3, UniqueImageReferences: 3,
		LastImageItemIndex: 2, LastImageItemBlocks: 1, EarlierImageBlocks: 2,
		ContentImages: 2, FunctionOutputImages: 1,
	}
	if details != wantDetails {
		t.Fatalf("image details = %#v, want %#v", details, wantDetails)
	}
	groups := plan.Groups()
	if len(groups) != 2 || len(groups[0].Images) != 2 || len(groups[1].Images) != 1 {
		t.Fatalf("groups = %#v", groups)
	}

	rewritten, err := plan.RewriteGroupsText([]string{
		"one is https://example.com/one.png and two is data:image/png;base64,AAECAw==",
		"tool result is https://example.com/tool.png",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(rewritten) == string(body) {
		t.Fatal("rewrite did not change body")
	}
	encoded := string(rewritten)
	for _, forbidden := range []string{"type\\\":\\\"image", "example.com/one.png", "example.com/tool.png", "data:image/png;base64", "AAECAw=="} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("rewritten body retained %q: %s", forbidden, encoded)
		}
	}
	if strings.Count(encoded, "Joint visual analysis") != 2 || !strings.Contains(encoded, "[Image 1 — already analyzed") || !strings.Contains(encoded, "[Image 2 — already analyzed") {
		t.Fatalf("group markers/analysis missing: %s", encoded)
	}
	var object map[string]any
	if err := json.Unmarshal(rewritten, &object); err != nil {
		t.Fatal(err)
	}
	messages := object["messages"].([]any)
	message := messages[1].(map[string]any)
	content := message["content"].([]any)
	marker := content[1].(map[string]any)
	if marker["type"] != "text" {
		t.Fatalf("marker = %#v", marker)
	}
	if _, ok := marker["cache_control"]; !ok {
		t.Fatalf("cache_control was not copied: %#v", marker)
	}
	if _, ok := message["message_extra"]; !ok {
		t.Fatal("message extra field was dropped")
	}
	tool := messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tool["tool_use_id"] != "tool-1" || tool["is_error"] != false || tool["tool_extra"] != "keep" {
		t.Fatalf("tool_result fields changed: %#v", tool)
	}
	second, err := discoverClaude(rewritten)
	if err != nil {
		t.Fatal(err)
	}
	if second.HasImages() {
		t.Fatal("rewritten body still has images")
	}
	if again, err := second.RewriteGroupsText(nil); err != nil || string(again) != string(rewritten) {
		t.Fatalf("idempotent rewrite changed body: err=%v body=%s", err, again)
	}
}

func TestClaudeToolResultSingleBlockCompat(t *testing.T) {
	contents := []struct {
		name    string
		content string
		markers int
	}{
		{name: "text then image", content: `[{"type":"text","text":"before"},{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]`, markers: 1},
		{name: "image then text", content: `[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}},{"type":"text","text":"after"}]`, markers: 1},
		{name: "image only", content: `[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]`, markers: 1},
		{name: "multiple images", content: `[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}},{"type":"image","source":{"type":"url","url":"https://example.com/b.png"}}]`, markers: 2},
	}
	for _, test := range contents {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"tool-1","content":` + test.content + `}]}]}`)
			plan, err := discoverClaude(body, Options{ClaudeToolResultSingleBlock: true})
			if err != nil {
				t.Fatal(err)
			}
			rewritten, err := plan.RewriteGroupsText([]string{"analysis result"})
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]any
			if err := json.Unmarshal(rewritten, &object); err != nil {
				t.Fatal(err)
			}
			tool := object["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
			blocks := tool["content"].([]any)
			if len(blocks) != 1 {
				t.Fatalf("compat content = %#v", blocks)
			}
			merged := blocks[0].(map[string]any)["text"].(string)
			if strings.Count(merged, "[Image ") != test.markers || !strings.Contains(merged, "Joint visual analysis") || !strings.Contains(merged, "analysis result") {
				t.Fatalf("merged text = %q", merged)
			}
			second, err := discoverClaude(rewritten, Options{ClaudeToolResultSingleBlock: true})
			if err != nil || second.HasImages() {
				t.Fatalf("rewritten discovery: images=%v err=%v", second.HasImages(), err)
			}
			again, err := second.RewriteGroupsText(nil)
			if err != nil || string(again) != string(rewritten) {
				t.Fatalf("idempotent rewrite changed body: err=%v body=%s", err, again)
			}
		})
	}
}

func TestClaudeToolResultSingleBlockCompatPreservesBoundaries(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/direct.png"}}]},` +
		`{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"tool-1","is_error":true,"extra":"keep","content":[` +
		`{"type":"text","text":"before","cache_control":{"type":"ephemeral"}},` +
		`{"type":"custom_a","value":1},` +
		`{"type":"image","source":{"type":"url","url":"https://example.com/tool.png"}},` +
		`{"type":"text","text":"after"},` +
		`{"type":"custom_b","value":2}` +
		`]}]}]}`)
	plan, err := discoverClaude(body, Options{ClaudeToolResultSingleBlock: true})
	if err != nil {
		t.Fatal(err)
	}
	rewritten, err := plan.RewriteGroupsText([]string{"direct analysis", "tool analysis"})
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(rewritten, &object); err != nil {
		t.Fatal(err)
	}
	messages := object["messages"].([]any)
	direct := messages[0].(map[string]any)["content"].([]any)
	if len(direct) != 2 {
		t.Fatalf("ordinary user content was coalesced: %#v", direct)
	}
	tool := messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tool["tool_use_id"] != "tool-1" || tool["is_error"] != true || tool["extra"] != "keep" {
		t.Fatalf("outer tool_result fields changed: %#v", tool)
	}
	blocks := tool["content"].([]any)
	if len(blocks) != 3 || blocks[1].(map[string]any)["type"] != "custom_a" || blocks[2].(map[string]any)["type"] != "custom_b" {
		t.Fatalf("non-text blocks changed: %#v", blocks)
	}
	merged := blocks[0].(map[string]any)
	text := merged["text"].(string)
	for _, want := range []string{"before", "after", "already analyzed", "Joint visual analysis", "tool analysis"} {
		if !strings.Contains(text, want) {
			t.Fatalf("merged text missing %q: %q", want, text)
		}
	}
	if merged["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Fatalf("cache_control not carried: %#v", merged)
	}
}

func TestClaudePromptFallbackAndIndependentToolGroups(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"old visible request"}]},` +
		`{"role":"assistant","content":[` +
		`{"type":"tool_result","content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]},` +
		`{"type":"tool_result","content":[{"type":"text","text":"second tool prompt"},{"type":"image","source":{"type":"url","url":"https://example.com/b.png"}}]}` +
		`]},` +
		`{"role":"user","content":[{"type":"text","text":"new visible request"}]}` +
		`]}`)
	plan, err := discoverClaude(body)
	if err != nil {
		t.Fatal(err)
	}
	groups := plan.Groups()
	if len(groups) != 2 {
		t.Fatalf("groups = %#v", groups)
	}
	if groups[0].Prompt != "old visible request" {
		t.Fatalf("nearest user fallback = %q", groups[0].Prompt)
	}
	if groups[1].Prompt != "second tool prompt" {
		t.Fatalf("same-container prompt = %q", groups[1].Prompt)
	}
	if groups[0].ID == groups[1].ID {
		t.Fatal("tool_result groups were not independent")
	}
}

func TestClaudeNoImagePassthroughAndAtomicRewrite(t *testing.T) {
	for _, body := range []string{
		`{"model":"claude","messages":[{"role":"user","content":"plain"}]}`,
		`{"model":"claude"}`,
	} {
		plan, err := discoverClaude([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if plan.HasImages() {
			t.Fatal("unexpected image")
		}
		out, err := plan.RewriteGroupsText(nil)
		if err != nil || string(out) != body {
			t.Fatalf("passthrough = %q, err=%v", out, err)
		}
	}

	body := []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}},{"type":"image","source":{"type":"url","url":"https://example.com/b.png"}}]}]}`)
	plan, err := discoverClaude(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.RewriteGroupsText(nil); err == nil {
		t.Fatal("missing result was accepted")
	} else {
		var planner *Error
		if !errors.As(err, &planner) || planner.StatusCode != 502 {
			t.Fatalf("missing result error = %v", err)
		}
	}
	if string(plan.original) != string(body) {
		t.Fatal("failed rewrite mutated original plan")
	}
}

func TestClaudePlannerErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		opt  Options
		kind ErrorKind
		code int
	}{
		{"messages object", `{"messages":{}}`, Options{}, ErrorMalformedRequest, 400},
		{"message object", `{"messages":["bad"]}`, Options{}, ErrorMalformedRequest, 400},
		{"content object", `{"messages":[{"content":{}}]}`, Options{}, ErrorMalformedRequest, 400},
		{"block scalar", `{"messages":[{"content":["bad"]}]}`, Options{}, ErrorMalformedRequest, 400},
		{"missing source", `{"messages":[{"content":[{"type":"image"}]}]}`, Options{}, ErrorMalformedRequest, 400},
		{"source scalar", `{"messages":[{"content":[{"type":"image","source":"bad"}]}]}`, Options{}, ErrorMalformedRequest, 400},
		{"missing base64 fields", `{"messages":[{"content":[{"type":"image","source":{"type":"base64","data":"AA=="}}]}]}`, Options{}, ErrorMalformedRequest, 400},
		{"file source", `{"messages":[{"content":[{"type":"image","source":{"type":"file","file_id":"x"}}]}]}`, Options{}, ErrorUnsupportedImage, 422},
		{"unknown source", `{"messages":[{"content":[{"type":"image","source":{"type":"blob"}}]}]}`, Options{}, ErrorUnsupportedImage, 422},
		{"private URL", `{"messages":[{"content":[{"type":"image","source":{"type":"url","url":"http://127.0.0.1/a.png"}}]}]}`, Options{}, ErrorUnsupportedImage, 422},
		{"invalid base64", `{"messages":[{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"***"}}]}]}`, Options{}, ErrorUnsupportedImage, 422},
		{"reference limit", `{"messages":[{"content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}]}`, Options{MaxReferenceBytes: 8}, ErrorLimitsExceeded, 413},
		{"unique image limit", `{"messages":[{"content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}},{"type":"image","source":{"type":"url","url":"https://example.com/b.png"}}]}]}`, Options{MaxImages: 1}, ErrorLimitsExceeded, 413},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := discoverClaude([]byte(test.body), test.opt)
			if err == nil {
				t.Fatal("expected planner error")
			}
			var planner *Error
			if !errors.As(err, &planner) {
				t.Fatalf("error type = %T: %v", err, err)
			}
			if planner.Kind != test.kind || planner.StatusCode != test.code {
				t.Fatalf("planner error = %#v, want kind=%s code=%d", planner, test.kind, test.code)
			}
		})
	}
}
