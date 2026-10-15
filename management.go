package main

// Management surface. 库库AI logs in with a phone number / QR code, not an
// OAuth redirect, so the host's built-in "open this URL" login cannot express
// it. Instead the plugin registers a login page (management resource) and an
// authenticated route the page posts credentials to.
//
//   - resource route   /v0/resource/plugins/kuku/login -> login page (HTML).
//     Served WITHOUT management auth, so it carries no data: it collects the
//     operator's management key into localStorage, then posts the credential to
//     the authenticated route below.
//   - management route /v0/management/kuku/login       -> submit + save (JSON).
//
// The host's own auth-files panel is the account list; this plugin does not
// duplicate it.

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	managementMu       sync.Mutex
	managementBasePath string
	resourceBasePath   string
)

func handleManagementRegister(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRegistrationRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// The SDK struct has no json tags, so accept the snake_case spelling too.
	var loose struct {
		BasePath         string `json:"base_path"`
		ResourceBasePath string `json:"resource_base_path"`
	}
	_ = json.Unmarshal(raw, &loose)

	base := firstNonEmpty(strings.TrimRight(strings.TrimSpace(req.BasePath), "/"),
		strings.TrimRight(strings.TrimSpace(loose.BasePath), "/"), "/v0/management")
	resource := firstNonEmpty(strings.TrimRight(strings.TrimSpace(req.ResourceBasePath), "/"),
		strings.TrimRight(strings.TrimSpace(loose.ResourceBasePath), "/"), "/v0/resource/plugins/"+providerName)
	managementMu.Lock()
	managementBasePath = base
	resourceBasePath = resource
	managementMu.Unlock()

	return okEnvelope(pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{
				Method:      http.MethodPost,
				Path:        base + "/" + providerName + "/login",
				Description: "提交库库AI 凭据（BDUSS + PTOKEN/STOKEN）并保存（JSON，需管理鉴权）",
			},
			{
				Method:      http.MethodGet,
				Path:        base + "/" + providerName + "/login",
				Description: "库库AI 登录状态（JSON，需管理鉴权）",
			},
			{
				Method:      http.MethodGet,
				Path:        base + "/" + providerName + "/accounts",
				Description: "库库AI 各账号剩余积分（JSON，需管理鉴权）",
			},
			{
				Method:      http.MethodGet,
				Path:        base + "/" + providerName + "/signin/status",
				Description: "库库AI 自动签到状态（JSON，需管理鉴权）",
			},
			{
				Method:      http.MethodPost,
				Path:        base + "/" + providerName + "/signin/toggle",
				Description: "开关库库AI 自动签到（JSON，需管理鉴权）",
			},
			{
				Method:      http.MethodPost,
				Path:        base + "/" + providerName + "/signin/run",
				Description: "立即执行一次库库AI 签到（JSON，需管理鉴权）",
			},
		},
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        "/login",
				Menu:        "库库登录",
				Description: "添加库库AI 账号（粘贴 BDUSS 与 PTOKEN）",
			},
			{
				Path:        "/quota",
				Menu:        "库库额度",
				Description: "库库AI 各账号剩余积分与签到开关",
			},
		},
	})
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Resource routes are served WITHOUT management auth, so they carry no data:
	// they collect the operator's key and re-fetch the authenticated route.
	if isResourcePath(req.Path) {
		if strings.HasSuffix(req.Path, "/quota") {
			return htmlEnvelope(renderQuotaPage())
		}
		return htmlEnvelope(renderLoginPage())
	}

	path := req.Path
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	switch {
	case strings.HasSuffix(path, "/"+providerName+"/login"):
		if method == http.MethodGet {
			return jsonEnvelope(map[string]any{"ok": true, "provider": providerName})
		}
		return submitLogin(req)
	case strings.HasSuffix(path, "/"+providerName+"/accounts"):
		return jsonEnvelope(collectAccounts())
	case strings.HasSuffix(path, "/"+providerName+"/signin/status"):
		return jsonEnvelope(signinStatus())
	case strings.HasSuffix(path, "/"+providerName+"/signin/toggle"):
		if method != http.MethodPost {
			return badRequest("method not allowed")
		}
		switch strings.ToLower(strings.TrimSpace(req.Query.Get("enabled"))) {
		case "1", "true", "on":
			applyDailyBonusConfig(true)
			startSigninScheduler()
		case "0", "false", "off":
			applyDailyBonusConfig(false)
			stopSigninScheduler()
		default:
			return badRequest("enabled must be 0 or 1")
		}
		signinMu.Lock()
		persistSigninState()
		signinMu.Unlock()
		return jsonEnvelope(signinStatus())
	case strings.HasSuffix(path, "/"+providerName+"/signin/run"):
		if method != http.MethodPost {
			return badRequest("method not allowed")
		}
		return jsonEnvelope(runSignin())
	case strings.HasSuffix(path, "/"+providerName+"/quota"):
		return jsonEnvelope(collectAccounts())
	}
	return badRequest("unknown route")
}

// signinStatus is the JSON shape of the sign-in state.
func signinStatus() map[string]any {
	signinMu.Lock()
	defer signinMu.Unlock()
	return map[string]any{"enabled": signinData.Enabled, "hour": signinData.Hour,
		"last_run_at": signinData.LastRunAt, "last_run": signinData.LastRun}
}

// accountBalance is one row of the quota page.
type accountBalance struct {
	UID     string  `json:"uid"`
	Label   string  `json:"label"`
	Balance float64 `json:"balance"`
	Error   string  `json:"error,omitempty"`
}

// collectAccounts lists this plugin's credentials and their remaining points.
func collectAccounts() map[string]any {
	out := map[string]any{"generated_at": time.Now().Format(time.RFC3339)}
	creds, err := ownCredentials()
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	rows := make([]accountBalance, 0, len(creds))
	for _, sa := range creds {
		row := accountBalance{UID: sa.UID, Label: "库库AI (" + firstNonEmpty(sa.Nickname, sa.UID, "unknown") + ")"}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		balance, err := fetchBalance(ctx, sa)
		cancel()
		if err != nil {
			row.Error = err.Error()
		} else {
			row.Balance = balance
		}
		rows = append(rows, row)
	}
	out["accounts"] = rows
	return out
}

// fetchBalance reads the token bucket balance from the vip-remain payload.
func fetchBalance(ctx context.Context, sa *storedAuth) (float64, error) {
	data, err := sa.client().vipRemain(ctx)
	if err != nil {
		return 0, err
	}
	if v, ok := numFloat(data["balance_points"]); ok {
		return v, nil
	}
	// Fall back to the token asset bucket (asset_type 1) when the flat field is absent.
	if assets, ok := data["assets"].([]any); ok {
		for _, a := range assets {
			asset, _ := a.(map[string]any)
			if asset == nil {
				continue
			}
			if t, _ := num(asset["asset_type"]); t == 1 {
				if v, ok := numFloat(asset["balance"]); ok {
					return v, nil
				}
				if v, ok := numFloat(asset["balance_points"]); ok {
					return v, nil
				}
			}
		}
	}
	return 0, fmt.Errorf("上游未返回积分字段")
}

// submitLogin is the authenticated credential-submission route.
func submitLogin(req pluginapi.ManagementRequest) ([]byte, error) {
	var body struct {
		BDUSS  string `json:"bduss"`
		PTOKEN string `json:"ptoken"`
		STOKEN string `json:"stoken"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return badRequest("invalid JSON body")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sa, err := loginFromCredentials(ctx, body.BDUSS, body.PTOKEN, body.STOKEN)
	if err != nil {
		markLoginError(strings.TrimSpace(body.State), err.Error())
		return jsonEnvelope(map[string]any{"ok": false, "error": err.Error()})
	}
	// Persist into the host auth store so the host can select this credential.
	name := providerName + "-" + accountIdentity(sa) + ".json"
	storage, _ := json.Marshal(sa)
	if err := saveAuthFile(name, storage); err != nil {
		markLoginError(strings.TrimSpace(body.State), "保存凭据失败: "+err.Error())
		return jsonEnvelope(map[string]any{"ok": false, "error": "保存凭据失败: " + err.Error()})
	}
	if s := strings.TrimSpace(body.State); s != "" {
		markLoginReady(s, sa)
	}
	hostLog("info", "kuku: credential saved", map[string]any{"uid": sa.UID, "file": name})
	return jsonEnvelope(map[string]any{"ok": true, "uid": sa.UID, "file": name})
}

// -----------------------------------------------------------------------------
// Login page
// -----------------------------------------------------------------------------

func renderLoginPage() string {
	target := managementBase() + "/" + providerName + "/login"
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>库库AI 登录</title><style>` + pageCSS + `</style></head><body><main>`)
	b.WriteString(`<h1>库库AI 登录</h1>`)
	b.WriteString(`<p class="meta">粘贴浏览器中的 <b>BDUSS</b> 与 <b>PTOKEN</b>（登录 kuku.baidu.com 后从 Cookie 中获取）。插件会用 PTOKEN 换取 STOKEN 并校验，然后保存为 CPA 凭据。</p>`)
	b.WriteString(`<form id="gate" class="card"><label>管理密钥 <input id="key" type="password" autocomplete="off" placeholder="MGMT_KEY"></label> <button type="submit">继续</button></form>`)
	b.WriteString(`<form id="login" class="card" hidden>`)
	b.WriteString(`<label>BDUSS <input id="bduss" autocomplete="off" placeholder="BDUSS"></label>`)
	b.WriteString(`<label>PTOKEN <input id="ptoken" autocomplete="off" placeholder="PTOKEN（推荐）"></label>`)
	b.WriteString(`<label>STOKEN <input id="stoken" autocomplete="off" placeholder="STOKEN（可选，填了就不换）"></label>`)
	b.WriteString(`<button type="submit">登录并保存</button></form>`)
	b.WriteString(`<p id="msg" class="meta"></p>`)
	b.WriteString(`<script>` + loginScript(target) + `</script>`)
	b.WriteString(`</main></body></html>`)
	return b.String()
}

func loginScript(target string) string {
	return `
var KEY = localStorage.getItem('kuku.mgmt.key') || '';
var gate = document.getElementById('gate');
var login = document.getElementById('login');
var keyInput = document.getElementById('key');
var msg = document.getElementById('msg');
var state = new URLSearchParams(location.search).get('state') || '';
function show() { gate.hidden = true; login.hidden = false; }
gate.addEventListener('submit', function (e) {
  e.preventDefault(); KEY = keyInput.value.trim();
  if (KEY) { localStorage.setItem('kuku.mgmt.key', KEY); show(); }
});
login.addEventListener('submit', function (e) {
  e.preventDefault();
  msg.textContent = '正在登录并校验…';
  var payload = {
    bduss: document.getElementById('bduss').value.trim(),
    ptoken: document.getElementById('ptoken').value.trim(),
    stoken: document.getElementById('stoken').value.trim(),
    state: state
  };
  fetch('` + target + `', {method: 'POST', headers: {'Content-Type': 'application/json', 'X-Management-Key': KEY}, body: JSON.stringify(payload)})
    .then(function (r) { return r.json(); })
    .then(function (j) {
      if (j.ok) { msg.textContent = '成功：账号 ' + (j.uid || '') + ' 已保存。可关闭本页。'; }
      else { msg.textContent = '失败：' + (j.error || '未知错误'); }
    })
    .catch(function (err) { msg.textContent = '请求失败：' + err.message; });
});
if (KEY) { show(); }
`
}

// -----------------------------------------------------------------------------
// Quota page
// -----------------------------------------------------------------------------

// renderQuotaPage is self-contained: it asks for the management key, then reads
// the authenticated JSON routes. No server-side data on a no-auth resource route.
func renderQuotaPage() string {
	base := managementBase() + "/" + providerName
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>库库AI 额度</title><style>` + pageCSS + `</style></head><body><main>`)
	b.WriteString(`<h1>库库AI 额度</h1>`)
	b.WriteString(`<form id="gate" class="card"><label>管理密钥 <input id="key" type="password" autocomplete="off" placeholder="MGMT_KEY"></label> <button type="submit">继续</button></form>`)
	b.WriteString(`<div id="panel" hidden>`)
	b.WriteString(`<p class="meta">各账号剩余积分。自动签到默认关闭；开启后每天在设定时间后为每个账号领取一次。</p>`)
	b.WriteString(`<table id="rows"><thead><tr><th>账号</th><th>剩余积分</th><th>说明</th></tr></thead><tbody></tbody></table>`)
	b.WriteString(`<div class="card"><label>签到时间（0-23）<input id="hour" type="number" min="0" max="23" value="9"></label>`)
	b.WriteString(`<button id="toggle" type="button">开启自动签到</button> <button id="run" type="button">立即签到一次</button></div>`)
	b.WriteString(`<p id="msg" class="meta"></p></div>`)
	b.WriteString(`<script>` + quotaScript(base) + `</script>`)
	b.WriteString(`</main></body></html>`)
	return b.String()
}

func quotaScript(base string) string {
	return `
var BASE = '` + base + `';
var KEY = localStorage.getItem('kuku.mgmt.key') || '';
var gate = document.getElementById('gate');
var panel = document.getElementById('panel');
var msg = document.getElementById('msg');
var rows = document.querySelector('#rows tbody');
var hour = document.getElementById('hour');
var toggle = document.getElementById('toggle');
var state = {enabled: false};
function esc(v) { return String(v == null ? '' : v).replace(/[&<>"]/g, function (c) { return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]; }); }
function call(path, opts) {
  opts = opts || {};
  opts.headers = {'Content-Type': 'application/json', 'X-Management-Key': KEY};
  return fetch(BASE + path, opts).then(function (r) { return r.json().then(function (j) { if (!r.ok) throw new Error(j.error || r.statusText); return j; }); });
}
function render() {
  toggle.textContent = state.enabled ? '关闭自动签到' : '开启自动签到';
  hour.value = state.hour;
}
function load() {
  rows.innerHTML = '<tr><td colspan="3">加载中…</td></tr>';
  call('/accounts').then(function (j) {
    if (j.error) { rows.innerHTML = '<tr><td colspan="3">' + esc(j.error) + '</td></tr>'; return; }
    var list = j.accounts || [];
    if (!list.length) { rows.innerHTML = '<tr><td colspan="3">还没有账号，请先在“库库登录”页添加。</td></tr>'; return; }
    rows.innerHTML = list.map(function (a) {
      return '<tr><td>' + esc(a.label) + '</td><td>' + esc(a.error ? '-' : a.balance) + '</td><td>' + esc(a.error || '') + '</td></tr>';
    }).join('');
  }).catch(function (e) { rows.innerHTML = '<tr><td colspan="3">' + esc(e.message) + '</td></tr>'; });
  return call('/signin/status').then(function (j) { state = j; render(); }).catch(function () {});
}
gate.addEventListener('submit', function (e) {
  e.preventDefault(); KEY = document.getElementById('key').value.trim();
  if (!KEY) return; localStorage.setItem('kuku.mgmt.key', KEY);
  gate.hidden = true; panel.hidden = false; load();
});
toggle.addEventListener('click', function () {
  call('/signin/toggle?enabled=' + (state.enabled ? '0' : '1'), {method: 'POST'})
    .then(function (j) { state = j; render(); msg.textContent = '自动签到已' + (j.enabled ? '开启' : '关闭'); })
    .catch(function (e) { msg.textContent = '失败：' + e.message; });
});
document.getElementById('run').addEventListener('click', function () {
  msg.textContent = '正在签到，请稍候…';
  call('/signin/run', {method: 'POST'})
    .then(function (j) { msg.textContent = '完成：' + JSON.stringify(j.accounts || []); load(); })
    .catch(function (e) { msg.textContent = '失败：' + e.message; });
});
hour.addEventListener('change', function () {
  msg.textContent = '签到时间已设为 ' + hour.value + ' 时（重新开启开关后生效）';
});
if (KEY) { gate.hidden = true; panel.hidden = false; load(); }
`
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func isResourcePath(path string) bool {
	return strings.Contains(path, "/resource/")
}

func managementBase() string {
	managementMu.Lock()
	defer managementMu.Unlock()
	if strings.TrimSpace(managementBasePath) == "" {
		return "/v0/management"
	}
	return managementBasePath
}

func resourceURL(baseURL, path string) string {
	managementMu.Lock()
	base := resourceBasePath
	managementMu.Unlock()
	if strings.TrimSpace(base) == "" {
		base = "/v0/resource/plugins/" + providerName
	}
	prefix := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if prefix == "" {
		return base + path
	}
	return prefix + base + path
}

func jsonEnvelope(v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":  []string{"application/json; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body: body,
	})
}

func htmlEnvelope(page string) ([]byte, error) {
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":  []string{"text/html; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body: []byte(page),
	})
}

func badRequest(msg string) ([]byte, error) {
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusBadRequest,
		Headers: http.Header{
			"Content-Type":  []string{"text/plain; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body: []byte(msg),
	})
}

var _ = fmt.Sprintf
var _ = html.EscapeString

const pageCSS = `
:root{color-scheme:light dark}
body{margin:0;font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;background:#f5f6f8;color:#1c1e21}
main{max-width:720px;margin:0 auto;padding:28px 20px}
h1{font-size:20px;margin:0 0 6px}
.meta{color:#65676b;font-size:13px;margin:6px 0 14px}
.card{background:#fff;border:1px solid #dcdfe3;border-radius:10px;padding:16px;margin:12px 0}
label{display:block;margin:10px 0;font-size:13px;color:#333}
input{display:block;width:100%;box-sizing:border-box;margin-top:4px;padding:8px 10px;border:1px solid #ccd0d5;border-radius:8px;font-size:14px}
button{margin-top:12px;padding:9px 16px;border:0;border-radius:8px;background:#2563eb;color:#fff;font-size:14px;cursor:pointer}
button:hover{background:#1d4ed8}
@media (prefers-color-scheme:dark){body{background:#18191a;color:#e4e6eb}.card{background:#242526;border-color:#3a3b3c}.meta{color:#b0b3b8}input{background:#3a3b3c;border-color:#4e4f50;color:#e4e6eb}}
`
