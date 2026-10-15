# OpenCode Go Unified CPA Plugin

This local fork derives from [massiveits/opencode-go-cliproxyapi](https://github.com/massiveits/opencode-go-cliproxyapi), revision `a4369b9b1cf18a4111a0167e3b50e5d69054fe45` (plugin 0.1.10). Its plugin identity is **`opencode-go-unified`**, version **`0.2.1-dev`**. The upstream MIT license and attribution are retained.

Version 0.2.1-dev restores the requested public model ID in Chat, Messages, and Responses response metadata, including streaming lifecycle events. An exact match to the upstream target is required; other reported model IDs remain unchanged so clients can still detect a mismatch. Tool payloads, signatures, usage and native Gemini model-version fields are preserved.

The fork adds Google Gemini GenerateContent as a fourth upstream route while retaining the existing Chat Completions, Messages, and Responses adapters. Models discovered from the configured Go subscription catalog share the existing **`opencode-go` provider and credential pool**. Gemini-family models use `/v1/models/{model}:generateContent` or `:streamGenerateContent?alt=sse`. Unknown model families stay visible in diagnostics until an explicit route override supplies their protocol.

All requests use CPA host HTTP callbacks. Inference propagates `host_callback_id` for request logging and cancellation. These native RPC callbacks in CPA v8.0.28 use the global CPA proxy setting, without per-credential or per-request proxy overrides. The plugin opens **no listening socket** and does not start a proxy process. Only the plugin shared library is built; the CPA executable is neither changed nor rebuilt. CPA's pinned SDK translators are compiled into this DLL for Gemini conversion; this reuses conversion code rather than invoking CPA's native Gemini Executor.

The original plugin and this fork must **not both be enabled**, because both claim the same provider. Install/enable and production acceptance are separate from building this deliverable. See [config.example.yaml](config.example.yaml) for a configuration fragment; it is not automatically applied.

Authentication uses **OpenCode Go API keys**, configured under `plugins.configs.opencode-go-unified.api-keys`. The plugin does not implement browser OAuth. It no longer registers CPA's combined `AuthProvider` capability, which previously made CPA display an OAuth login card whose unsupported login handler returned HTTP 500. After installing this fix, refresh the management page; add or change subscription keys in the plugin configuration instead of using OAuth login.

CPA's normal file credential loader continues to load plugin-generated files containing `"type": "opencode-go"`. The executor reads only the credential selected by CPA: historical `AuthAttributes.api_key`, then `AuthMetadata.api_key`, then the raw `StorageJSON` field. A present but invalid source is rejected, and the executor never substitutes the first configured key. Provider identity, four protocol routes, scheduling, and the quota page remain separate from OAuth login. The SDK's internal executor scope value `oauth` means credential-bound model registration here; it does not implement an OAuth exchange.

If you manually imported a legacy file with only `"provider": "opencode-go"` and no `type`, back it up and migrate it to include `"type": "opencode-go"` before adopting this fix. Automatically generated files already include `type`. Verify account counts, disabled states, and model registration after replacement and after a later restart; the generic loader normally identifies accounts by filename. This source change and offline validation do not install or enable a new DLL in an existing CPA process.

For isolated conversations, clients should send a stable unique `x-opencode-session` header on every turn. It takes precedence over CPA's canonical session metadata, then other recognized session headers, then the inherited first-user-turn content hash. The content-hash fallback cannot distinguish two independent conversations with the same opening message.

Native Gemini requests preserve opaque thought signatures and tool schemas; the plugin validates JSON and rewrites the model ID, then forwards the native fields without CPA's Gemini request normalizer. Upstream verifies the signatures. Native responses preserve `functionCall`, `functionResponse`, `thoughtSignature`, and other fields. Cross-format clients use CPA v8.0.28's translators and should retain signature/tool-call carrier fields on subsequent turns. Clients supply conversation history; the fork does not add server-side `previous_response_id` storage, explicit cache APIs, Interactions, or countTokens. Subscription-specific acceptance of native caching fields, tools, and multi-turn signature round trips still needs upstream acceptance testing.

A native dynamic Go plugin for [CLIProxyAPI](https://help.router-for.me/plugin/development) that exposes OpenCode Go as a single provider (`opencode-go`).

The plugin unifies model discovery, protocol translation, and execution across OpenCode Go's upstream endpoints while leveraging CLIProxyAPI's built-in authentication, scheduling, keys rotation, and cooldown management.

## The Problem

OpenCode Go exposes models across multiple API protocols (OpenAI Chat Completions `/v1/chat/completions`, Anthropic Messages `/v1/messages`, and OpenAI Responses `/v1/responses`).

Without this plugin, using OpenCode Go in CLIProxyAPI requires configuring separate provider blocks for each protocol family. This leads to:
- **Duplicated configuration & keys**: The same API keys must be configured across multiple provider blocks.
- **Fragmented scheduling & rotation**: Keys rotation, rate limits, and cooldowns cannot be shared across protocols—exhausting quota on one protocol does not coordinate with another.
- **Client protocol burden**: Clients must know beforehand which upstream protocol and endpoint each model requires.
- **Fragmented catalog**: Models are split across disjoint provider namespaces instead of a unified model list.

## The Solution

This plugin exposes OpenCode Go as a single provider (`opencode-go`) backed by a shared keys pool:
- **Unified auth pool**: Configure keys once; CLIProxyAPI schedules, rotates, and cools down keys across all protocols.
- **Transparent protocol translation & routing**: Clients request models (e.g. `opencode-go/glm-5.2`, `opencode-go/gpt-5.6-luna`) without needing to know the upstream protocol format.
- **Single model catalog**: All models are discovered and published under the `opencode-go` provider namespace in `/v1/models`.

## Features

- **Single Provider Namespace**: Exposes models under the `opencode-go` provider prefix (e.g. `opencode-go/glm-5.2`, `opencode-go/qwen3.7-max`, `opencode-go/gpt-5.6-luna`).
- **Multi-Protocol Translation**: Translates requests and streaming responses between client formats and upstream endpoints:
  - OpenAI Chat Completions (`/v1/chat/completions`)
  - Anthropic Messages (`/v1/messages`)
  - OpenAI Responses (`/v1/responses`)
  - Google Gemini (`/v1/models/{model}:generateContent` / `:streamGenerateContent?alt=sse`)
- **Thinking & Reasoning Support**: Maps reasoning effort across supported client and upstream formats.
- **Dynamic Catalog Discovery**: Fetches remote model catalogs with an in-memory last-good snapshot and custom route overrides.
- **Multi-Key Auth Scheduling**: Pools multiple API keys with CLIProxyAPI's native scheduler for rotation, retries, and error cooldowns across all protocols.
- **OpenCode Go Quota Page**: Management Center includes a separate `OpenCode Go Quota` page. Page load lists credentials without contacting OpenCode; each card is refreshed manually and independently, and quota values do not affect routing or CPA's native quota page.

## Requirements

- **CLIProxyAPI**: `v8.0.28` (the SDK version pinned by this fork)
- **Go Toolchain**: Go 1.26.7+ (with CGO enabled for C-shared build mode)

## Build

Build the dynamic shared library for your platform:

### Windows (AMD64)
```powershell
go build -buildmode=c-shared -o plugins/windows/amd64/opencode-go-unified.dll .
```

### Linux (AMD64)
```bash
go build -buildmode=c-shared -o plugins/linux/amd64/opencode-go-unified.so .
```

### macOS (ARM64)
```bash
go build -buildmode=c-shared -o plugins/darwin/arm64/opencode-go-unified.dylib .
```

Place the compiled binary into your CLIProxyAPI plugin directory (e.g. `<cliproxyapi_root>/plugins/<os>/<arch>/`).

## Configuration

Configure the plugin in your CLIProxyAPI `config.yaml` under `plugins.configs.opencode-go-unified`:

```yaml
plugins:
  configs:
    opencode-go-unified:
      # Upstream base URL (default: "https://opencode.ai/inference/go/v1")
      base-url: "https://opencode.ai/inference/go/v1"

      # Optional catalog endpoint override (default: "{base-url}/models")
      # catalog-url: "https://opencode.ai/inference/go/v1/models"

      # Client-facing model ID prefix configuration
      model-prefix:
        enabled: true           # true -> "opencode-go/<model>", false -> bare "<model>" (default: true)
        value: "opencode-go"    # prefix name (default: "opencode-go")

      # OpenCode Go API keys (at least one required). Supports ${ENV_VAR} expansion.
      api-keys:
        - value: "sk-opencode-key-1"
        - value: "sk-opencode-key-2"
        - value: "${OPENCODE_GO_API_KEY}"

      # Catalog discovery settings
      catalog:
        refresh-interval: "15m"          # discovery refresh cadence, min "1m" (default: "15m")
        stale-while-unavailable: true    # retain last good catalog snapshot on refresh failure (default: true)

      # Protocol enable/disable switches (all default to true)
      protocols:
        chat-completions: true   # enables models routed to /v1/chat/completions
        messages: true           # enables models routed to /v1/messages
        responses: true          # enables models routed to /v1/responses
        gemini: true             # enables models routed to Gemini GenerateContent

      # Explicit route overrides per model (takes priority over built-in prefix routing)
      route-overrides:
        "custom-model":
          protocol: "messages"           # "chat-completions" | "messages" | "responses"
          endpoint: "/v1/messages"       # must start with /

      # Execution settings
      request-timeout: "5m"              # upstream request timeout (default: "5m")
      max-response-bytes: 67108864       # max non-streaming response body size in bytes (default: 64 MiB)
      allow-http: false                  # allow http:// scheme for local mock/testing (default: false)
```

### Configuration Options

| Option | Type | Default | Description |
|---|---|---|---|
| `api-keys` | `[]object` | *(Required)* | List of API keys (`- value: "..."`). Supports `${ENV_VAR}` expansion. Duplicates and empty values are rejected. |
| `base-url` | `string` | `https://opencode.ai/inference/go/v1` | Upstream base URL. Must be valid HTTPS (or HTTP if `allow-http: true`) without query parameters, fragments, or userinfo. |
| `catalog-url` | `string` | `{base-url}/models` | Full URL for catalog discovery. Defaults to `{base-url}/models`. |
| `model-prefix.enabled` | `bool` | `true` | When `true`, client-facing model names use `<prefix>/<model>`. When `false`, uses bare model IDs. |
| `model-prefix.value` | `string` | `opencode-go` | Provider prefix string when prefixing is enabled. |
| `catalog.refresh-interval` | `duration` | `15m` | Interval between catalog polling refreshes (e.g. `15m`, `1h`). Minimum is `1m`. |
| `catalog.stale-while-unavailable` | `bool` | `true` | When `true`, serves the last valid catalog snapshot if an update fails. |
| `protocols.chat-completions` | `bool` | `true` | Protocol switch for Chat Completions endpoints. |
| `protocols.messages` | `bool` | `true` | Protocol switch for Messages endpoints. |
| `protocols.responses` | `bool` | `true` | Protocol switch for Responses endpoints. |
| `protocols.gemini` | `bool` | `true` | Protocol switch for native Gemini GenerateContent endpoints. |
| `route-overrides` | `map` | `{}` | Map of model ID to `{ protocol: "...", endpoint: "..." }` overriding built-in family routing. Valid protocols: `chat-completions`, `messages`, `responses`, `gemini`. |
| `request-timeout` | `duration` | `5m` | Upstream HTTP request timeout. Must be positive. |
| `max-response-bytes` | `int64` | `67108864` (64 MiB) | Maximum non-streaming response body size in bytes. |
| `allow-http` | `bool` | `false` | When `true`, permits `http://` scheme in `base-url` / `catalog-url` for local testing. |

## Testing

```powershell
# Run all tests
go test ./...

# Run tests with coverage
go test ./... -cover

# Run linter / vetting
go vet ./...
```

## Current model coverage and client usage

The live Go catalog read on 2026-10-14 returned 40 IDs. The offline catalog fixture covers all 40: 26 Chat Completions, 7 Messages, 6 Responses, and 1 Gemini route. Model presence and protocol routing are distinct from successful inference or a particular key's subscription permissions.

Official Go documentation and models.dev supply protocol metadata for 36 of these IDs. The existing family routes cover `deepseek-flash`, `glm-5.1`, and `minimax-m2.5`. `omen-alpha` has an explicit Chat compatibility route with a diagnostic: its minimal upstream probe returned HTTP 200 but no SSE event before the 25-second timeout. Its full streaming acceptance is pending.

Two Qwen IDs are exact exceptions to the inherited family table: `qwen3.6-plus` and `qwen3.7-max` use Chat Completions. `qwen3.7-plus`, `qwen3.8-max`, and `qwen3.8-flash` use Messages. Claude, Space Bunny, Step, and Cat Jam routes are also represented.

After an authorized installation, an OpenAI-compatible client uses the existing CPA base `http://127.0.0.1:8317/v1` and model `opencode-go/gemini-4-argon-contributor`. A native Gemini client uses CPA's frontend action at `/v1beta/models/opencode-go/gemini-4-argon-contributor:streamGenerateContent?alt=sse`; the plugin requests the upstream `/inference/go/v1` action. The client authenticates with its **CPA access key**; the OpenCode Go subscription key belongs in the plugin configuration. Set a stable, unique `x-opencode-session` on every turn.

The example environment key must be available to the CPA process, because expansion happens inside the loaded plugin. Never overwrite the whole existing CPA configuration with the example fragment. Keep the old OpenCode plugin disabled and use the prefixed model IDs to avoid native-provider model collisions.

Rebuild this plugin only on Windows with `./build.ps1 -GoExe <go.exe> -CCompiler <gcc.exe>`. Offline DLL verification is `python tools/abi_smoke.py <absolute DLL path>`. Neither command installs or enables the plugin.

Sources: [official Go documentation](https://dev.opencode.ai/v2/docs/console/go/), [documentation source](https://github.com/anomalyco/opencode/blob/dev/services/www/src/docs/content/console/go.mdx), [models.dev data](https://models.dev/api.json), and [CPA v8.0.28](https://github.com/router-for-me/CLIProxyAPI/tree/v8.0.28).
