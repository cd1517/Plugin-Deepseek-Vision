package interceptor

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/config"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/vision"
)

func TestHandleClaudeRewritesDirectAndToolResultImages(t *testing.T) {
	analyzer := &batchTestAnalyzer{}
	r := NewRuntime(func(*config.Config) (vision.Analyzer, error) { return analyzer, nil })
	r.Reconfigure(testConfig(t))
	defer r.Shutdown()
	body := `{"model":"deepseek-v4-flash","max_tokens":256,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"inspect both"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"},"cache_control":{"type":"ephemeral"}},` +
		`{"type":"tool_result","tool_use_id":"tool_1","is_error":false,"content":[{"type":"image","source":{"type":"url","url":"https://example.com/tool.png"}}]}` +
		`]}]}`
	resp, err := r.Handle(makeRequest("deepseek-v4-flash", "claude", "/v1/messages", body))
	if err != nil || resp.Terminate {
		t.Fatalf("response=%#v err=%v", resp, err)
	}
	rewritten := string(resp.Body)
	if strings.Contains(rewritten, `"type":"image"`) || strings.Contains(rewritten, "QUJD") || strings.Contains(rewritten, "example.com") || strings.Count(rewritten, "Joint visual analysis") != 2 {
		t.Fatalf("claude rewrite=%s", rewritten)
	}
	for _, required := range []string{`"tool_use_id":"tool_1"`, `"is_error":false`, `"cache_control":{"type":"ephemeral"}`} {
		if !strings.Contains(rewritten, required) {
			t.Fatalf("rewrite missing %s: %s", required, rewritten)
		}
	}
	analyzer.mu.Lock()
	batches := append([][]vision.ImageInput(nil), analyzer.batches...)
	analyzer.mu.Unlock()
	if len(batches) != 2 || len(batches[0]) != 1 || len(batches[1]) != 1 {
		t.Fatalf("batches=%#v", batches)
	}
}

func TestHandleClaudeToolResultSingleBlockCompat(t *testing.T) {
	cfg := testConfig(t)
	cfg.ClaudeToolResultSingleBlock = true
	r := NewRuntime(func(*config.Config) (vision.Analyzer, error) { return &batchTestAnalyzer{}, nil })
	r.Reconfigure(cfg)
	defer r.Shutdown()
	body := `{"messages":[{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"tool_1","is_error":false,"content":[{"type":"text","text":"tool output"},{"type":"image","source":{"type":"url","url":"https://example.com/tool.png"}}]}]}]}`
	resp, err := r.Handle(makeRequest("deepseek-v4-flash", "claude", "/v1/messages", body))
	if err != nil || resp.Terminate {
		t.Fatalf("response=%#v err=%v", resp, err)
	}
	var object map[string]any
	if err := json.Unmarshal(resp.Body, &object); err != nil {
		t.Fatal(err)
	}
	tool := object["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	blocks := tool["content"].([]any)
	if len(blocks) != 1 || !strings.Contains(blocks[0].(map[string]any)["text"].(string), "joint visual analysis") {
		t.Fatalf("compat tool_result=%#v", tool)
	}
	if tool["tool_use_id"] != "tool_1" || tool["is_error"] != false {
		t.Fatalf("tool_result fields changed: %#v", tool)
	}
}

func TestClaudeRouteGateAndNativeErrors(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}]}`
	for _, tc := range []struct {
		name string
		path string
		pass bool
	}{
		{name: "exact", path: "/v1/messages"},
		{name: "count tokens", path: "/v1/messages/count_tokens", pass: true},
		{name: "near path", path: "/v1/message", pass: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRuntime(t, &testAnalyzer{})
			defer r.Shutdown()
			resp, err := r.Handle(makeRequest("deepseek-v4-flash", "claude", tc.path, body))
			if err != nil || resp.Terminate {
				t.Fatalf("response=%#v err=%v", resp, err)
			}
			if gotPass := string(resp.Body) == body; gotPass != tc.pass {
				t.Fatalf("passthrough=%v want=%v body=%s", gotPass, tc.pass, resp.Body)
			}
		})
	}

	r := newTestRuntime(t, &testAnalyzer{})
	defer r.Shutdown()
	malformed, _ := r.Handle(makeRequest("deepseek-v4-flash", "claude", "/v1/messages", `{`))
	if !malformed.Terminate || malformed.StatusCode != http.StatusBadRequest || !strings.Contains(string(malformed.ResponseBody), `"type":"error"`) || !strings.Contains(string(malformed.ResponseBody), `"type":"invalid_request_error"`) {
		t.Fatalf("malformed response=%#v body=%s", malformed, malformed.ResponseBody)
	}
	unsupportedBody := `{"messages":[{"content":[{"type":"image","source":{"type":"file","file_id":"file_1"}}]}]}`
	unsupported, _ := r.Handle(makeRequest("deepseek-v4-flash", "claude", "/v1/messages", unsupportedBody))
	if !unsupported.Terminate || unsupported.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(unsupported.ResponseBody), `"type":"invalid_request_error"`) {
		t.Fatalf("unsupported response=%#v body=%s", unsupported, unsupported.ResponseBody)
	}
}

func TestClaudeAnalyzerAndUnavailableFailuresUseAPIError(t *testing.T) {
	body := `{"messages":[{"content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}]}`
	r := newTestRuntime(t, &testAnalyzer{err: errors.New("secret provider detail")})
	defer r.Shutdown()
	failed, err := r.Handle(makeRequest("deepseek-v4-flash", "claude", "/v1/messages", body))
	if err != nil || !failed.Terminate || failed.StatusCode != http.StatusBadGateway || !strings.Contains(string(failed.ResponseBody), `"type":"api_error"`) || strings.Contains(string(failed.ResponseBody), "secret") {
		t.Fatalf("failed response=%#v err=%v body=%s", failed, err, failed.ResponseBody)
	}

	unavailable, err := HandleUnavailable(makeRequest("deepseek-v4-flash", "claude", "/v1/messages", body), "deepseek-v4-flash")
	if err != nil || !unavailable.Terminate || unavailable.StatusCode != http.StatusBadGateway || !strings.Contains(string(unavailable.ResponseBody), `"type":"error"`) || !strings.Contains(string(unavailable.ResponseBody), `"type":"api_error"`) {
		t.Fatalf("unavailable response=%#v err=%v body=%s", unavailable, err, unavailable.ResponseBody)
	}
}
