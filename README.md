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
| 客户端工具调用（原生 MCP connector 桥接，即 Agent 能力） | ⏳ **未实现（phase 2）**，当前带 `tools` 的请求会明确报错 |
| 每日签到（自动领积分） | ⏳ 未实现（phase 3） |

**目前是纯文本聊天。** 库库AI 的 Agent 能力（模型发起工具调用、由调用方执行）需要走上游的原生 MCP connector 通道，这是 phase 2 的工作量主体，见 [DEVELOPMENT.md](DEVELOPMENT.md)。

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
      # daily_bonus: false        # phase 3 才会生效
```

## 添加账号

库库AI 用手机号/扫码登录，不是 OAuth 重定向，所以插件自带一个登录页：

1. 在 CPA 面板侧边栏点「**库库登录**」，或点 provider 的登录按钮（会打开同一个页面）。
2. 首次打开需要填一次 CPA 的管理密钥（`MGMT_KEY`），只存在浏览器 localStorage。
3. 浏览器登录 `kuku.baidu.com`，从 Cookie 里复制 **BDUSS** 与 **PTOKEN** 填进去。
4. 插件用 PTOKEN 换取 STOKEN、校验登录、保存为 CPA 凭据。账号随即出现在 CPA 的凭据列表里。

> 也可以直接提供 STOKEN（填了就不换取）。多个账号重复上述步骤即可，插件按上游 uid 区分文件。

## 使用

客户端照常用 CPA 的统一密钥：

| 配置项 | 值 |
|---|---|
| Base URL | `https://你的CPA域名/v1` |
| API Key | CPA 的普通 API Key |
| 模型 | `kuku/<上游模型 id>`，如 `kuku/gateway-glm-5.3-flash` |

模型列表通过 `model_prefix` 命名空间隔离，避免和 xAI / Codex / WorkBuddy 的模型撞名；也可以用 `publish_mode: allow` 只发布你想要的模型。

## 许可

插件代码 MIT（见 [LICENSE](LICENSE)）。上游协议实现移植自 `IKUNHEIHIE/kuku2api`（0BSD），登录与上游调用逻辑的功劳归原作者。自动签到属于官方规则灰色地带，风险自担。
