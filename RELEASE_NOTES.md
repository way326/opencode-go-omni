# Release Notes

## Unreleased

### Credential secret exposure remediation

- Write new OpenCode Go auth files as reference-only (`type`, `id`, `label`) and
  resolve canonical selected AuthIDs against an immutable credential directory;
  conflicting legacy key fields and unknown IDs fail closed.
- Migrate only verified matching legacy auth files, preserving unknown/CPA
  metadata and verifying the saved file by reading it back. Existing plaintext
  files that no longer match the configured key remain for management-side
  cleanup rather than being silently rewritten or deleted.
- Treat SHA-256 AuthIDs as stable fingerprints (offline candidate checks and
  cross-installation linkability are possible), not as random opaque IDs. Prefer
  environment-variable references and rotate/revoke keys that have already
  been exposed; UI redaction alone does not revoke a credential.
- Confirm SDK/Core compatibility and exercise the installed CPA ABI before
  treating the change as runtime-verified. Management Center preview/copy
  redaction is a companion frontend change and does not protect raw downloads.

## 0.2.2 - 2026-11-17

### Runtime catalog synchronization

- Derive a stable SHA-256 revision from the exact model projection published by
  `model.static` and `model.for_auth`, covering model IDs, limits, modalities
  and thinking metadata while excluding executor-only route fields.
- Use CPA `host.auth.get` and serialized full-file Auth updates to request the
  existing CPA watcher to refresh the model registry after catalog changes.
  Preserve unknown Auth fields and retry failed or ambiguous writes on a later
  refresh without adding per-Auth workers or a second model snapshot.
- Synchronize initial registration, reconfiguration, stale/fail-closed refresh
  outcomes and ticker updates while draining the previous generation before a
  lifecycle switch.
- Keep Auth callback errors redacted and document the no-CAS limitation of the
  CPA v8.0.28 full-file save ABI.

### Validation and packaging

- Add offline tests for canonical model revisions, AuthGet/AuthSave behavior,
  pending retries, filtering, physical-path deduplication and lifecycle sync.
- Keep the Windows package version aligned at `0.2.2`.

### Claude tool-result image bridge

- Translate image-bearing Anthropic tool results into the legal Chat
  Completions shape: keep a textual role tool receipt with the original call
  ID, then attach the ordered text/image data in a following user message.
- Require a complete, uniquely paired tool batch before emitting attachments.
  Preserve parallel result order and mixed text/image order while rejecting
  orphaned, duplicated, incomplete or malformed image results before any
  upstream inference request.
- Leave existing text-only tool-result translation unchanged. This bridge
  establishes legal conversion, not that every OpenCode Go upstream model can
  infer from the attached image; live model recognition requires separate
  acceptance.

### Review fixes: stream completion, session affinity and shutdown

- Validate EOF completion when converting Responses streams to Chat or
  Messages. Keep native Responses EOF passthrough, and stop at the first
  Responses completed/incomplete event so later bytes cannot become client
  content after a terminal.
- Reject truncated Messages-to-Responses streams that never supplied a valid
  stop reason or explicit message_stop instead of synthesizing success. Preserve
  valid stop-reason-plus-EOF compatibility and native Messages passthrough.
- Derive fallback sessions for native Responses file inputs without letting
  affinity reject content that the native request adapter accepts. Preserve
  explicit session precedence and existing text/image session digests.
- Preserve Chat image detail (`low`, `high`, `auto`) in Responses input images.
- Quiesce new plugin work and drain active streams and host callbacks before
  shutdown returns. The debug slow-drain threshold does not permit unsafe unloading;
  a host callback that cannot be interrupted may delay shutdown until it exits.
  Offline Windows checks do not establish real Unix unload acceptance.

### Code Mode custom tool argument fidelity

- Translate freeform custom tools without an explicit parameters schema into
  function tools with one required string `input` field. Explain the JSON
  wrapper in the upstream tool description while preserving the original tool
  instructions for its contents.
- Encode historical custom calls with the same wrapper on translated Chat and
  Messages routes. Preserve empty input, Unicode, line endings, JSON-looking
  code, call IDs, tool receipts, and namespace identity across subsequent turns.
- On non-GPT Responses routes, use the same argument wrapper for historical
  calls, flatten historical namespace names together with declarations, and
  restore the original name and namespace in returned calls. Convert wrapped
  function argument events into custom input events without leaking the JSON
  transport wrapper into executable code.
- Keep the existing legacy response unwrapping, top-level `apply_patch` filter,
  native GPT Responses passthrough and Gemini SDK route unchanged. Code Mode can
  invoke the local patch tool inside `exec`; no live plugin is installed by a
  source build.
- Recognize the exact `beam-free` model as Chat Completions, following its
  upstream provider metadata; retain unsupported handling for unknown model
  families rather than assigning a guessed route.
- Map translated reasoning controls for `claude-haiku-5-5` to adaptive thinking
  and `output_config.effort`, as required by that model's Messages endpoint.
  Keep explicit off controls, native Messages passthrough, and the budget
  protocol used by other Messages models intact.

## 0.2.1 - 2026-10-16

### Codex Responses terminal delivery at EOF

- On the Chat upstream route, hold a recognized Codex HTTP Responses success
  terminal until Finish validation passes, then send the original terminal JSON
  as the last data-only chunk and close downstream normally. Preserve all
  incremental events, output items, usage fields and account scheduling.
- Give terminal commit and watchdog/error cleanup one owner. A failed terminal
  emit closes with an error; it is never logged as a successful commit. Reject
  duplicate terminals and invalid data already received after completion.
- Preserve the original terminal and framing for length-limited `incomplete`
  replies and other non-success terminal events; do not turn them into success
  or a new transport error.
- Keep other clients, WebSocket and other upstream protocols outside this
  workaround. It relies on CPA v8.0.31 EOF framing behavior and requires
  compatibility checks after core upgrades. Genuine cancellation and the
  existing five-minute request-timeout policy are unchanged.
- Source and debug DLL acceptance do not install or hot-reload the plugin.

### Stream lifecycle and interrupted reasoning

- Stop synthesized Chat-to-Responses and Chat-to-Messages streams after a valid
  finish reason and usage trailer, with terminal events and usage delivered once.
  Keep finish-plus-EOF compatibility; reject incomplete trailing frames and
  synthesized responses that end without a finish reason.
- Give the pump and watchdog one close outcome, track watchdog work through
  shutdown, and notify downstream completion before reclaiming the upstream.
  Apply the same tracked upstream cleanup to streaming HTTP error bootstraps.
- Keep DeepSeek reasoning-only history as an assistant turn with explicit empty
  content instead of null; retain reasoning text, turn boundaries, and nullable
  content on actual tool-call turns.
- Preserve the configured timeout and native Chat clean-EOF behavior. These
  lifecycle changes alone do not fix CPA core's independent 499 usage race;
  the scoped EOF compatibility behavior above addresses terminal delivery.
  Building and offline acceptance do not install or hot-reload a live plugin.

### Codex Multi-Agent V2 plaintext input compatibility

- Normalize textual Responses `agent_message` items once at the shared executor
  boundary before routing to Chat, Messages, Responses or Gemini. Preserve task
  text, ordering and attributed source without forwarding private envelope fields.
- Consume CPA's optimized plaintext task contract; do not decrypt opaque history.
  Reject recognized Fernet envelopes, malformed or unsupported agent content
  before inference. Generic ciphertext detection is not guaranteed.
- Preserve effective Payload precedence, original request metadata, ordinary
  reasoning handling, tool identity and the existing image-result bridge.
- Keep Codex scheduling and lifecycle unchanged. Live V2 and Ultra behavior
  require separate end-to-end acceptance; offline conversion is not that proof.

### Image tool results on the Responses-to-Chat route

- Bridge image-bearing `function_call_output` and `custom_tool_call_output`
  results to an attributed user attachment while retaining their text tool
  receipts and call IDs. Preserve ordered text/image content, base64 or remote
  image URLs, and image detail.
- Complete parallel tool-result batches before adding attachments; reject
  incomplete or ambiguous image-result pairing without sending an inference
  request. Existing text-only tool-result behavior is preserved.
- Keep the shared text-only decoder and other protocol routes unchanged.
  File-ID-only images and file outputs remain unsupported on this bridge.
  Offline verification does not establish upstream vision capability or a
  successful live Codex `view_image` round trip; installation is separate.

### Native OpenCode Go quota support

- Register CPA's standard `QuotaProvider` for `opencode-go`, enabling native
  Management Center quota discovery without changing or rebuilding CPA core.
- Query the selected credential through the host HTTP callback and normalize
  rolling, weekly, and monthly upstream windows into remaining fractions.
- Preserve the separate quota page, support decimal usage percentages, and
  reject missing or malformed windows instead of reporting unused quota.
- Keep credential and upstream error details out of quota errors. Quota reset
  is explicitly unsupported; no inference calls or background quota polling are
  introduced.

### Earlier changes included in this release

#### API-key authentication without a false OAuth entry

- Stop advertising the combined `AuthProvider` capability that made CPA v8.0.28 offer an unsupported browser login and return HTTP 500. OpenCode Go remains an API-key provider; refresh the management page after installing the fix.
- Keep plugin-generated `type: opencode-go` credentials in CPA's scheduling pool. Read the selected key from attributes, metadata, or persisted JSON, without falling back to another configured key.
- Preserve the 0.2.1-dev response model metadata fix and all four protocol routes. Legacy manual files with only `provider` and no `type` require an explicit migration before replacement.
- These are local source changes with offline acceptance; installation and live management-page verification remain separate steps.

OpenAI Codex CLI translation supports tool namespaces, custom programmatic
tools (`exec`), text tool results, and reasoning controls. Supported image tool
results depend on the protocol route described above; model presence and
offline conversion tests do not establish every upstream model's capabilities.

#### Bug fixes

- Fix missing Responses stream item lifecycle completion events (output_text.done, content_part.done, function_call_arguments.done, output_item.done) prior to response.completed, resolving dropped assistant output and tool calls in OpenAI Codex CLI and strict Responses clients.
- Fix handling of in-history messages with role: "system" across protocol adapters without rejecting them as unsupported roles or forwarding invalid turn roles to upstream providers that require alternating user/assistant turns.
- Fix handling of multi-part content arrays in function_call_output.output under /v1/responses decoding.
- Fix upstream HTTP >= 400 error propagation in streaming and non-streaming requests by reading and extracting the upstream error payload instead of discarding it with generic fallback errors.
- Add two-way Responses tool namespace and additional_tools translation: merge dynamic declarations from input items (type: "additional_tools"), unroll client-side grouping tools (type: "namespace") into qualified wire names for upstream models, and restore original tool names and namespaces across non-streaming output and streaming SSE events for OpenAI Codex CLI and multi-agent harnesses.
- Add Codex custom tool normalization and hosted tool dropping: rewrite generic custom tools (`type: "custom"`, e.g. `exec`) into function tools with default object schemas, and drop client-only/hosted tools (`apply_patch`, `web_search`, `web_search_preview`, `tool_search`, `image_generation`) when routing to Chat Completions or Messages endpoints, resolving Codex CLI failures on models like `space-bunny-free`.
- Add bidirectional support for `custom_tool_call` and `custom_tool_call_output` conversation items and outbound custom tool events: translate inbound custom tool calls and results across Chat Completions and Messages protocols, unwrap arguments into clean raw input, and restore `custom_tool_call` and `response.custom_tool_call_input.done` on outbound non-streaming and streaming responses, resolving Codex tool dispatch aborts on programmatic tools like `exec`.
- Remove local reasoning effort validation across request translators: allow client-declared reasoning effort levels (e.g. `xhigh`, `max`, and unlisted levels) to pass through transparently to upstream endpoints without local gatekeeping in `chatcompletions`, `responses`, and `messages` adapters.
- Normalize tools for non-GPT native Responses models: unroll namespaces, convert custom tools (`exec`) into function tools, and sanitize `web_search` definitions when targeting non-GPT Responses endpoints (e.g. `muse-spark`, `grok`), while preserving transparent passthrough for native GPT models (e.g. `gpt-6-luna`).
- Restore custom tool calls across native Responses-to-Responses streaming and non-streaming adapters, ensure historical function_call arguments default to valid JSON via `shared.DefaultArgs` to resolve Muse Spark errors, drop compaction input items to fix Grok compaction blob decode failures, and expand `UnwrapCustomToolInput` to unwrap arguments, code, cmd, and command fields.
- Drop historical `reasoning` input items for non-GPT Responses upstreams: resolve Grok HTTP 400 "Could not decode the compaction blob" decryption errors on multi-turn conversations caused by pooled account credential mismatches on encrypted reasoning blobs.


## Upgrade Notes

- Replace the old plugin binary with the new release binary.
- Restart CLIProxyAPI after replacing the plugin.
- Hard-refresh Management Center if the plugin page looks stale.

**Full Changelog**: https://github.com/massiveits/opencode-go-cliproxyapi/compare/v0.1.9...v0.1.10
