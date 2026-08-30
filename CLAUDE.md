# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目定位

把腾讯 CodeBuddy（`copilot.tencent.com`）封装成 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（下称 CPA）的 **原生动态插件**，注册为 `workbuddy` provider。插件以 `-buildmode=c-shared` 编译成 `.so`/`.dylib`/`.dll`，由 CPA 宿主 dlopen 加载，通过 C ABI + JSON-RPC 双向通信。

对 Sliverkiss 公开的 `workbuddy.so` 二进制做的 clean-room 逆向重写（符号表 / 字符串常量 / RPC 形状）。

根目录 `main.go` **只有 C ABI 那一层**（cgo preamble + 4 个 `//export` + 宿主传输），其余全在 `internal/` 下按职责分包，都是纯 Go（不含 cgo，能单独编译和测试）：

```
main.go                  C ABI：cgo preamble、//export、hostCall、writeResponse
internal/hostrpc/        插件 → 宿主：传输注入、Call、Log、Stream{Emit,EmitError,Close}、AuthSave
internal/wire/           跨边界的 JSON 形状：Envelope、UpstreamError、各种信封构造
internal/codebuddy/      上游本身：endpoints、StoredAuth、HTTP client、请求头、DoJSON、APIEnvelope
internal/quota/          积分状态机：probe、Gate、Debit/TrackSpend、note、Classify
internal/auth/           auth provider：ParseAuth、StartLogin、PollLogin、RefreshAuth、ToAuthData
internal/executor/       转发：Execute、ExecuteStream、pump/collect、SSE 聚合、请求改写
internal/plugin/         RPC 分发 + 能力声明 + 模型清单
```

依赖单向：`main → plugin → {auth, executor} → quota → codebuddy`，`wire` / `hostrpc` 是叶子。`codebuddy` 和 `hostrpc` 不 import 本仓任何包。

## 常用命令

```bash
# 测试（纯 Go 逻辑，不需要跑起 CPA）
go test ./...
go test -run TestQuotaGate ./...          # 单个测试
go test -race -run TestDebitCredits ./... # 并发相关的必须带 -race

go vet ./...
go build ./internal/...                    # 只编业务逻辑，不需要 cgo/gcc

# 本地开发闭环（macOS + homebrew 装的 CPA）：编译 → 装到 plugins/ → 改配置 → 重启服务
./install-homebrew.sh
# 可覆盖的环境变量：CLIPROXY_PLUGIN_DIR / CLIPROXY_CONF / PLUGIN_ARCH

# 手动交叉编译（部署到 Linux 版 CPA）
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared -o workbuddy.so .
```

**编译硬约束**：`CGO_ENABLED=1` 必须开启（c-shared 依赖 cgo）；`GOARCH` 必须与运行 CPA 的实例一致（amd64 / arm64），架构不符宿主会加载失败。产物旁边的 `.h` 头文件无用，install 脚本会删掉。

**验证加载成功**：CPA 日志出现 `plugin loaded ... plugin_id=workbuddy`，且 `GET /v1/models` 能看到 `hy4-preview` / `hy3` / `glm-5.3-flash` / `deepseek-v4-flash`。

## 架构

### 1. C ABI 边界（`main.go`，全仓唯一的 cgo 文件）

宿主在 `cliproxy_plugin_init` 里递入 `cliproxy_host_api` 函数指针表，插件填回自己的 `cliproxy_plugin_api`。两个方向都是"方法名 + JSON 字节"：

- **宿主 → 插件**：`cliproxyPluginCall(method, request) → response`，全部收敛到 `plugin.HandleMethod` 的 switch。
- **插件 → 宿主**：`hostCall(method, request)` 走 init 时捕获的全局 `hostAPI` 指针。因为它摸 C 类型只能留在 `main`，所以 init 里用 `hostrpc.SetTransport(hostCall)` **注入**给 `internal/hostrpc`，其他包一律通过 `hostrpc.Log` / `StreamEmit` / `StreamClose` / `AuthSave` 调宿主，自己不碰 cgo。

内存所有权：返回给宿主的缓冲用 `C.CBytes` 分配、由 `cliproxyPluginFree` 释放；宿主返回的缓冲用 `hostAPI.free_buffer` 释放。`func main() {}` 是空的（c-shared 要求）。

⚠️ `//export` 符号必须留在 `package main`，且 C typedef 只能出现在**一个** cgo 文件里 —— 所以 `main.go` 保持单文件，别往根目录加第二个 `import "C"` 的文件。

### 2. RPC 方法分发（`internal/plugin/dispatch.go`）

新增能力 = 在这个 switch 里加 case + 在 `newRegistration()`（`register.go`）的 `Capabilities` 里声明。当前声明了 model provider / auth provider / executor 三种角色，executor 的输入输出格式都是 `chat-completions`，scope 为 `both`。

| 方法 | 处理函数 | 说明 |
|---|---|---|
| `plugin.register` / `reconfigure` | `newRegistration` | 能力声明 + 元数据 |
| `model.static` / `model.for_auth` | `staticModels` | 模型清单（含各自 context length） |
| `auth.parse` | `auth.ParseAuth` | 认不出就返回 `Handled:false`，让宿主试其他 provider |
| `auth.login.start` / `poll` | `auth.StartLogin` / `auth.PollLogin` | 扫码登录，宿主控制轮询节奏 |
| `auth.refresh` | `auth.RefreshAuth` | 刷 token，顺带重新探测额度 |
| `executor.execute` | `executor.Execute` | 非流式：内部转流式再聚合 |
| `executor.execute_stream` | `executor.ExecuteStream` | 流式：异步 pump |

### 3. 错误状态码回传链路（改动执行器时最容易踩的坑）

宿主的 `decodeEnvelopeResult` **只从 `envelope.Error.HTTPStatus` 恢复上游状态码**，这个状态码才是驱动"额度冷却 + 凭据轮换"（`MarkResult`）的唯一信号。所以：

```
上游失败 → quota.Classify() → *wire.UpstreamError（带 Status/Code/Retryable）
         → wire.ErrorEnvelopeFor() → wire.StatusEnvelope() → {"ok":false,"error":{"http_status":429,...}}
```

返回普通 `error` 会丢掉状态码，宿主只当成泛化插件故障，凭据不会被冷却、不会轮换。`quota.Classify` 还负责把 5xx/408 标成 `Retryable`（宿主会重试），把额度耗尽统一映射成 **429**。

### 4. 流式（`executor.ExecuteStream` + `pumpUpstreamStream`）

关键设计：**上游连接同步打开，chunk 异步推送**。

先同步 `Do()` 拿到 response 并检查状态码 —— 因为一旦这次 RPC 返回了 OK，宿主就把请求记成成功，之后再用 stream chunk 报错为时已晚（`MarkResult` 已经跑过，凭据永远不会被冷却）。状态码没问题后才把 body 交给后台 goroutine `pumpUpstreamStream`，边读边 `host.stream.emit`，客户端逐字收到。

`req.StreamID` 为空时退化为同步收集（`collectUpstreamStream` + `aggregateSSE`）。

`emit` 返回错误（客户端断连 → 宿主已关流）时立即 break，避免继续读一个死上游。

**SSE 帧问题（`clientNeedsSSEFrame`）**：CPA 的 chat-completions 直通路径会自己加 `data: ` 前缀，但所有跨协议响应翻译器（claude / gemini / codex）只吃已经带 `data: ` 的行。所以按 `Metadata["request_path"]` 判断：`/v1/chat/completions` 和 `/v1/completions` 不加前缀，其他入口（比如 Anthropic 协议）自己加。

### 5. 额度 / 积分追踪（`internal/quota/`）

CodeBuddy 是**预付积分包**而非速率限制，上游不给恢复时间戳（靠人工充值），所以本地自己维护冷却：

- `creditsByAuth`（`sync.Map`，key 由 `quota.Key` 算：优先 `AuthID`，回落 `uid:<UID>`）缓存每个凭据的 `creditsState`。
- **两层余额**：`probeCredits` 调计费 API（`/v2/billing/meter/get-user-resource`，`ProductCode=p_tcaca`）拿权威值；`debitCredits` 从响应 chunk 里报告的消耗做本地扣减，标记 `estimated`（面板上显示为 `~`）。下次探测时对账。
- **冷却只能由权威来源触发**：计费 API 读到 0，或上游明确报额度耗尽（`markCreditsExhausted`）。本地估算扣到 0 **不会**启动冷却 —— 否则估算漂移会把还有余额的凭据白白锁死。
- `storeCreditsBalance` 在余额仍为 0 时保留原始 `exhaustedAt`，避免反复探测无限延长冷却窗口。
- `probeCredits` 和 `debitCredits` **共用同一把 `creditsProbeLock(key)` 互斥锁** —— 探测的写和扣减的 read-modify-write 必须串行，否则并发请求从同一基线扣减会丢失扣减（见 `TestDebitCreditsIsAtomicUnderConcurrency`）。
- `quota.Gate` 是请求前置检查：已知耗尽直接短路返回 429（省一次上游往返）；探测失败**从不致命**，放行请求、以上游结论为准。
- chunk 里的 credit 是**运行总量**不是增量，聚合时取 max 而非求和。
- 余额摘要写进 `codebuddy.StoredAuth.Note`，经 `hostrpc.AuthSave`（`host.auth.save`）落盘 + upsert 内存记录，显示在管理面板凭据卡片上（中文，与 CodeBuddy 控制台一致）。宿主会把 metadata 合并回 auth 文件，所以 `Note` 必须能 marshal/parse 往返（见 `TestStoredAuthNoteRoundTrips`）。

关键常量（`credits.go`，包内不导出）：`cooldown` 30min、`balanceTTL` 10min、`probeTimeout` 10s、`exhaustedStatus` 429。

### 6. 登录流程（`internal/auth/`：`StartLogin` / `PollLogin`）

CodeBuddy 把浏览器登录和 `auth/state` 下发的 state **绑在 cookie 上**，所以每个登录流程用独立 cookie jar 的 client（`codebuddy.NewLoginClient`），存进 `loginStates`（TTL 5min）。

轮询顺序有讲究：先查 `auth/token`（应用层 code 11217 = 登录中，code 0 = 拿到 token），**再**查 `login/account` —— 后者在 openresty 网关后面，登录完成前一律 401，不能用来判断登录状态。

### 7. 请求改写（`internal/executor/rewrite.go`）

转发前对请求体做两类改写，任何执行路径都会经过：

- **`sanitizeBlockedTemplates`**：腾讯内容审核把 Claude Code 两句固定 system 模板逐字加进了黑名单（身份句 `You are Claude Code, Anthropic's official CLI for Claude.` 和 git 注入句 `Main branch (you will usually use this for PRs)`），命中即"敏感内容"拒答。是精确匹配而非语义审核，所以改一个词就绕过（`CLI`→`CLI tool`、`Main branch`→`Default branch`）。**这是 cat-and-mouse：腾讯加新模板句就得跟着改这个函数。**
- **`forceMaxThinking`**：`hy3` 前缀的模型强制 `reasoning_effort=high`，覆盖客户端设置。CodeBuddy 只对 `high` 真开深度思考，`medium`/`max`/`xhigh` 等档位它直接忽略。

改写同时处理纯字符串 content 和 OpenAI 多模态数组（`rewriteContentField`）。

另外 `forceStreamBody` 给非流式请求强塞 `stream:true` —— CodeBuddy 上游用 `code 11101` 拒绝非流式请求，只能内部转流式再用 `aggregateCompletion` 折叠成单个 `chat.completion`。

`cleanChunkJSON` 剥掉 delta 里空值字段（`null`/`""`/`[]`/`{}`），避免严格客户端被 `{"function_call":null,"tool_calls":[]}` 噎住；同时顺手取出 credit，省一次解析。

## 上游 API 约定

所有 CodeBuddy 接口都是 `{code, msg, data}` 信封（`codebuddy.APIEnvelope`），`code != 0` 即业务失败 —— `codebuddy.DoJSON` 统一处理并返回内层 `data` + HTTP 状态码。

请求头由 `codebuddy.CommonHeaders` / `BackendHeaders` 构造，注意 CodeBuddy 的 `X-No-*` 约定：字段为空时要显式发 `X-No-Authorization` / `X-No-User-Id` / `X-No-Enterprise-Id` / `X-No-Department-Info`，而不是省略头。

## 测试约定

测试跟着代码放在各自包内（同包才能碰未导出字段），覆盖 quota / credits / 错误分类 / SSE 聚合等纯逻辑，不打真实网络：

| 文件 | 覆盖 |
|---|---|
| `internal/quota/credits_test.go` | 计费响应解析、冷却窗口、余额存取、本地扣减（含并发原子性） |
| `internal/quota/classify_test.go` | 额度关键词识别、状态码保真、429 能带进信封 |
| `internal/quota/gate_test.go` | 前置检查放行 / 拦截 |
| `internal/quota/note_test.go` | 凭据卡片文案 |
| `internal/wire/envelope_test.go` | 普通 error 不伪造 HTTP 状态 |
| `internal/hostrpc/hostrpc_test.go` | 传输注入、无 stream id 不发调用、`AuthSave` 必须内联裸 JSON |
| `internal/codebuddy/auth_test.go` | `StoredAuth.Note` marshal/parse 往返 |
| `internal/auth/auth_test.go` | `ToAuthData` 的 metadata |
| `internal/executor/sse_test.go` | credit 提取、chunk 清理、SSE 聚合 |

写测试时：

- 用完 `creditsByAuth` 的 key 必须 `t.Cleanup(func(){ creditsByAuth.Delete(key) })` —— 全局 `sync.Map` 会跨测试污染。同理 `hostrpc.transport` 是包级变量，改了要用 `t.Cleanup` 还原（见 `stubTransport`）。
- `quota.Classify` 传 `sa=nil` 可以跳过后台重探测的 goroutine。
- 测试注释里普遍写明"为什么这个断言重要"（比如"没有这个字段 429 就到不了 MarkResult"），沿用这个风格。

## 约定

- 提交信息：`type(scope): 中文描述`，近期 scope 用 `dev-MMDD`（如 `feat(dev-0830): 新增额度探测与凭据冷却机制`）。
- 代码注释是英文，用户可见文本（凭据卡片 note）是中文。注释密度较高且偏重"为什么"而非"做什么"，改动时保持。
- 错误变量命名用 `errXxx`（`errProbe` / `errMarshal` / `errClose`）而非裸 `err`，除了最外层的即时判断。
- `workbuddy.json`（含 access/refresh token）和编译产物已在 `.gitignore` 里，切勿提交。
- 默认分支 `main`，当前开发在 `release` 分支。
