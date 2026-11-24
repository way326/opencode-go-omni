# OpenCode Go Omni

[![Build](https://github.com/way326/opencode-go-omni/actions/workflows/build.yml/badge.svg)](https://github.com/way326/opencode-go-omni/actions/workflows/build.yml)

**OpenCode Go Omni** is a native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) v8 plugin that exposes OpenCode Go subscription models under the provider key **`opencode-go`**. One plugin, one credential pool, four upstream protocol routes:

- OpenAI **Chat Completions**
- Anthropic **Messages**
- OpenAI **Responses**
- Google **Gemini GenerateContent** (`/v1/models/{model}:generateContent`, `:streamGenerateContent?alt=sse`)

Plugin version: **0.2.2**. This project is a maintained fork of [massiveits/opencode-go-cliproxyapi](https://github.com/massiveits/opencode-go-cliproxyapi), revision `a4369b9b1cf18a4111a0167e3b50e5d69054fe45` (plugin 0.1.10); the upstream MIT license and attribution are retained (see [License & attribution](#license--attribution)).

> The original upstream plugin and this fork must **not** be enabled at the same time - both claim the `opencode-go` provider.

## What it is

- **Plugin-only deliverable.** It builds to a single shared library (`-buildmode=c-shared`) that CPA loads from its plugin directory. It opens **no listening socket** and starts no proxy process; all host interaction goes through CPA host HTTP callbacks (`host_callback_id` is propagated for request logging and cancellation).
- **A provider, not a core patch.** CPA keeps ownership of listeners, scheduling, the management UI and native quota surfaces; this repository never modifies or rebuilds the CPA executable.
- **Catalog-driven.** Models discovered from the configured Go subscription catalog share the `opencode-go` provider and credential pool; unknown model families stay visible in diagnostics until an explicit route override supplies their protocol.

## Features

- **Four upstream protocols unified** - Chat Completions, Anthropic Messages, OpenAI Responses and Gemini GenerateContent across one provider and one key pool.
- **Reasoning & payload fidelity** - reasoning effort is mapped across client/upstream formats; interrupted reasoning-only assistant turns keep an explicit empty `content` and their original `reasoning_content`; tool-result images are bridged into legal upstream shapes; native Gemini model-version fields are preserved.
- **Native quota integration** - implements CPA's standard quota provider so the Management Center quota page shows upstream rolling / weekly / monthly windows; a standalone quota page is also available.
- **Multi-key scheduling** - pools API keys with CPA's native scheduler (rotation, retries, cooldowns) across all routes.
- **Session isolation** - a stable `x-opencode-session` header per conversation.
- **Codex Multi-Agent V2 normalization** - textual agent envelopes are normalized before upstream routing under a plaintext contract (see below).
- **Streaming lifecycle guarantees** - single-owner terminal emission, a fixed time budget per phase, and completion checks for translated streams, with native protocol passthrough boundaries described below.

## Requirements

- **CPA v8** with native plugin support.
- **Go 1.26+** and a CGO-capable C compiler *to build* (LLVM-MinGW on Windows). The compiled plugin needs nothing extra at runtime.
- OpenCode Go subscription API keys. **No OAuth** is implemented.

## Install

1. Build the shared library or download a release artifact (see [Build from source](#build-from-source)).
2. Place it in CPA's plugin directory, e.g. `plugins/windows/amd64/opencode-go-omni.dll`.
3. Merge the configuration block below into CPA's `config.yaml` under `plugins.configs.opencode-go-omni`, then refresh the Management Center page.

> Building this repository does **not** install or enable the plugin in a running CPA. Installation, DLL replacement and live acceptance are separate, explicitly authorized steps.

## Configuration

The values below mirror `config.example.yaml`, which is an annotated fragment to merge - it is not applied automatically:

```yaml
plugins:
  enabled: true
  configs:
    opencode-go-cliproxyapi:    # original upstream plugin - keep disabled
      enabled: false
    opencode-go-omni:
      enabled: true
      base-url: "https://opencode.ai/inference/go/v1"
      api-keys:
        - value: "${OPENCODE_GO_API_KEY}"
      model-prefix:
        enabled: true
        value: "opencode-go"
      protocols:
        chat-completions: true
        messages: true
        responses: true
        gemini: true
      catalog:
        refresh-interval: "15m"
        stale-while-unavailable: true
      request-timeout: "5m"
      max-response-bytes: 67108864
      allow-http: false
```

| Key | Notes |
| --- | --- |
| `base-url` | Upstream OpenCode Go API base. |
| `api-keys` | One or more subscription keys; pooled by CPA's scheduler. Prefer `value: "${OPENCODE_GO_API_KEY}"` so the literal is not stored in CPA YAML. |
| `model-prefix` | Public model IDs are prefixed so clients see `opencode-go/...`. |
| `protocols.*` | Enable or disable individual upstream routes. |
| `catalog.refresh-interval` / `catalog.stale-while-unavailable` | Catalog refresh period; keep the last-good catalog while upstream is unavailable. |
| `request-timeout` | Fixed time budget for the opening request and for the streaming pump - not an idle timeout. Active output does not reset the pump budget. |
| `max-response-bytes` | Response size bound. |
| `allow-http` | Keep `false` unless TLS is terminated elsewhere. |

## Authentication & credentials

- API keys are configured under `plugins.configs.opencode-go-omni.api-keys`. The plugin intentionally does **not** register a browser OAuth login (no combined `AuthProvider` capability and no login card).
- New plugin auth files contain only `type`, `id`, and `label`; they do not store `api_key`. For canonical `opencode-go-key-<sha256>` IDs, execution and standard quota resolve the exact selected AuthID from the active plugin config. Legacy `api_key` fields are only consistency checks for these IDs; mismatches fail closed. Noncanonical imported legacy IDs retain the old precedence (`AuthAttributes.api_key`, `AuthMetadata.api_key`, then `StorageJSON`) and never fall back to another configured key.
- The SHA-256 ID is a stable fingerprint, not a random opaque identifier: someone with candidate key values can verify them offline, and identical keys are linkable across installations. This tradeoff preserves deterministic auth filenames; use an HMAC/random-ID design instead if that is outside your threat model.
- Prefer environment references such as `value: "${OPENCODE_GO_API_KEY}"`. Literal keys remain supported for compatibility, but may be exposed through local configuration views. If a key has already been exposed, rotate/revoke it upstream; moving it to an environment variable or hiding a UI preview does not invalidate existing copies.
- Legacy files imported with only `"provider": "opencode-go"` and no `"type"` should be backed up and given `"type": "opencode-go"` before adoption.

## Behavior notes

### Quota

- The standard quota capability queries `GET {base-url}/usage` with the selected credential's Bearer key. The host quota endpoint accepts an `auth_index` - never a browser-supplied key. Reported used percentages are converted to CPA's `remainingFraction` with upstream reset times preserved.
- Missing windows, missing percentages or malformed data return an error rather than a full-quota result; upstream error bodies and credentials are never included in quota errors. Quota reset is unsupported.
- The standalone **OpenCode Go Quota** page shares the same response validation. Page load lists configured credentials without contacting OpenCode; each card refreshes manually and independently.

### Sessions and streaming

- Clients should send a stable unique `x-opencode-session` header on every turn. It takes precedence over CPA's canonical session metadata, other recognized session headers, and the inherited first-user-turn content-hash fallback (which cannot distinguish two conversations with the same opening message).
- On the Chat Completions upstream route, synthesized Responses and Messages streams complete when a valid finish reason and its usage trailer arrive - a redundant `[DONE]` is not required. A valid finish followed by EOF still flushes the terminal; an incomplete trailing line or EOF without a finish reason fails conversion instead of being reported as success.
- Responses-to-Chat/Messages conversion requires a completed/incomplete terminal at EOF. Native Responses keeps its existing EOF passthrough behavior: the plugin does not invent a terminal or require ordinary frames to pass JSON validation. Once a Responses completed/incomplete event arrives, later bytes are ignored for every output format.
- Messages-to-Responses conversion requires a valid stop reason or an explicit message_stop before it can synthesize a completion; partial text followed only by EOF cannot become a successful response. A valid stop reason followed by EOF remains supported even without the redundant message_stop event. Native Messages retains its passthrough behavior.
- For recognized Codex clients (observed `Codex Desktop` headers and the `codex_cli_rs` user agent), a successful `response.completed` is held until converter finalization passes, then emitted as the final data-only SSE chunk immediately before downstream EOF. This compatibility behavior depends on the CPA core's Responses framer - **retest after core upgrades**. WebSocket is outside this fix.
- Plugin shutdown stops accepting new work and drains active streams and host callbacks before returning to the loader. Debug builds record slow drains after the diagnostic threshold; callbacks that cannot be interrupted may delay shutdown until they return. Real Unix unloading requires separate platform acceptance.

### Codex Multi-Agent V2

- Enable CPA's `client.codex.optimize-multi-agent-v2` and use fresh Codex sessions so the producer supplies plaintext task strings. Textual `agent_message` envelopes become ordinary user messages, preserving ordered task content and sender/recipient attribution.
- Recognized Fernet envelopes are rejected, and other content types or unknown agent fields are rejected rather than silently dropped. The plugin does not decrypt anything.

### Model IDs

- Chat, Messages and Responses responses restore the requested public model ID in response metadata, including streaming lifecycle events. An exact match to the upstream target is required; other reported model IDs remain unchanged so clients can still detect mismatches.

## Build from source

```powershell
# Windows (AMD64), local development (debug tag, per CONTRIBUTING.md)
go build -tags debug -buildmode=c-shared -o plugins/windows/amd64/opencode-go-omni.dll .

# Windows packaging (release DLL + zip + sha256 under dist/)
.\build.ps1 -GoExe <path-to-go> -CCompiler <path-to-gcc>
```

```bash
# Linux (AMD64)
go build -buildmode=c-shared -o plugins/linux/amd64/opencode-go-omni.so .

# macOS (ARM64)
go build -buildmode=c-shared -o plugins/darwin/arm64/opencode-go-omni.dylib .
```

Offline verification:

```powershell
go test -tags debug ./...
go vet ./...
python tools/abi_smoke.py <path-to-built-dll>    # offline C ABI smoke: zero network, zero listeners
```

Tests under `tests/` carry the `live` build tag and are opt-in against a running CPA.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md): focused PRs against `main`, local builds and tests with `-tags debug`, offline unit tests only, and release-note updates for user-facing changes.

Working documents from before the rename to `opencode-go-omni` (validation records and fix plans) are preserved verbatim under [docs/](docs/) with a short index. Version history is kept in [RELEASE_NOTES.md](RELEASE_NOTES.md).

## Compatibility & status

- Plugin ABI 1 / RPC schema 6; compiled against the CPA v8 plugin SDK pinned in `go.mod`. Development validation ran against CPA core v8.0.30-v8.0.31; re-verify the Codex Responses terminal behavior after CPA core upgrades.
- Stable release: `0.2.2` (`v0.2.2` tag). Install the matching package from the [GitHub Releases](https://github.com/way326/opencode-go-omni/releases) page.

## License & attribution

MIT - see [LICENSE](LICENSE). Forked from [massiveits/opencode-go-cliproxyapi](https://github.com/massiveits/opencode-go-cliproxyapi) at revision `a4369b9b1cf18a4111a0167e3b50e5d69054fe45` (plugin 0.1.10); the upstream copyright notice and license are retained. Maintained at [way326/opencode-go-omni](https://github.com/way326/opencode-go-omni).
