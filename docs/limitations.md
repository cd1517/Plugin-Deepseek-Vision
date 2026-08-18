# Limitations

- The supported interception boundary is exact: the source/path pair must be
  `openai-response` + `/v1/responses`, `openai` + `/v1/chat/completions`, or
  `claude` + `/v1/messages`, and the host's final model must be in
  `target_models`. Similar routes and token-count/compact endpoints pass through.
- Release artifacts target Linux, macOS, and Windows on amd64/arm64. Each asset
  is a native CGO build and must match the CLIProxyAPI host platform; do not
  copy `.so`, `.dylib`, or `.dll` files between platforms. CLIProxyAPI also
  supports FreeBSD amd64 dynamic plugins, but v0.3.1 does not publish that
  asset until it has passed native FreeBSD acceptance.
- The plugin calls CLIProxyAPI `host.model.execute` using the OpenAI Responses
  protocol and an ordered primary-plus-fallback model chain. Provider-specific
  protocols and transports are host concerns; image file-ID inputs are not
  supported by the plugin rewrite contract. The plugin does not register a
  CLIProxyAPI server-side tool.
- Responses and Chat accept validated HTTP(S)/data-URI references. Claude
  accepts validated URL and base64 image sources. Unsupported image references
  fail closed rather than being forwarded.
- Anthropic's standard multi-block `tool_result.content` rewrite remains the
  default. Some GLM/Anthropic-compatible deployments consume only
  `content[0]`; enable `claude_tool_result_single_block_compat` to merge the
  rewritten tool-result text into that first block. The workaround is scoped
  to nested tool results and does not alter ordinary Claude user-image blocks.
- Plain string `function_call_output.output` values are valid non-image tool
  results and pass through unchanged. Array outputs are scanned for rich
  `input_image` content and converted as prompt groups.
- The walkers scan all visible input/messages turns, including Chat tool
  messages and Claude `tool_result.content[]`, so images from retained history
  and the current turn are converted. A
  `previous_response_id` does not expose server-side history to the callback;
  images hidden behind that identifier cannot be inspected or rewritten.
- There is normally one host model call per image-bearing prompt item, with all
  of that item's images supplied together in order. Duplicate prompt groups are
  merged and successful group analyses may be reused from a small TTL cache.
  The plugin globally bounds in-flight host callbacks while CLIProxyAPI owns
  provider concurrency, retry, routing, and rate-limit policy. An explicit 413
  from the host causes ordered batch splitting; other failures are not retried
  or classified using provider-specific response text.
- `vision_fallback_models` preserves order and accepts at most three distinct
  models after `vision_model`. Fallback is attempted only for retryable
  upstream HTTP 408/429/5xx, per-attempt timeout, invalid/empty/oversized
  result, or generic host executor errors; parent cancellation and rewrite
  failures stop immediately.
- For an eligible image request in any supported protocol, malformed JSON is a 400, unsupported
  image sources are a 422, configured body/reference/emergency unique-image limits are a
  413 with a category-specific public message and a content-free `host.log`
  diagnostic, and VLM/timeout/invalid-result/rewrite failures are a 502. Failures are
  fail-closed and never forward the original image; non-eligible requests pass
  through by design.
- When the runtime is unavailable before normal discovery, targeted malformed
  or image-shaped supported requests are conservatively terminated with 502;
  this lifecycle fallback does not terminate unrelated models.
- The response stream is not modified. Preprocessing must finish before the
  host begins delivering a stream, so VLM latency contributes to first-byte
  latency.
- The process-local cache capacity and data-URI/URL TTLs are configurable
  (defaults: 128 entries, 15 minutes, and 2 minutes). Reconfigure/restart clears
  it. It is not distributed, so another CLIProxyAPI process may repeat the analysis.
- Reanalysis defaults to `refresh`; a new call ID runs once and an identical
  replay is idempotent for the call ID plus decoded-image/normalized-URL
  fingerprints, focus, normalized language, and full ordered model chain
  (detail affects the returned image fingerprint; cache mode is not a separate
  identity field). `no_store` does not read or write cross-request cache state.
  `analysis_cache_size: 0` disables only the ordinary analysis LRU; the
  separate bounded generation-local refresh idempotency cache remains.
- The opt-in full-context trace is process-local and intentionally stores
  plaintext user/image/model data. It is a diagnostic capture, not an audit log,
  and must not be left enabled as normal production logging.
- `deepseek-v4-pro` is retained as a future-supported target, but its Responses
  availability currently depends on the upstream service. It is not required,
  probed, or release-tested in v0.3.1; real validation uses `deepseek-v4-flash`.
- Agent reanalysis is disabled by default and only handles declared
  `view_image`/`deepseek_vision_reanalyze` rich tool output. The plugin trusts
  actual image blocks only. `attachment_ids` are opaque Agent-owned handles;
  the plugin validates 1–16 strings but never resolves or reads them, and they
  cannot supply image bytes. The `deepseek_vision_reanalyze` schema requires a
  1–16 `attachment_ids` array, a
  non-empty `focus` of at most 2,000 characters, `detail` `high`/`original`
  (default `high`), and `cache` `refresh`/`no_store` (default `refresh`).
- At most three active tail call IDs are accepted. Identical `refresh` replay
  for a call ID is idempotent; changing its input is rejected. `no_store` has
  no cross-request persistence.
- Local paths are redacted by default. Even with the opt-in switch, paths are
  retained only when `view_image` is declared and they strictly match
  `.codex/attachments/<id>/`; broader or untrusted paths are removed.
- Structured 502 errors expose only an opaque `error_id`, fixed
  `vision_fallback_exhausted`, and safe attempt summaries. Host executor
  failures remain generic; provider text, credentials, URLs, and local paths
  are not returned.
