# 开发说明

## 为什么是插件而不是反代独立服务

`kuku2api` 是一个独立 Node 服务：自己监听端口、校验自己的 API Key、用 SQLite 存账号/会话/用量、自带控制台。如果只是 nginx 把 CPA 的请求转到它，会留下两套密钥、两套凭据、两套账号池和两套统计口径——不是"统一密钥聚合"。

CPA 的插件模型里，宿主负责对外入口、API Key、账号选择、用量队列；插件负责登录、凭据、模型发现、把请求打到上游。所以这里只移植后者。

## 文件映射（kuku2api → 本插件）

| kuku2api（Node） | 本插件（Go） | 说明 |
|---|---|---|
| `src/upstream.mjs` | `upstream.go` | 上游 HTTP/SSE 客户端、`frameToDelta`、`parseSseBlock` |
| `src/passport.mjs` `exchangeStoken` / `loginAuthBody` | `upstream.go` | PTOKEN → STOKEN 换取（含引擎签名 md5） |
| `src/passport.mjs`（QR/SMS 各 leg） | `passport.go` | passport 客户端：设备 Cookie、getapi token、getqrcode/qrcode/unicast/bdusslogin、senddpass/?login、图形验证码 |
| `src/logins.mjs` | `loginsession.go` | 登录会话注册表：TTL、轮询去重、共享的 finish（换 STOKEN → 校验 → 落盘） |
| `src/accounts.mjs` / `credential-refresh.mjs` | `auth.go` | 凭据形状、身份、解析、续期 |
| `src/gateway.mjs`（纯文本路径） | `executor.go` | `messagesToText` 等价逻辑、think 档位、会话/回复 id 分配 |
| `src/agent.mjs` / `native-agent.mjs` / `tool-schema.mjs` | `native.go` | 原生 MCP 工具桥接 |
| `src/server.mjs` 的 `/v1/models` | `models.go` | 目录拉取 + 前缀/过滤 |
| `src/scheduler.mjs` + `gateway.mjs` 的 `claimFreePoints` | `signin.go` | 每日签到、额度页 |

丢弃：自带 HTTP 服务器、自有 API Key、SQLite 控制台、Responses API、ledger——全部由 CPA 顶替。

## 关键设计点

**登录不走 OAuth URL 流。** 宿主的 `auth.login.start/poll` 只认"URL + 轮询"，装不下"扫码/短信"。所以 `auth.login.start` 返回的是**本插件管理页**的 URL；页面用管理密钥调三组受鉴权保护的路由：

- `POST …/kuku/login/qr` → 插件向 passport 申请二维码，**把 PNG 以 data URL 内联在 JSON 里**返回（`<img>` 发不了自定义头，而资源页是无鉴权的，所以图片不能单独开路由）。
- `GET …/kuku/login/qr/{id}` → 轮询 unicast 通道；confirmed 后调 `bdusslogin` 把真实 BDUSS/PTOKEN 落进该次尝试的 jar，再走共享 finish。
- `POST …/kuku/login/sms`、`…/sms/resend`、`…/sms/verify` → 短信链路；图形验证码同样以 data URL 内联（passport 把图绑在插件进程的设备 Cookie 上，浏览器直接加载会永远不匹配）。
- `POST …/kuku/login` → 兜底的手工 Cookie 方式。

**凭据不出进程**：jar 只活在会话里，任何路由都不回显 BDUSS/PTOKEN/STOKEN，成功只返回 uid。

**主题**：资源页 iframe 里 CPA 面板会注入 `data-cpamp-plugin-host` / `data-theme` 和自己的 CSS 变量。页面 CSS 直接消费 `--bg-primary`/`--text-primary`/`--primary-color` 等宿主 token（只在面板外查看时才回退到 `prefers-color-scheme`），所以明暗跟随面板设置而不是操作系统。

**执行器契约。** `executor.execute` / `executor.execute_stream` 收到的是 OpenAI 请求体（`Payload`，回退 `OriginalRequest`），返回完整响应或流式 chunks。一次 executor 调用 = 一次上游轮次。

**会话。** 当前每个请求新建一个上游会话（`allocateIds` ×2），把完整历史作为 `CONVERSATION_JSON` 文本发给上游。
`ponytail: 每请求新会话，靠重发历史保证正确性；上游侧缓存/会话复用丢失。需要时按 kuku2api 的 deriveSessionKey 加会话映射。`

## Phase 2：原生 MCP 工具桥接（Agent 能力）—— 已实现（`native.go`）

库库AI 上游**不吃** OpenAI `tools` 字段；它有一条原生 MCP connector 通道。`kuku2api` 的 `native-agent.mjs` 在**单次 HTTP 请求内**完成整套桥接，正好一对一映射到一次 executor 调用：

1. 开两条控制流：`/wenchain/genflowpro/sse/msgexchange`（base）与 `/api/genflowprosse/call`（sandbox）。
2. 分配 id、`sendmsg` 时带 `connector_list: [{name, description, type: "custom"}]`。
3. sandbox task 里收到 `local_connector` 的 `tools/list` → 用客户端工具定义回 `callback`。
4. 收到 `tools/call` → 校验参数 → 把工具调用作为 `chat.completion` 的 `tool_calls` 交给调用方，结束本轮（`closeTurn`）。
5. 调用方下一轮把真实工具结果带回，重发完整历史，新建上游会话。

**不需要移植** kuku2api 实验性的跨请求续接（`NATIVE_AGENT_CONTINUATION`，默认关闭）：它依赖保活长连接，插件里也活不下来。

风险点：控制流的帧去重、`task_id`/`request_id` 映射、账号在工作期间的失效、以及"上游可能已执行、不能盲目重试"的语义。

## Phase 3：每日签到 —— 已实现（`signin.go`）

`kuku2api` 的契约：`GET /api/genflowpro/freepoint/homenew` 列出任务（`activities[].tabs[].tasks[]`），`POST /api/genflowpro/freepoint/rewardClaim`（form: `activity_key`/`period_id`/`task_key`）领取，`POST …/taskComplete` 上报。

- 凭据枚举走 `host.auth.list` + `host.auth.get`（`MethodHostAuthList`/`MethodHostAuthGet`），按 `provider`/`type == kuku` 过滤。
- `taskComplete` 只上报进度，**不发钱**；`rewardClaim` 才是发放。CHAT 任务要先真跑一轮对话才可领（会花积分），所以默认关闭。
- 账号串行、间隔 15 秒；每天在设定时间后跑一次，状态落在 `data/kuku-signin.state`（CPA 容器的 `data/` 已挂载）。
- 开关/状态/立即执行：管理页「库库额度」，或 `…/kuku/signin/{status,toggle,run}`。

## 构建与测试

```bash
docker run --rm --platform linux/amd64 -v "$PWD":/src -w /src -v kuku-gomod:/go/pkg/mod \
  golang:1.26-bookworm bash -c 'export PATH=$PATH:/usr/local/go/bin && go vet ./... && go test ./... && ./build.sh'
```

测试覆盖纯逻辑：凭据解析、身份、模型前缀/发布策略、think 档位、SSE 帧解析与 `frameToDelta`、prompt 组装、签名体形状、鉴权失败判定；工具桥接侧覆盖工具校验/去重、历史往返（含 tool 结果配对）、connector 指纹、prompt 渲染、tool_calls 输出、签到任务扁平化与设备 id 稳定性。真实上游（登录、模型目录、对话、工具闭环、签到）**必须用真账号单独验收**，会消耗积分。

## 已知限制

- 仅文本：图片/文件附件未实现（上游有完整的上传+关联+风控协议，见 `upstream.mjs` 的 `uploadImage`/`attachImages`）。
- 仅 chat-completions：不做 Responses API。
- 每请求新建上游会话（见上）。
- 工具桥接**不做跨请求续接**：模型一旦发出工具调用，本轮就结束（`closeTurn`），由调用方在下一轮带回结果。
- 签到默认关闭；CHAT 任务要花一轮对话才能领，积分净收益可能为负，按账号实际额度决定是否开。
