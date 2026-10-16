# kuku-cliproxy

把**百度库库AI（KukuAI / GenFlowPro）**接入 [CLIProxyAPI（CPA）](https://github.com/router-for-me/CLIProxyAPI) 的原生插件，作为一个 OpenAI 兼容 provider 使用，支持多账号。

插件协议实现移植自 [IKUNHEIHIE/kuku2api](https://github.com/IKUNHEIHIE/kuku2api)（0BSD）：那里是一个自带 HTTP 服务、API Key、SQLite 控制台的独立 Node 服务；这里只保留 CPA 没有的部分——登录、凭据、模型发现、上游执行，其余交给 CPA。

> 定位与 [workbuddy-cliproxy](https://github.com/zqcccc/workbuddy-cliproxy) 相同：登录一个 App 账号 → 反代成 OpenAI API → 多个账号组成号池，客户端只用 CPA 的统一密钥。

## 状态

| 能力 | 状态 |
|---|---|
| 账号登录（BDUSS + PTOKEN → STOKEN 换取并校验） | ✅ 已实现 |
| 凭据自动续期（`auth.refresh` 用 PTOKEN 重新换取 STOKEN） | ✅ 已实现 |
| 模型发现（`model.for_auth` 拉上游目录 + 前缀/过滤） | ✅ 已实现 |
| Chat 对话（非流式 + 流式 SSE） | ✅ 已实现 |
| 客户端工具调用（原生 MCP connector 桥接，即 Agent 能力） | ✅ 已实现 |
| 每日签到（自动领积分） | ✅ 已实现，默认关闭 |
| 额度面板（各账号剩余积分） | ✅ 已实现 |

**是 Agent，不是纯聊天。** 库库AI 上游不吃 OpenAI `tools`，但有一条原生 MCP connector 通道：插件把客户端工具注册成 connector，模型发起的调用以标准 `tool_calls` 交回调用方执行，效果同 workbuddy。跨请求续接（保活长连接）未移植，见 [DEVELOPMENT.md](DEVELOPMENT.md)。

## 安装

1. 编译（本机无 Go 时用 docker）：

```bash
docker run --rm --platform linux/amd64 -v "$PWD":/src -w /src -v kuku-gomod:/go/pkg/mod \
  golang:1.26-bookworm bash -c 'export PATH=$PATH:/usr/local/go/bin && ./build.sh'
```

产物：`dist/kuku-linux-amd64.so`。

2. 部署到 CPA 的插件目录，**文件名必须是 `kuku.so`**（宿主用文件名推导插件 id）：

```bash
cp dist/kuku-linux-amd64.so /path/to/cpa/plugins/kuku.so
docker restart cli-proxy-api
```

3. 在 `config.yaml` 里启用：

```yaml
plugins:
  enabled: true
  configs:
    kuku:
      enabled: true
      priority: 100
      model_prefix: kuku          # 模型对外 id 变成 kuku/<上游 id>
      publish_mode: catalog       # catalog=全部；allow=只发布 extra_models + publish_allow
      # publish_allow: [gateway-glm-5.3-flash]
      # extra_models: [some-hidden-model]
      daily_bonus: false           # 自动签到，默认关闭（见「每日签到」）
```

## 添加账号

在 CPA 面板侧边栏点「**库库登录**」（或直接打开 `/v0/resource/plugins/kuku/login`），首次填一次 CPA 管理密钥（只存浏览器 localStorage），然后三种方式任选：

| 方式 | 操作 |
|---|---|
| **扫码登录**（推荐） | 点「生成二维码」，用**百度 App** 扫码并在手机上确认。二维码 4 分钟有效，页面自动轮询。 |
| **短信登录** | 填 11 位手机号 → 发送验证码 → 填 6 位验证码登录。上游要图形验证码时会显示图片（图片由插件代取，因为 passport 把它绑在插件进程的设备 Cookie 上）。 |
| **粘贴 Cookie** | 兜底：从 `kuku.baidu.com` 的 Cookie 里复制 BDUSS + PTOKEN。 |

三种方式都在**插件进程内**完成真实登录，直接取到 BDUSS/PTOKEN，再用 PTOKEN 换取 STOKEN、校验、保存为 CPA 凭据。**Cookie 不经过浏览器，插件也不会回显任何凭据值**——成功只返回 uid。多个账号重复即可，按上游 uid 区分文件。

## 使用

客户端照常用 CPA 的统一密钥：

| 配置项 | 值 |
|---|---|
| Base URL | `https://你的CPA域名/v1` |
| API Key | CPA 的普通 API Key |
| 模型 | `kuku/<上游模型 id>`，如 `kuku/gateway-glm-5.3-flash` |

模型列表通过 `model_prefix` 命名空间隔离，避免和 xAI / Codex / WorkBuddy 的模型撞名；也可以用 `publish_mode: allow` 只发布你想要的模型。

## 每日签到

面板「**库库额度**」页列出各账号剩余积分，并提供签到开关与「立即签到一次」。

- 默认**关闭**。开启后每天在设定时间（默认 9 点）后为每个账号领一次，状态存在 CPA 的 `data/kuku-signin.state`。
- 也可用 `config.yaml` 的 `daily_bonus: true` 开启，或在管理接口 `…/kuku/signin/{status,toggle,run}` 操作。
- 「登录」类任务只需上报即可领取；「对话」类任务必须先真跑一轮对话才可领——那一轮**会消耗积分**，账号额度很小时净收益可能为负，自己判断是否开。
- 账号之间串行领取，每个账号间隔 15 秒。

## 许可

插件代码 MIT（见 [LICENSE](LICENSE)）。上游协议实现移植自 `IKUNHEIHIE/kuku2api`（0BSD），登录与上游调用逻辑的功劳归原作者。自动签到属于官方规则灰色地带，风险自担。
