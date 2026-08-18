# Configuration

The host passes either the plugin mapping directly or the complete
`plugins.configs.deepseek-vision` document. `config.example.yaml` shows the
complete host shape. All values are validated before an atomic reconfigure;
an invalid update leaves the previous snapshot active.

| Field | Meaning | Default |
| --- | --- | --- |
| `enabled`, `priority` | Host-owned switches | host-defined |
| `target_models` | Final upstream models eligible for interception | `["deepseek-v4-flash"]` |
| `vision_model` | VLM model identifier | `gpt-5.6-luna` |
| `vision_fallback_models` | Ordered VLM candidates selected after `vision_model`; at most three, no duplicates | `[]` |
| `language` | Preferred output language | `zh` |
| `request_timeout_seconds` | Total preprocessing deadline | 120 |
| `max_inflight_vision_requests` | Global in-flight prompt-group VLM calls; excess work queues | 4 |
| `emergency_max_images_per_request` | Last-resort unique-image ceiling | 256 |
| `max_request_bytes` | Raw supported-protocol request body limit | 20 MiB |
| `max_image_reference_bytes` | URL/data URI limit | 15 MiB |
| `max_response_bytes` | VLM response limit | 4 MiB |
| `max_result_chars` | Extracted result limit | 20,000 |
| `analysis_cache_size` | Maximum ordinary prompt-group analysis entries; `0` disables ordinary reuse | 128 |
| `analysis_cache_ttl_seconds` | Data-URI analysis TTL | 900 |
| `analysis_url_cache_ttl_seconds` | URL-image analysis TTL | 120 |
| `agent_reanalysis_enabled` | Opt-in controlled rich tool-output reanalysis | `false` |
| `claude_tool_result_single_block_compat` | Merge rewritten Claude `tool_result` text into `content[0]` for Anthropic-compatible models that ignore later blocks | `false` |
| `trace_enabled` | Full plaintext debug trace | `false` |

The native ABI applies an additional process-wide admission budget of 32 MiB of
raw RPC bytes and four concurrent callbacks. This protects the C-to-Go copy and
subsequent JSON/rewrite allocations; it can reject a request before the larger
per-configuration `max_request_bytes` ceiling is reached.

Limit rejections are diagnosed through CLIProxyAPI's native `host.log`. A 413
warning contains `limit_kind`, `actual`, `maximum`, the active body/reference/
emergency image-count settings, and `config_generation`. ABI admission failures instead
report the ABI request bytes, hard cap, process budget and in-flight usage. No
request body, image reference, header or credential is logged.

The plugin calls `host.model.execute` with OpenAI Responses input and tries the
ordered chain `vision_model`, then each `vision_fallback_models` entry. A
fallback is attempted only for a retryable upstream HTTP status (408/429/5xx),
an attempt timeout, an invalid/empty/oversized result, or a generic host
executor error. A parent request timeout/cancellation, rewrite failure, or
other non-retryable condition stops the chain. CLIProxyAPI routes every model
with its existing provider credentials; the plugin never reads another key.
The nested execution skips this plugin's own interceptor, so it does not
recurse. No additional VLM endpoint or key, and no CLIProxyAPI server-side
tool, is supported or required. CLIProxyAPI owns provider protocol
translation, transport, retry, and credential policy.

The CPAMC form exposes `target_models`, `vision_model`, ordered
`vision_fallback_models`, `language`, global in-flight vision requests, the
emergency image ceiling, total timeout, the three cache controls,
`agent_reanalysis_enabled`, the Claude tool-result single-block compatibility
switch, and a boolean `trace_enabled` switch. Array fields
use JSON array syntax. Their descriptions include bilingual defaults; key
integer controls also state their validation ranges. Advanced size controls
remain available through YAML.

`claude_tool_result_single_block_compat: true` is a narrow workaround for
Anthropic-compatible downstreams such as GLM deployments that consume only
`tool_result.content[0]`. After image preprocessing, the plugin merges the
tool result's existing text, image markers, and joint visual analysis into the
first text block. Outer `tool_result` fields and remaining non-text blocks are
preserved. Direct images in ordinary Claude user-message content retain the
standard multi-block Anthropic shape. Keep the switch disabled unless the
downstream exhibits this compatibility problem.

## Full-context debug trace

`trace_enabled: true` creates `logs/deepseek-vision-trace/events.jsonl` and one
request bundle below `logs/deepseek-vision-trace/requests/`. In the Docker
example this is the host-mounted `./logs/deepseek-vision-trace/` directory.
Each bundle preserves the exact inbound multi-turn body, complete image URLs or
data URIs, discovered image positions and prompt-group context, cache/deduplication plan,
every VLM request and response, parsed VLM result, rewritten request body, and
the final interceptor result. The event stream references the bundle and uses
the host-provided request/trace IDs.

This mode is intentionally high sensitivity. Treat the directory as a complete
copy of user conversations and image data. Authorization, API-key, token,
secret, credential, and cookie header/metadata fields are always replaced with
`[REDACTED]`; image URLs and data URIs are not redacted. Files use mode `0600`,
directories use `0700`, request bundles are capped at 1 GiB by deleting the
oldest complete inactive bundle, and the event stream rotates at 64 MiB with
three backups. Disable the switch immediately after diagnosis. Disabling does
not delete existing traces.

Trace open/write/rotation failures never reject configuration or change a
request result. The plugin disables tracing and emits one ordinary host warning
for the failed generation.

Deprecated `vision_backend`, `vision_base_url`, `vision_api_key_env`,
`per_call_timeout_seconds`, `retry_max_attempts`, `max_concurrency`,
`cache_size`, and `cache_ttl_seconds` fields are accepted only for decoding and
unconditionally ignored. Configure the actual model/provider in CLIProxyAPI.

Each runtime generation owns an LRU using the configured capacity and TTLs.
Keys hash the ordered prompt-group image references, the complete ordered model
chain, normalized language, and complete prompt. Normal image work uses this
TTL cache. Reanalysis defaults to `cache: refresh`: a new call ID runs and
stores a result, while an identical replay for that call ID reuses the
idempotency entry. Its identity is call ID plus decoded-image or normalized URL
fingerprints, focus, normalized language, and the full ordered model chain;
detail affects the returned image fingerprint, while cache mode is not a
separate identity field. A call ID reused with different identity is rejected.
`cache: no_store` runs analysis without reading or writing any cross-request
entry.
Reconfigure or restart creates a fresh cache. Setting `analysis_cache_size: 0`
disables the ordinary analysis LRU while retaining single-request deduplication
and the separate bounded generation-local call-ID idempotency cache for refresh
replay.

`deepseek-v4-pro` is not enabled by default because its Responses endpoint is
not part of the validated release surface. Add it explicitly to
`target_models` only after verifying that upstream path in your deployment.

The VLM prompt is not a generic caption request. All images attached to one
supported message/content/tool-result item are sent together in order. Luna is asked to
label the images, faithfully transcribe text, and describe both individual
content and cross-image relationships. Image text is declared untrusted and
must never be followed as an instruction. The configured language applies to
the explanation while transcription preserves original characters. Up to
2,000 runes of text from the same prompt item are included as bounded context.
The default rewritten prompt tells the non-vision target model that these
attachments have already been analyzed and must not be reopened with
`view_image`; local attachment paths are redacted while the user's request
text is preserved. With controlled reanalysis enabled and `view_image`
declared, the marker allows a new-focus call. The only path exception is a strictly validated path under
`.codex/attachments/<id>/`, and only when `agent_reanalysis_enabled` is true
and `view_image` is declared in the request.

## Controlled agent reanalysis

Reanalysis is driven by declared tools and rich tool output. The plugin trusts
only actual image blocks in the matching output (`input_image`, `image_url`, or
the protocol-native Claude image block); arguments cannot smuggle image bytes,
URLs, or local paths. It does not register or require a CLIProxyAPI server-side
tool.

`deepseek_vision_reanalyze` accepts exactly this argument shape (unknown keys
are rejected):

```json
{
  "attachment_ids": ["id-1", "id-2"],
  "focus": "required task-specific focus",
  "detail": "high",
  "cache": "refresh"
}
```

- `attachment_ids`: required array of 1–16 non-empty opaque handles owned by the
  Agent. The plugin validates them but never resolves, reads, or uses them as
  image sources; only actual output image blocks supply images.
- `focus`: required non-empty string, at most 2,000 characters.
- `detail`: optional `high` or `original`, default `high`.
- `cache`: optional `refresh` or `no_store`, default `refresh`.

The most recent eligible tool output is a tail group. A request may contain at
most three active tail call IDs. A new `refresh` call ID executes once; an
identical replay for that ID is idempotent, and changing its identity
fingerprints or focus/language/model chain is rejected. `no_store` does not
persist across requests. `view_image` accepts only optional `path` and `detail`
arguments, requires a non-empty string when `path` is present, and defaults to
`detail: high`. For a declared, associated active output, malformed arguments,
unsupported fields, and invalid detail values are client errors rather than
image fallbacks. Undeclared, unknown, unassociated, or historical tool outputs
remain ordinary image inputs. The plugin never reads the argument path and
still trusts only image blocks in the tool output.

`max_images_per_request` from older builds remains decodable but is ignored. It
cannot silently restore the former four-block rejection behavior.

## Gate and pass-through rules

The handler requires an exact supported route and the final-model gate:

```text
openai-response + /v1/responses
openai          + /v1/chat/completions
claude          + /v1/messages
final Model in target_models
```

The compact path, unknown image references and unsupported request shapes do not
silently pass an image through: unsupported images terminate with a client
error, while a VLM failure terminates with HTTP 502. A successful rewrite is
idempotent, removes every discovered protocol-native image block and reference
from its original structured position, and verifies that no image block remains.

Existing YAML that omits `vision_fallback_models` and
`agent_reanalysis_enabled` remains valid and receives the defaults above.
Structured 502 responses expose only `error_id`, the fixed
`vision_fallback_exhausted` code, and ordered `attempts` containing `model`,
`category`, optional `upstream_status`, and `retryable`. Host executor details,
provider response text, credentials, complete image references, and local paths
remain generic or redacted.
