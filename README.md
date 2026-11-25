# OpenCode Go Omni

[![构建状态](https://github.com/way326/opencode-go-omni/actions/workflows/build.yml/badge.svg)](https://github.com/way326/opencode-go-omni/actions/workflows/build.yml)
[![最新版本](https://img.shields.io/github/v/release/way326/opencode-go-omni)](https://github.com/way326/opencode-go-omni/releases/latest)

OpenCode Go Omni 是面向 [CLIProxyAPI（CPA）](https://github.com/router-for-me/CLIProxyAPI) 的原生插件，为 OpenCode Go 订阅提供 `opencode-go` 模型提供方，并通过同一凭据池支持四种 API 协议：

- OpenAI Chat Completions
- OpenAI Responses
- Anthropic Messages
- Google Gemini GenerateContent

**当前稳定版本：[v0.2.3](https://github.com/way326/opencode-go-omni/releases/tag/v0.2.3)。** 请按操作系统和架构下载对应的 Release 压缩包。

> 不要同时启用本插件和原版 `opencode-go-cliproxyapi` 插件。两者注册相同的 `opencode-go` 提供方。

## 项目特点

- **使用 CPA 插件宿主。** API 监听、客户端认证、请求调度、管理界面和插件生命周期均由 CPA 管理。本插件不单独监听端口，也不修改或重建 CPA 可执行文件。
- **一个提供方支持四种协议。** 客户端使用 CPA 标准 API 端点；插件根据模型选择已配置的上游协议。
- **按模型目录路由。** 读取模型目录及协议元数据，并支持针对特定模型配置显式路由。目录中出现某个模型只表示它具备路由资格，不保证上游账号当前一定可用。
- **多凭据池。** 每个 OpenCode Go Key 与其 CPA 凭据身份精确绑定；凭据调度和健康状态由 CPA 管理。
- **配额查询。** 实现 CPA 原生配额提供方，并提供独立的 OpenCode Go Quota 页面，可按凭据手动刷新滚动、每周和每月用量。
- **协议转换与流式处理。** 提供跨协议转换、会话亲和及流式完成状态保护。
- **Codex Multi-Agent V2。** 支持规范化处理符合要求的明文 Agent 消息；加密或未知格式会被拒绝，不会尝试解密或静默丢弃。

## 环境要求

- 支持原生插件的 CLIProxyAPI v8。v0.2.3 曾在隔离的官方 CLIProxyAPI v8.1.2 环境中进行冒烟验收；这不代表已覆盖所有 CPA 构建版本。
- OpenCode Go 订阅 API Key。
- 本插件不提供 OAuth 登录流程。

## 安装

1. 从 [v0.2.3 Release](https://github.com/way326/opencode-go-omni/releases/tag/v0.2.3) 下载与系统和 CPU 架构匹配的压缩包。
2. 解压共享库，并放入 CPA 配置的插件目录。常见文件名为 Windows 的 `opencode-go-omni.dll`、Linux 的 `opencode-go-omni.so`、macOS 的 `opencode-go-omni.dylib`。
3. 将下方配置合并到 CPA 的 `config.yaml`，并确认插件已启用。
4. 保持原版 `opencode-go-cliproxyapi` 插件禁用。请按当前 CPA 安装方式加载或刷新插件。
5. 客户端连接 CPA 的常规 API 地址并使用 CPA 客户端 API Key。启用模型前缀时，模型 ID 形如 `opencode-go/<模型名>`。

仅构建本项目不会自动安装插件，也不会启用或修改正在运行的 CPA。

## 配置

以下示例与 [`config.example.yaml`](config.example.yaml) 保持一致。请只合并插件配置块，不要覆盖 CPA 配置中的其他内容。

```yaml
plugins:
  enabled: true
  configs:
    opencode-go-cliproxyapi:
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

在 CPA 进程可访问的环境中设置 `OPENCODE_GO_API_KEY`，再按部署方式启动或重新加载 CPA。建议使用环境变量引用，避免把真实 Key 写进配置文件。

| 配置项 | 说明 |
| --- | --- |
| `base-url` | OpenCode Go API 根地址；默认值为 `https://opencode.ai/inference/go/v1`。 |
| `catalog-url` | 可选的模型目录地址；默认使用 `{base-url}/models`。 |
| `api-keys` | 一个或多个上游订阅 Key，由 CPA 管理凭据调度。 |
| `model-prefix` | 控制客户端看到的模型 ID 前缀，常用值为 `opencode-go/`。 |
| `protocols` | 分别启用或关闭 Chat Completions、Messages、Responses 和 Gemini 协议。 |
| `catalog.refresh-interval` | 模型目录刷新间隔。 |
| `catalog.stale-while-unavailable` | 上游目录暂时不可用时，是否保留最近一次有效目录。 |
| `route-overrides` | 按模型指定协议和端点的可选覆盖配置；参见 `config.example.yaml`。 |
| `request-timeout` | 上游请求的时间预算，默认 5 分钟。流式处理使用固定阶段预算，不是空闲超时。 |
| `max-response-bytes` | 非流式响应的最大大小，默认 64 MiB。 |
| `allow-http` | 是否允许明文 HTTP；仅供本地测试，默认 `false`。 |

实际访问上游时应使用 HTTPS。`allow-http` 不能替代 TLS，除本地测试外应保持关闭。

## 模型发现与路由

- 插件读取模型目录中的显示信息和协议元数据，并兼容旧版目录格式。
- 路由优先级为：用户显式覆盖、已知故障规则、精确兼容规则、支持的目录协议元数据，最后才是旧目录的模型家族回退规则。
- 插件只使用上述四种支持的协议。模型目录元数据不能指定任意 URL；不支持或互相冲突的协议不会被猜测路由。
- `model-prefix.enabled: true` 时，客户端模型 ID 会带 `opencode-go/` 前缀；如客户端集成要求无前缀，可关闭此选项。
- 模型出现在目录中不等于上游账号必然接受请求；实际可用性取决于上游服务和账号状态。

## 凭据与安全

- 客户端使用 CPA 客户端 API Key 访问代理。OpenCode Go 订阅 Key 应配置在插件的 `api-keys` 中，不应发给客户端。
- 新建的插件凭据文件只包含提供方类型、凭据 ID 和标签，不写入上游 `api_key`。对于规范凭据 ID，请求执行和配额查询都会从当前插件配置中解析与所选 CPA Auth ID 对应的 Key；身份不匹配或未知时会拒绝请求。
- 导入的旧凭据文件会先校验再使用。迁移前请备份并审查旧文件，不要随意删除或编辑 CPA 管理的凭据文件。
- 规范凭据 ID 使用稳定的 SHA-256 指纹：相同 Key 在不同安装间可被关联，已知候选 Key 也可离线比对。因此它不是随机或不可识别的标识符。
- 如果 Key 曾经泄露，应在上游服务处轮换或撤销。改用环境变量不会使已泄露的副本失效。

## 配额

CPA 原生配额集成会使用所选凭据请求 `GET {base-url}/usage`，并保留上游提供的重置时间。独立的 **OpenCode Go Quota** 页面首次加载只列出已配置的凭据，不会自动请求上游；只有手动刷新时才查询对应凭据。两种配额路径都会校验滚动、每周和每月窗口；数据缺失或格式错误时返回错误，不会误报为零用量。当前不支持重置配额。

## 会话与协议行为

- 建议客户端为每个对话发送稳定且唯一的 `x-opencode-session` 请求头；它的优先级高于自动推断的会话标识。
- 将 Responses 或 Messages 转换为其他协议时，只有收到有效完成信号才会生成成功终态；原生协议透传与转换校验分别处理。
- 对可识别的 Codex 客户端，插件会等到 Responses 转换最终校验通过后再发送成功终态。该行为依赖 CPA 的 Responses 帧处理器，升级 CPA 核心后应重新验证。
- 插件关闭时会停止接收新任务并等待进行中的流和宿主回调结束。无法中断的宿主回调可能延长关闭时间，直到回调返回。

## 构建与验证

`go.mod` 指定 Go 1.26.11。构建共享库需要启用 CGO 并安装兼容的 C 编译器；运行时除 CPA 插件宿主外无需额外依赖。

本地开发请使用 `debug` build tag：

```powershell
go test -tags debug ./...
go vet ./...
go build -tags debug -buildmode=c-shared -o plugins/windows/amd64/opencode-go-omni.dll .
python tools/abi_smoke.py plugins/windows/amd64/opencode-go-omni.dll
```

离线 ABI 冒烟测试使用内存中的假宿主，不发起网络请求，也不监听端口。`tests/` 下的测试使用 `live` build tag，需要单独配置 CPA，默认不会运行。

Windows Release 包装可参考 `build.ps1`；跨平台 Release 压缩包由 GitHub Actions 工作流构建和发布。

## 参与开发

开发环境和 Pull Request 约定见 [CONTRIBUTING.md](CONTRIBUTING.md)。面向用户的变更请同步记录到 [RELEASE_NOTES.md](RELEASE_NOTES.md)。

## 兼容性与许可证

插件 ABI 1 / RPC schema 6。本版本使用的 CPA 插件 SDK 版本固定在 [`go.mod`](go.mod) 中。v0.2.3 曾在隔离环境中通过官方 CLIProxyAPI v8.1.2 冒烟测试；这不替代在你的实际部署环境中进行验证。

本项目采用 MIT 许可证，详见 [LICENSE](LICENSE)。项目由 [massiveits/opencode-go-cliproxyapi](https://github.com/massiveits/opencode-go-cliproxyapi) 派生并持续维护，基于上游提交 `a4369b9b1cf18a4111a0167e3b50e5d69054fe45`（插件版本 0.1.10），保留上游版权声明和许可证。
