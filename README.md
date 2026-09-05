# workbuddy-cliproxy

把**腾讯 CodeBuddy**（`copilot.tencent.com`）封装成 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)(CPA)插件,任何支持 OpenAI / Anthropic 协议的客户端(Claude Code、Cursor、Cline、SDK……)都能直接调用 CodeBuddy 背后的模型。

对 [Sliverkiss/cpa-plugin](https://github.com/Sliverkiss/cpa-plugin) 公开 `workbuddy.so` 的 clean-room 逆向重写,补齐了源码与 x86_64 支持;workbuddy 的原始设计归属 Sliverkiss。

## 工作原理

在 CPA 里注册为 `workbuddy` provider:负责 CodeBuddy 扫码登录、token 刷新,并把请求转发到 `copilot.tencent.com/v2/chat/completions`。登录后凭据存为 `workbuddy.json`。

## 出站代理

插件的 CodeBuddy 上游出网(对话 / token 刷新 / 登录 / 积分探测)跟随 CPA 的代理语义,不再各自裸连:

- **系统代理**:读宿主带 `Host` 的请求(auth.parse / login.* / refresh)上报的全局 `proxy-url`(CPA 配置 `proxy-url`)。executor 对话请求不带 `Host`,因此插件把它缓存在进程内作回退——宿主每次扫描/刷新凭据都会带来最新的系统代理。
- **认证文件单独配置代理**:在凭据文件顶层加 `"proxy_url": "..."`(CPA 面板改 proxy_url 时也会写入同一个字段),该账号的对话 / 刷新 / 积分探测就单独走这个代理,覆盖系统代理。
- **支持协议**:`http` / `https` / `socks5` / `socks5h`,用户名密码带在 URL 里(`socks5://user:pass@host:1080`);`direct` 或 `none` 显式绕过全局与环境代理。解析与传输复用 CPA 自带的 `sdk/proxyutil`,与宿主其余出网路径同一套语义。
- **优先级**:请求元数据 `proxy_url` → 凭据文件顶层 `proxy_url` → 系统代理 → 直连/继承环境代理。任一环节写 `direct`/`none` 即显式直连。
- **cookie 语义不变**:走代理只是换了连接层,登录用的独立 cookie jar、共享客户端的 jar 与超时都保持原样。
- **失败即放行**:代理配置非法/不支持时回退默认直连并记日志,不会让一条坏代理把请求整条打断。

```json
// auths/workbuddy.json 顶层加一行即可让该账号走独立代理:
{
  "auth": { "...": "…" },
  "account": { "...": "…" },
  "type": "workbuddy",
  "proxy_url": "socks5://127.0.0.1:1080"
}
```

## 模型

`hy4-preview` · `hy3` · `glm-5.3-flash` · `deepseek-v4-flash`

具体可用性以 CodeBuddy 账号权限为准。

## 安装

**前置**:运行中的 CLIProxyAPI v7.2.x(带 CGO / 插件支持)、CodeBuddy 账号、Go 1.26+ 与 gcc;编译架构需与 CPA 实例一致(amd64 / arm64)。

```bash
git clone https://github.com/lovingfish/workbuddy-cliproxy.git
cd workbuddy-cliproxy
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -buildmode=c-shared -o workbuddy.so .
```

产物:`.so`(Linux)/ `.dylib`(macOS)/ `.dll`(Windows)。放到 CPA 的 `plugins/` 目录,在 `config.yaml` 启用:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    workbuddy: { enabled: true, priority: 100 }
```

重启 CPA,日志出现 `plugin loaded ... plugin_id=workbuddy` 即成功,`GET /v1/models` 也能看到上面的模型。然后到 CPA 面板添加 workbuddy 凭据,扫码登录 CodeBuddy。

## 使用

CPA 默认端口 `8317`,API key 见 `config.yaml` 的 `api-keys`。

| 协议 | Base URL |
|------|----------|
| OpenAI | `http://<host>:8317/v1` |
| Anthropic | `http://<host>:8317`(不带 `/v1`,走 `x-api-key`) |

```bash
# Claude Code
export ANTHROPIC_BASE_URL=http://localhost:8317
export ANTHROPIC_API_KEY=<api-key>
export ANTHROPIC_MODEL=hy3-preview-agent
claude
```

```bash
# curl / OpenAI
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <api-key>" -H "Content-Type: application/json" \
  -d '{"model":"hy3-preview-agent","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

流式 / 非流式都支持;非流式请求会被内部转成流式再聚合(CodeBuddy 上游 `code 11101` 拒绝非流式)。

## Claude Code 兼容性

腾讯 CodeBuddy 的内容审核把 Claude Code 的两句固定 system 模板逐字加进了黑名单,命中即回"敏感内容"拒答:

- `You are Claude Code, Anthropic's official CLI for Claude.`(身份句)
- `Main branch (you will usually use this for PRs)`(git 注入句)

任何一字改动都绕过(精确匹配,非语义审核)。workbuddy 转发前会自动把这两句做最小改写(`CLI`→`CLI tool`、`Main branch`→`Default branch`),语义不变,Claude Code 照常工作。

属于 cat-and-mouse:腾讯哪天多加模板句,得跟着改 `sanitizeBlockedTemplates`。

## 思考模式

hy3 系列(`hy3` / `hy3-preview` / `hy3-preview-agent`)自动开最大思考:workbuddy 转发前强制 `reasoning_effort=high`,覆盖客户端任何设置。CodeBuddy 只对 `high` 真正开深度思考(`medium` / `max` / `xhigh` 等档位它直接忽略),所以这已是 hy3 能用的最高档。思考内容走 SSE 的 `delta.reasoning_content`,客户端要支持渲染思考块才看得到。

## 流式

真流式(async):转发上游时边读边通过 `host.stream.emit` 把每个 chunk 实时推给 CPA,客户端逐字收到(不是等收齐了一股脑)。hy3 几千字的思考过程也是实时流出的,不是憋半天再刷出来。

## License

MIT。
