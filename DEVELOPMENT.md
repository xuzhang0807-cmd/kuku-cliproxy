# 开发说明

## 为什么是插件而不是反代独立服务

`kuku2api` 是一个独立 Node 服务：自己监听端口、校验自己的 API Key、用 SQLite 存账号/会话/用量、自带控制台。如果只是 nginx 把 CPA 的请求转到它，会留下两套密钥、两套凭据、两套账号池和两套统计口径——不是"统一密钥聚合"。

CPA 的插件模型里，宿主负责对外入口、API Key、账号选择、用量队列；插件负责登录、凭据、模型发现、把请求打到上游。所以这里只移植后者。

## 文件映射（kuku2api → 本插件）

| kuku2api（Node） | 本插件（Go） | 说明 |
|---|---|---|
| `src/upstream.mjs` | `upstream.go` | 上游 HTTP/SSE 客户端、`frameToDelta`、`parseSseBlock` |
| `src/passport.mjs` `exchangeStoken` / `loginAuthBody` | `upstream.go` | PTOKEN → STOKEN 换取（含引擎签名 md5） |
| `src/accounts.mjs` / `credential-refresh.mjs` | `auth.go` | 凭据形状、身份、解析、续期 |
| `src/gateway.mjs`（纯文本路径） | `executor.go` | `messagesToText` 等价逻辑、think 档位、会话/回复 id 分配 |
| `src/server.mjs` 的 `/v1/models` | `models.go` | 目录拉取 + 前缀/过滤 |
| `src/scheduler.mjs` | `signin.go`（stub） | 每日签到，phase 3 |
| `src/agent.mjs` / `native-agent.mjs` / `tool-schema.mjs` | 待办（phase 2） | 原生 MCP 工具桥接 |

丢弃：自带 HTTP 服务器、自有 API Key、SQLite 控制台、Responses API、ledger——全部由 CPA 顶替。

## 关键设计点

**登录不走 OAuth URL 流。** 宿主的 `auth.login.start/poll` 只认"URL + 轮询"，装不下"输手机号 + 验证码"。所以 `auth.login.start` 返回的是**本插件管理页**的 URL；页面收集 BDUSS + PTOKEN，POST 到受管理鉴权保护的 `…/kuku/login`；插件换取 STOKEN、校验、用 `host.auth.save` 落盘，再让 `auth.login.poll` 报成功。

**执行器契约。** `executor.execute` / `executor.execute_stream` 收到的是 OpenAI 请求体（`Payload`，回退 `OriginalRequest`），返回完整响应或流式 chunks。一次 executor 调用 = 一次上游轮次。

**会话。** 当前每个请求新建一个上游会话（`allocateIds` ×2），把完整历史作为 `CONVERSATION_JSON` 文本发给上游。
`ponytail: 每请求新会话，靠重发历史保证正确性；上游侧缓存/会话复用丢失。需要时按 kuku2api 的 deriveSessionKey 加会话映射。`

## Phase 2：原生 MCP 工具桥接（Agent 能力）

库库AI 上游**不吃** OpenAI `tools` 字段；它有一条原生 MCP connector 通道。`kuku2api` 的 `native-agent.mjs` 在**单次 HTTP 请求内**完成整套桥接，正好一对一映射到一次 executor 调用：

1. 开两条控制流：`/wenchain/genflowpro/sse/msgexchange`（base）与 `/api/genflowprosse/call`（sandbox）。
2. 分配 id、`sendmsg` 时带 `connector_list: [{name, description, type: "custom"}]`。
3. sandbox task 里收到 `local_connector` 的 `tools/list` → 用客户端工具定义回 `callback`。
4. 收到 `tools/call` → 校验参数 → 把工具调用作为 `chat.completion` 的 `tool_calls` 交给调用方，结束本轮（`closeTurn`）。
5. 调用方下一轮把真实工具结果带回，重发完整历史，新建上游会话。

**不需要移植** kuku2api 实验性的跨请求续接（`NATIVE_AGENT_CONTINUATION`，默认关闭）：它依赖保活长连接，插件里也活不下来。

风险点：控制流的帧去重、`task_id`/`request_id` 映射、账号在工作期间的失效、以及"上游可能已执行、不能盲目重试"的语义。

## Phase 3：每日签到

`kuku2api` 的契约：`GET /api/genflowpro/freepoint/homenew` 列出任务（`activities[].tabs[].tasks[]`），`POST /api/genflowpro/freepoint/rewardClaim`（form: `activity_key`/`period_id`/`task_key`）领取，`POST …/taskComplete` 上报。

卡点：需要枚举宿主里所有 kuku 凭据。`host.auth.list` 这个 host 调用存在，但 SDK v7.2.157 没有给出它的请求/响应类型，所以枚举步骤刻意留到确认后再做，不靠猜。

## 构建与测试

```bash
docker run --rm --platform linux/amd64 -v "$PWD":/src -w /src -v kuku-gomod:/go/pkg/mod \
  golang:1.26-bookworm bash -c 'export PATH=$PATH:/usr/local/go/bin && go vet ./... && go test ./... && ./build.sh'
```

测试覆盖纯逻辑：凭据解析、身份、模型前缀/发布策略、think 档位、SSE 帧解析与 `frameToDelta`、prompt 组装、签名体形状、鉴权失败判定。真实上游（登录、模型目录、对话）**必须用真账号单独验收**，会消耗积分。

## 已知限制

- 仅文本：图片/文件附件未实现（上游有完整的上传+关联+风控协议，见 `upstream.mjs` 的 `uploadImage`/`attachImages`）。
- 仅 chat-completions：不做 Responses API。
- 每请求新建上游会话（见上）。
- 自动签到未实现；开启只会记日志。
