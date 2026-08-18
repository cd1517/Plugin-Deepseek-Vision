package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	cfg, err := ParseYAML(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VisionModel != DefaultVisionModel || cfg.Language != DefaultLanguage || cfg.RequestTimeout != 120*time.Second {
		t.Fatalf("defaults = %#v", cfg)
	}
	if cfg.AnalysisCacheSize != 128 || cfg.AnalysisCacheTTL != 15*time.Minute || cfg.URLAnalysisCacheTTL != 2*time.Minute {
		t.Fatalf("cache defaults = %#v", cfg)
	}
	if cfg.MaxInflightVisionRequests != 4 || cfg.EmergencyMaxImagesPerRequest != 256 {
		t.Fatalf("vision scheduling defaults = %#v", cfg)
	}
	if cfg.TraceEnabled {
		t.Fatal("full-context trace must default to disabled")
	}
	if cfg.AgentReanalysisEnabled || len(cfg.VisionFallbackModels) != 0 {
		t.Fatalf("new feature defaults = %#v", cfg)
	}
	if cfg.ClaudeToolResultSingleBlock {
		t.Fatal("Claude tool-result single-block compatibility must default to disabled")
	}
	if len(cfg.TargetModels) != 1 || cfg.TargetModels[0] != "deepseek-v4-flash" {
		t.Fatalf("target models = %#v", cfg.TargetModels)
	}
}

func TestClaudeToolResultSingleBlockCompatConfiguration(t *testing.T) {
	enabled, err := ParseYAML([]byte("claude_tool_result_single_block_compat: true"))
	if err != nil || !enabled.ClaudeToolResultSingleBlock {
		t.Fatalf("enabled compatibility = %#v, err=%v", enabled, err)
	}
	disabled, err := ParseYAML([]byte("claude_tool_result_single_block_compat: false"))
	if err != nil || disabled.ClaudeToolResultSingleBlock {
		t.Fatalf("disabled compatibility = %#v, err=%v", disabled, err)
	}
	wrapper, err := ParseYAML([]byte("plugins:\n  configs:\n    deepseek-vision:\n      claude_tool_result_single_block_compat: true\n"))
	if err != nil || !wrapper.ClaudeToolResultSingleBlock {
		t.Fatalf("wrapped compatibility = %#v, err=%v", wrapper, err)
	}
}

func TestVisionFallbackModelsAndAgentReanalysisConfiguration(t *testing.T) {
	cfg, err := ParseYAML([]byte(`
vision_model: primary
vision_fallback_models: [" fallback-a ", fallback-b]
agent_reanalysis_enabled: true
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"primary", "fallback-a", "fallback-b"}
	models := cfg.VisionModels()
	if strings.Join(models, ",") != strings.Join(want, ",") || !cfg.AgentReanalysisEnabled {
		t.Fatalf("config=%#v models=%#v", cfg, models)
	}
	models[0] = "mutated"
	if cfg.VisionModel != "primary" {
		t.Fatal("VisionModels exposed mutable config storage")
	}
	if err := PublishValidated(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.VisionFallbackModels[0] = "mutated"
	if got := Snapshot().VisionFallbackModels[0]; got != "fallback-a" {
		t.Fatalf("published fallback slice was not cloned: %q", got)
	}

	invalid := []string{
		"vision_model: primary\nvision_fallback_models: [primary]",
		"vision_fallback_models: [a, ' a ']",
		"vision_fallback_models: ['']",
		"vision_fallback_models: [a, b, c, d]",
	}
	for _, raw := range invalid {
		if _, err := ParseYAML([]byte(raw)); err == nil {
			t.Errorf("invalid fallback config accepted: %s", raw)
		}
	}
}

func TestTraceConfiguration(t *testing.T) {
	enabled, err := ParseYAML([]byte("trace_enabled: true"))
	if err != nil || !enabled.TraceEnabled {
		t.Fatalf("enabled trace = %#v, err=%v", enabled, err)
	}
	disabled, err := ParseYAML([]byte("trace_enabled: false"))
	if err != nil || disabled.TraceEnabled {
		t.Fatalf("disabled trace = %#v, err=%v", disabled, err)
	}
}

func TestDeprecatedHostFieldsAreIgnored(t *testing.T) {
	cfg, err := ParseYAML([]byte(`
vision_backend: host
vision_api_key_env: OLD_KEY
per_call_timeout_seconds: 1
retry_max_attempts: 9
max_concurrency: 32
cache_size: 99
cache_ttl_seconds: 5
vision_model: host-vision
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VisionModel != "host-vision" || cfg.AnalysisCacheSize != DefaultAnalysisCacheSize {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestAnalysisCacheConfiguration(t *testing.T) {
	cfg, err := ParseYAML([]byte(`
analysis_cache_size: 3
analysis_cache_ttl_seconds: 60
analysis_url_cache_ttl_seconds: 10
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AnalysisCacheSize != 3 || cfg.AnalysisCacheTTL != time.Minute || cfg.URLAnalysisCacheTTL != 10*time.Second {
		t.Fatalf("cache config = %#v", cfg)
	}
	disabled, err := ParseYAML([]byte("analysis_cache_size: 0"))
	if err != nil || disabled.AnalysisCacheSize != 0 {
		t.Fatalf("disabled cache = %#v, err=%v", disabled, err)
	}
}

func TestDeprecatedExternalFieldsAreUnconditionallyIgnored(t *testing.T) {
	cfg, err := ParseYAML([]byte(`
vision_backend: [external, legacy]
vision_base_url: {old: https://vision.example/v1}
vision_api_key_env: null
vision_model: gpt-5.6-luna
language: zh
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VisionModel != "gpt-5.6-luna" || cfg.Language != "zh" {
		t.Fatalf("active config = %#v", cfg)
	}
}

func TestParseHostWrapperIgnoresHostOwnedSwitches(t *testing.T) {
	raw := []byte(`plugins:
  enabled: false
  configs:
    deepseek-vision:
      enabled: false
      priority: 1
      vision_model: test-model
      max_result_chars: 1000
`)
	cfg, err := ParseYAML(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VisionModel != "test-model" || cfg.MaxResultChars != 1000 {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestParseStoreManagedWrapperIgnoresHostMetadata(t *testing.T) {
	raw, err := os.ReadFile("testdata/store-managed-wrapper.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseYAML(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VisionModel != "store-managed-model" {
		t.Fatalf("vision model = %q", cfg.VisionModel)
	}
	if _, err := ParseYAML([]byte("store: {}\nunknown_plugin_field: true")); err == nil {
		t.Fatal("unknown plugin field accepted")
	}
}

func TestParseRejectsNonemptyTrailingYAMLDocument(t *testing.T) {
	if _, err := ParseYAML([]byte("vision_model: first\n---\nvision_model: second\n")); err == nil {
		t.Fatal("nonempty trailing document accepted")
	}
	if _, err := ParseYAML([]byte("vision_model: first\n---\n# empty\n")); err != nil {
		t.Fatalf("empty trailing document rejected: %v", err)
	}
}

func TestTargetModelsAreCanonicalized(t *testing.T) {
	cfg, err := ParseYAML([]byte("target_models: ['  deepseek-v4-flash  ', 'deepseek-v4-pro ']"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TargetModels[0] != "deepseek-v4-flash" || cfg.TargetModels[1] != "deepseek-v4-pro" {
		t.Fatalf("target models = %#v", cfg.TargetModels)
	}
	if _, err := ParseYAML([]byte("target_models: ['deepseek-v4-flash', ' deepseek-v4-flash ']")); err == nil {
		t.Fatal("duplicate target model accepted")
	}
}

func TestLegacyImageLimitIsIgnored(t *testing.T) {
	cfg, err := ParseYAML([]byte("max_images_per_request: 4"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EmergencyMaxImagesPerRequest != DefaultEmergencyMaxImages {
		t.Fatalf("legacy field changed emergency limit: %d", cfg.EmergencyMaxImagesPerRequest)
	}
}

func TestParseRejectsUnknownAndInvalidValues(t *testing.T) {
	for _, raw := range []string{
		"unknown_field: true",
		"vision_model: ''",
		"language: ''",
		"request_timeout_seconds: 0",
		"emergency_max_images_per_request: 15",
		"max_inflight_vision_requests: 0",
		"max_request_bytes: 1",
		"max_image_reference_bytes: 1",
		"max_response_bytes: 1",
		"max_result_chars: 0",
		"analysis_cache_size: -1",
		"analysis_cache_size: 10001",
		"analysis_cache_ttl_seconds: 0",
		"analysis_url_cache_ttl_seconds: 0",
	} {
		if _, err := ParseYAML([]byte(raw)); err == nil {
			t.Errorf("ParseYAML(%q) error = nil", raw)
		}
	}
}

func TestConfigureIsAtomicOnFailure(t *testing.T) {
	if err := ConfigureYAML([]byte("vision_model: first")); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureYAML([]byte("vision_model: ''")); err == nil {
		t.Fatal("invalid reconfigure returned nil")
	}
	if got := Snapshot().VisionModel; got != "first" {
		t.Fatalf("snapshot changed after failed configure: %q", got)
	}
}

func TestShutdownIdempotent(t *testing.T) {
	Shutdown()
	Shutdown()
	if Snapshot() != nil {
		t.Fatal("Snapshot() is non-nil after shutdown")
	}
	if err := ConfigureYAML([]byte(strings.TrimSpace("{}"))); err != nil {
		t.Fatal(err)
	}
}
