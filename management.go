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
	"regexp"
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
				Method:      http.MethodPost,
				Path:        base + "/" + providerName + "/login/qr",
				Description: "发起库库AI 扫码登录，返回二维码图片（JSON，需管理鉴权）",
			},
			{
				Method:      http.MethodGet,
				Path:        base + "/" + providerName + "/login/qr/{id}",
				Description: "轮询库库AI 扫码状态（JSON，需管理鉴权）",
			},
			{
				Method:      http.MethodPost,
				Path:        base + "/" + providerName + "/login/sms",
				Description: "发起库库AI 短信登录（JSON，需管理鉴权）",
			},
			{
				Method:      http.MethodPost,
				Path:        base + "/" + providerName + "/login/sms/resend",
				Description: "重发库库AI 短信验证码（JSON，需管理鉴权）",
			},
			{
				Method:      http.MethodPost,
				Path:        base + "/" + providerName + "/login/sms/verify",
				Description: "提交库库AI 短信验证码完成登录（JSON，需管理鉴权）",
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
				// One route = one sidebar entry. The path stays /quota so the URL
				// that has been in use keeps working; the page itself is the whole
				// panel (accounts + login + daily claim).
				Path:        "/quota",
				Menu:        "库库AI",
				Description: "账号、积分与自动签到",
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
	// they collect the operator's key and re-fetch the authenticated routes.
	// Every resource path serves the same single page, so the sidebar entry and
	// any older bookmark (/login, /quota) all land on the same panel.
	if isResourcePath(req.Path) {
		return htmlEnvelope(renderKukuPage())
	}

	path := req.Path
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	switch {
	case strings.HasSuffix(path, "/"+providerName+"/login/qr"):
		if method != http.MethodPost {
			return badRequest("method not allowed")
		}
		return loginStartQr()
	case qrPollRe.MatchString(path):
		if method != http.MethodGet {
			return badRequest("method not allowed")
		}
		return loginPollQr(qrPollRe.FindStringSubmatch(path)[1])
	case strings.HasSuffix(path, "/"+providerName+"/login/sms/resend"):
		return loginSmsResend(req)
	case strings.HasSuffix(path, "/"+providerName+"/login/sms/verify"):
		return loginSmsVerify(req)
	case strings.HasSuffix(path, "/"+providerName+"/login/sms"):
		return loginSmsStart(req)
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

// accountAsset is one billing bucket from the vip-remain payload.
type accountAsset struct {
	Type   int64   `json:"type"`
	Name   string  `json:"name,omitempty"`
	Total  float64 `json:"total"`
	Bonus  float64 `json:"bonus,omitempty"`
	Vip    float64 `json:"vip,omitempty"`
	Charge float64 `json:"charge,omitempty"`
	Freeze float64 `json:"freeze,omitempty"`
}

// taskItem is one daily task as the panel shows it.
type taskItem struct {
	Type      string  `json:"type"`
	Key       string  `json:"key"`
	Status    string  `json:"status"`
	Claimable float64 `json:"claimable"`
	Reward    float64 `json:"reward"`
	MaxReward float64 `json:"max_reward,omitempty"`
}

// taskSummary is today's progress: how many tasks are lit, how many can be
// claimed right now, and what the day is worth.
type taskSummary struct {
	Done      int        `json:"done"`
	Total     int        `json:"total"`
	Claimable int        `json:"claimable"`
	Earned    float64    `json:"earned"`
	Items     []taskItem `json:"items"`
}

// accountBalance is one row of the quota page.
type accountBalance struct {
	UID     string         `json:"uid"`
	Label   string         `json:"label"`
	Balance float64        `json:"balance"`
	Assets  []accountAsset `json:"assets,omitempty"`
	Tasks   *taskSummary   `json:"tasks,omitempty"`
	// Lit is the "已点亮" flag: every one of today's tasks is finished.
	Lit   bool   `json:"lit"`
	Error string `json:"error,omitempty"`
}

// balanceFrom reads the raw vip-remain payload. Measured shape:
// data.list[] = {assetType, totalPoint, bonusPoint, vipPoint, chargePoint,
// freezePoint}; assetType 1 is the token bucket the chat calls bill against
// (kuku2api's balance_points is exactly that bucket's totalPoint).
func balanceFrom(data map[string]any) ([]accountAsset, float64) {
	list, _ := data["list"].([]any)
	assets := make([]accountAsset, 0, len(list))
	for _, a := range list {
		row, _ := a.(map[string]any)
		if row == nil {
			continue
		}
		assets = append(assets, accountAsset{
			Type:   assetInt(row, "assetType", "asset_type"),
			Name:   assetStr(row, "assetName", "asset_name"),
			Total:  assetNum(row, "totalPoint", "total_point"),
			Bonus:  assetNum(row, "bonusPoint", "bonus_point"),
			Vip:    assetNum(row, "vipPoint", "vip_point"),
			Charge: assetNum(row, "chargePoint", "charge_point"),
			Freeze: assetNum(row, "freezePoint", "freeze_point"),
		})
	}
	for _, a := range assets {
		if a.Type == 1 {
			return assets, a.Total
		}
	}
	if len(assets) > 0 {
		// Upstream stopped tagging types: report the first bucket rather than 0.
		return assets, assets[0].Total
	}
	return assets, 0
}

func assetInt(row map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if n, ok := num(row[k]); ok {
			return n
		}
	}
	return 0
}

func assetNum(row map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if f, ok := numFloat(row[k]); ok {
			return f
		}
	}
	return 0
}

func assetStr(row map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := row[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// Snapshot
// -----------------------------------------------------------------------------

// accountsCacheTTL keeps a page refresh from firing two upstream calls per
// credential on every click.
const accountsCacheTTL = 60 * time.Second

var (
	accountsMu    sync.Mutex
	accountsCache map[string]any
	accountsAt    time.Time
)

// collectAccounts lists this plugin's credentials with their balance and today's
// task progress.
func collectAccounts() map[string]any {
	accountsMu.Lock()
	if accountsCache != nil && time.Since(accountsAt) < accountsCacheTTL {
		cached := accountsCache
		accountsMu.Unlock()
		return cached
	}
	accountsMu.Unlock()

	out := map[string]any{"generated_at": time.Now().Format(time.RFC3339)}
	creds, err := ownCredentials()
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	rows := make([]accountBalance, len(creds))
	var wg sync.WaitGroup
	// ponytail: fixed fan-out cap; raise it if the account count grows a lot.
	sem := make(chan struct{}, 4)
	for i, sa := range creds {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, sa *storedAuth) {
			defer wg.Done()
			defer func() { <-sem }()
			rows[idx] = fetchAccount(sa)
		}(i, sa)
	}
	wg.Wait()
	out["accounts"] = rows

	accountsMu.Lock()
	accountsCache, accountsAt = out, time.Now()
	accountsMu.Unlock()
	return out
}

// fetchAccount reads one account's balance and its daily-task progress.
func fetchAccount(sa *storedAuth) accountBalance {
	row := accountBalance{UID: sa.UID, Label: "库库AI (" + firstNonEmpty(sa.Nickname, sa.UID, "unknown") + ")"}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	client := sa.client()
	if data, err := client.vipRemain(ctx); err != nil {
		row.Error = err.Error()
	} else {
		row.Assets, row.Balance = balanceFrom(data)
		// The payload carries the display name; use it so the row is readable
		// even for credentials stored before nicknames were captured.
		if name := str(data["uname"]); name != "" {
			row.Label = "库库AI (" + name + ")"
		}
	}
	summary := taskSummary{}
	if tasks, err := listTasks(ctx, client); err == nil {
		summary = taskSummaryOf(tasks)
		row.Tasks = &summary
		row.Lit = summary.Total > 0 && summary.Done == summary.Total
	} else if row.Error == "" {
		row.Error = err.Error()
	}
	hostLog("info", "kuku: quota snapshot", map[string]any{
		"uid": row.UID, "balance": row.Balance, "assets": len(row.Assets),
		"tasks_total": summary.Total, "tasks_done": summary.Done, "lit": row.Lit,
		"error": row.Error,
	})
	return row
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
	name, err := persistCredential(sa)
	if err != nil {
		markLoginError(strings.TrimSpace(body.State), "保存凭据失败: "+err.Error())
		return jsonEnvelope(map[string]any{"ok": false, "error": "保存凭据失败: " + err.Error()})
	}
	if s := strings.TrimSpace(body.State); s != "" {
		markLoginReady(s, sa)
	}
	hostLog("info", "kuku: credential saved", map[string]any{"uid": sa.UID, "file": name})
	return jsonEnvelope(map[string]any{"ok": true, "uid": sa.UID, "file": name})
}

// persistCredential writes a verified credential into the host auth store so the
// host can select it. The single place that names a credential file.
func persistCredential(sa *storedAuth) (string, error) {
	name := providerName + "-" + accountIdentity(sa) + ".json"
	storage, err := json.Marshal(sa)
	if err != nil {
		return "", err
	}
	if err := saveAuthFile(name, storage); err != nil {
		return "", err
	}
	return name, nil
}

var qrPollRe = regexp.MustCompile(`/login/qr/([A-Za-z0-9_-]+)$`)

// -----------------------------------------------------------------------------
// Login flows (QR / SMS) — the page drives these with the management key
// -----------------------------------------------------------------------------

func loginStartQr() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	out, err := startQrLogin(ctx)
	if err != nil {
		return jsonEnvelope(map[string]any{"state": "failed", "message": err.Error()})
	}
	return jsonEnvelope(out)
}

func loginPollQr(id string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return jsonEnvelope(pollQrLogin(ctx, id))
}

func loginSmsStart(req pluginapi.ManagementRequest) ([]byte, error) {
	if strings.ToUpper(strings.TrimSpace(req.Method)) != http.MethodPost {
		return badRequest("method not allowed")
	}
	var body struct {
		Phone string `json:"phone"`
	}
	_ = json.Unmarshal(req.Body, &body)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return jsonEnvelope(startSmsLogin(ctx, body.Phone))
}

func loginSmsResend(req pluginapi.ManagementRequest) ([]byte, error) {
	var body struct {
		LoginID     string `json:"login_id"`
		CaptchaCode string `json:"captcha_code"`
	}
	if json.Unmarshal(req.Body, &body) != nil {
		return badRequest("invalid JSON body")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return jsonEnvelope(resendSmsLogin(ctx, strings.TrimSpace(body.LoginID), body.CaptchaCode, "", ""))
}

func loginSmsVerify(req pluginapi.ManagementRequest) ([]byte, error) {
	var body struct {
		LoginID     string `json:"login_id"`
		Code        string `json:"code"`
		CaptchaCode string `json:"captcha_code"`
	}
	if json.Unmarshal(req.Body, &body) != nil {
		return badRequest("invalid JSON body")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return jsonEnvelope(verifySmsLogin(ctx, strings.TrimSpace(body.LoginID), body.Code, body.CaptchaCode, "", ""))
}

// -----------------------------------------------------------------------------
// Panel page
// -----------------------------------------------------------------------------

// renderKukuPage is the plugin's single management page: accounts, balance, the
// daily-claim switch and the add-account flows all live here. One page means one
// sidebar entry, and the operator never has to wonder which tab holds what.
//
// It is served on the browser-navigable resource route, which the host exposes
// WITHOUT management auth, so the page starts empty and only reads account data
// after the operator supplies the management key.
func renderKukuPage() string {
	base := managementBase() + "/" + providerName
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>库库AI</title><style>` + pageCSS + `</style></head><body><main>`)
	b.WriteString(`<h1>库库AI</h1>`)
	b.WriteString(`<p class="meta">账号、积分与自动签到。登录在插件进程内完成，Cookie 不经过浏览器。</p>`)
	b.WriteString(`<form id="gate" class="card"><label>管理密钥 <input id="key" type="password" autocomplete="off" placeholder="MGMT_KEY"></label> <button type="submit">打开</button></form>`)
	b.WriteString(`<p id="gate-err" class="err" hidden></p>`)

	b.WriteString(`<div id="panel" hidden>`)
	b.WriteString(`<p class="switchline"><label><input type="checkbox" id="daily" onchange="kukuToggle(this)"> 自动签到</label>`)
	b.WriteString(`<span class="quiet" id="schedule"></span>`)
	b.WriteString(`<button type="button" id="run" class="ghost">立即执行一次</button>`)
	b.WriteString(`<span class="quiet" id="last"></span></p>`)
	b.WriteString(`<div id="list"></div>`)

	b.WriteString(`<section class="card"><header><h2>添加账号</h2>`)
	b.WriteString(`<span class="quiet">扫码或短信验证码登录；Cookie 只是兜底</span></header>`)
	b.WriteString(`<div class="tabs" role="tablist">`)
	b.WriteString(`<button type="button" class="tab active" data-tab="qr">扫码登录</button>`)
	b.WriteString(`<button type="button" class="tab" data-tab="sms">短信登录</button>`)
	b.WriteString(`<button type="button" class="tab" data-tab="cookie">粘贴 Cookie</button></div>`)

	b.WriteString(`<div id="tab-qr"><p class="quiet">用<b>百度 App</b> 扫码，并在手机上确认登录。二维码 4 分钟内有效。</p>`)
	b.WriteString(`<button type="button" id="qr-start">生成二维码</button>`)
	b.WriteString(`<div id="qr-box" hidden><img id="qr-img" alt="登录二维码" width="200" height="200"><p id="qr-state" class="quiet"></p></div></div>`)

	b.WriteString(`<div id="tab-sms" hidden><label>手机号 <input id="phone" inputmode="numeric" autocomplete="off" placeholder="11 位中国大陆手机号"></label>`)
	b.WriteString(`<button type="button" id="sms-send">发送验证码</button>`)
	b.WriteString(`<div id="sms-captcha-box" hidden><label>图形验证码 <input id="sms-captcha" autocomplete="off" placeholder="看图输入"></label><img id="sms-captcha-img" alt="图形验证码"></div>`)
	b.WriteString(`<div id="sms-code-box" hidden><label>短信验证码 <input id="sms-code" inputmode="numeric" autocomplete="off" placeholder="6 位数字"></label><button type="button" id="sms-verify">登录</button></div>`)
	b.WriteString(`<p id="sms-state" class="quiet"></p></div>`)

	b.WriteString(`<div id="tab-cookie" hidden><p class="quiet">兜底方式：粘贴浏览器里的 <b>BDUSS</b> 与 <b>PTOKEN</b>（登录 kuku.baidu.com 后从 Cookie 获取）。</p>`)
	b.WriteString(`<label>BDUSS <input id="bduss" autocomplete="off" placeholder="BDUSS"></label>`)
	b.WriteString(`<label>PTOKEN <input id="ptoken" autocomplete="off" placeholder="PTOKEN（推荐）"></label>`)
	b.WriteString(`<label>STOKEN <input id="stoken" autocomplete="off" placeholder="STOKEN（可选，填了就不换）"></label>`)
	b.WriteString(`<button type="button" id="cookie-save">登录并保存</button></div>`)
	b.WriteString(`</section>`)

	b.WriteString(`<p id="msg" class="quiet"></p>`)
	b.WriteString(`</div>`)
	b.WriteString(`<script>` + kukuScript(base) + `</script>`)
	b.WriteString(`</main></body></html>`)
	return b.String()
}

// kukuScript is the whole page behaviour: the key gate, the account cards, the
// daily-claim switch and the three login flows. Every request carries the
// operator's key in X-Management-Key; nothing is stored server-side.
func kukuScript(base string) string {
	return `
var BASE = '` + base + `';
var KEY = localStorage.getItem('kuku.mgmt.key') || '';
var state = {enabled: false, hour: 9, last_run: null};
var pollTimer = null;
var smsID = '';
function $(id) { return document.getElementById(id); }
function esc(v) { return String(v == null ? '' : v).replace(/[&<>"]/g, function (c) { return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]; }); }
function fmt(n) { return Number(n || 0).toLocaleString('zh-CN', {maximumFractionDigits: 2}); }
function call(path, method, body) {
  return fetch(BASE + path, {
    method: method || 'GET',
    headers: {'Content-Type': 'application/json', 'X-Management-Key': KEY},
    body: body ? JSON.stringify(body) : undefined
  }).then(function (r) {
    return r.json().catch(function () { return {}; }).then(function (j) {
      if (!r.ok) throw new Error(j.error || j.message || ('HTTP ' + r.status));
      return j;
    });
  });
}

// ---------------------------------------------------------------- key gate ---
$('gate').addEventListener('submit', function (e) {
  e.preventDefault();
  KEY = $('key').value.trim();
  if (!KEY) return;
  localStorage.setItem('kuku.mgmt.key', KEY);
  $('gate').hidden = true;
  $('panel').hidden = false;
  load();
});
function gateFail(e) {
  $('gate').hidden = false;
  $('panel').hidden = true;
  var p = $('gate-err');
  p.hidden = false;
  p.textContent = '打开失败：' + (e && e.message ? e.message : '管理密钥无效');
}

// ------------------------------------------------------------- daily claim ---
var TASK_NAMES = {LOGIN: '每日登录', CHAT: '对话任务', SELF_DOWNLOAD: '下载客户端', DESKTOP_LOGIN: '桌面登录'};
var ASSET_NAMES = {token: '积分', duration: '时长(分钟)', scheduled_task: '定时任务', realtime_t: '实时任务'};

function renderSwitch() {
  $('daily').checked = !!state.enabled;
  $('schedule').textContent = '每天 ' + String(state.hour).padStart(2, '0') + ':00 起为所有账号执行';
  var last = state.last_run;
  if (!last) { $('last').textContent = state.enabled ? '尚未执行过' : '已关闭'; return; }
  var accs = last.accounts || [];
  var earned = 0, ok = 0;
  for (var i = 0; i < accs.length; i++) { earned += accs[i].points_earned || 0; if (accs[i].ok) ok++; }
  $('last').textContent = '上次 ' + new Date(last.when).toLocaleString('zh-CN')
    + ' · ' + accs.length + ' 个账号 · 成功 ' + ok + ' · 共领取 ' + fmt(earned) + ' 分';
}
function kukuToggle(el) {
  var on = el.checked;
  if (!confirm('确定要' + (on ? '开启' : '关闭') + '自动签到吗？')) { el.checked = !on; return; }
  call('/signin/toggle?enabled=' + (on ? '1' : '0'), 'POST').then(function (j) {
    state = j; renderSwitch();
  }).catch(function (e) { alert('切换失败：' + e.message); el.checked = !on; });
}

// ------------------------------------------------------------------ cards ----
function taskDot(it) {
  var cls = it.status === 'FINISHED' ? 'on' : (it.claimable > 0 ? 'ready' : '');
  var name = TASK_NAMES[it.type] || it.type;
  var tip = name + ' · ' + it.status + (it.claimable > 0 ? ' · 可领取 ' + it.claimable + ' 分' : '');
  return '<span class="dot ' + cls + '" title="' + esc(tip) + '">' + esc(name) + '</span>';
}
function accountCard(a) {
  var t = a.tasks, done = t ? t.done : 0, total = t ? t.total : 0;
  var pct = total ? Math.round(done * 100 / total) : 0;
  var tier = pct >= 70 ? 'high' : (pct >= 30 ? 'mid' : 'low');
  var h = '<section class="card"><header><h2>' + esc(a.label) + '</h2>';
  if (a.uid) h += '<span class="badge">' + esc(a.uid) + '</span>';
  // An errored account has no known task state, so it gets no lit badge.
  if (a.error) return h + '<span class="badge">查询失败</span></header><p class="err">' + esc(a.error) + '</p></section>';
  h += '<span class="badge' + (total ? (a.lit ? ' ok' : ' warn') : '') + '">'
    + (total ? (a.lit ? '今日已点亮' : '今日未点亮') : '今日无任务') + '</span></header>';
  h += '<p class="total">剩余 <strong>' + esc(fmt(a.balance)) + '</strong> credits</p>';
  if (total) {
    h += '<p class="barlabel">今日任务 ' + done + '/' + total
      + (t.claimable ? ' · <b>可领取 ' + t.claimable + ' 项</b>' : '')
      + ' <span class="quiet">(今日可得 ' + esc(fmt(t.earned)) + ' 分)</span></p>'
      + '<div class="bar"><i class="' + tier + '" style="width:' + pct + '%"></i></div>'
      + '<div class="tasks">' + (t.items || []).map(taskDot).join('') + '</div>';
  } else {
    h += '<p class="quiet">上游当前没有可领取的签到任务（积分活动按「期」开放）。</p>';
  }
  if ((a.assets || []).length) {
    h += '<h3>账户资产</h3><div class="tasks">' + a.assets.map(function (x) {
      return '<span class="badge">' + esc(ASSET_NAMES[x.name] || x.name || ('资产' + x.type))
        + ' ' + esc(fmt(x.total)) + '</span>';
    }).join('') + '</div>';
  }
  return h + '</section>';
}
function load() {
  $('list').innerHTML = '<section class="card"><p class="quiet">加载中…</p></section>';
  call('/accounts').then(function (j) {
    if (j.error) { $('list').innerHTML = '<p class="err">' + esc(j.error) + '</p>'; return; }
    var accs = j.accounts || [];
    if (!accs.length) {
      $('list').innerHTML = '<section class="card"><p class="quiet">还没有账号，用下面的「添加账号」扫码或短信登录一个。</p></section>';
      return;
    }
    var sum = 0, lit = 0;
    for (var i = 0; i < accs.length; i++) { sum += accs[i].balance || 0; if (accs[i].lit) lit++; }
    $('list').innerHTML = '<p class="quiet">共 ' + accs.length + ' 个账号 · 合计 ' + esc(fmt(sum))
      + ' credits · 今日已点亮 ' + lit + '/' + accs.length + '</p>' + accs.map(accountCard).join('');
  }).catch(gateFail);
  return call('/signin/status').then(function (j) { state = j; renderSwitch(); }).catch(function () {});
}
$('run').addEventListener('click', function () {
  $('msg').textContent = '正在为所有账号领取，请稍候…';
  call('/signin/run', 'POST').then(function (j) {
    var accs = j.accounts || [], earned = 0, failed = [];
    for (var i = 0; i < accs.length; i++) {
      earned += accs[i].points_earned || 0;
      if (!accs[i].ok) failed.push(accs[i].uid + ': ' + (accs[i].message || ''));
    }
    $('msg').textContent = '完成：' + accs.length + ' 个账号，共领取 ' + fmt(earned) + ' 分'
      + (failed.length ? '；失败 ' + failed.length + ' 个（' + failed.join('；') + '）' : '');
    load();
  }).catch(function (e) { $('msg').textContent = '失败：' + e.message; });
});

// ------------------------------------------------------------ add account ----
var tabs = document.querySelectorAll('.tab');
Array.prototype.forEach.call(tabs, function (t) {
  t.addEventListener('click', function () {
    Array.prototype.forEach.call(tabs, function (o) { o.classList.toggle('active', o === t); });
    ['qr', 'sms', 'cookie'].forEach(function (k) { $('tab-' + k).hidden = k !== t.dataset.tab; });
  });
});
function stopPoll() { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } }
function done(j) {
  if (j.state === 'added') { stopPoll(); $('msg').textContent = '成功：账号 ' + (j.uid || '') + ' 已保存。'; load(); return true; }
  if (j.state === 'failed') { stopPoll(); $('msg').textContent = '失败：' + (j.message || '未知错误'); return true; }
  return false;
}
$('qr-start').addEventListener('click', function () {
  stopPoll(); $('qr-box').hidden = true; $('msg').textContent = '正在申请二维码…';
  call('/login/qr', 'POST').then(function (j) {
    if (j.state === 'failed') { $('msg').textContent = '失败：' + (j.message || ''); return; }
    $('qr-img').src = j.image; $('qr-box').hidden = false; $('msg').textContent = '';
    $('qr-state').textContent = '等待扫码…';
    var id = encodeURIComponent(j.login_id);
    pollTimer = setInterval(function () {
      call('/login/qr/' + id).then(function (p) {
        if (done(p)) return;
        if (p.state === 'scanned') $('qr-state').textContent = '已扫码，请在手机上确认';
        else if (p.state === 'expired') { $('qr-state').textContent = '二维码已过期，请重新生成'; stopPoll(); }
        else if (p.state === 'unknown') { $('qr-state').textContent = p.message || '登录会话已过期'; stopPoll(); }
        else $('qr-state').textContent = '等待扫码…';
      }).catch(function (e) { $('qr-state').textContent = '轮询失败：' + e.message; });
    }, 2000);
  }).catch(function (e) { $('msg').textContent = '失败：' + e.message; });
});
function renderSms(j) {
  if (j.login_id) smsID = j.login_id;
  if (done(j)) return;
  if (j.state === 'captcha_required') {
    var box = $('sms-captcha-box'), img = $('sms-captcha-img');
    box.hidden = false;
    if (j.captcha_image) { img.src = j.captcha_image; img.hidden = false; }
    else { img.removeAttribute('src'); img.hidden = true; }
    $('sms-state').textContent = (j.message || '需要图形验证码，看图输入后再次点击发送验证码')
      + (j.captcha_error ? '（图片获取失败：' + j.captcha_error + '）' : '');
    return;
  }
  if (j.state === 'sent') {
    $('sms-code-box').hidden = false; $('sms-captcha-box').hidden = true;
    $('sms-captcha-img').removeAttribute('src');
    $('sms-state').textContent = j.message || '验证码已发送';
    return;
  }
  $('sms-state').textContent = j.message || ('状态：' + j.state);
}
$('sms-send').addEventListener('click', function () {
  var phone = $('phone').value.trim();
  if (!smsID && !/^1[3-9]\d{9}$/.test(phone)) { $('sms-state').textContent = '请输入正确的 11 位手机号'; return; }
  $('sms-state').textContent = '正在发送…';
  var body = smsID ? {login_id: smsID, captcha_code: $('sms-captcha').value.trim()} : {phone: phone};
  call(smsID ? '/login/sms/resend' : '/login/sms', 'POST', body)
    .then(renderSms).catch(function (e) { $('sms-state').textContent = '失败：' + e.message; });
});
$('sms-verify').addEventListener('click', function () {
  var code = $('sms-code').value.trim();
  if (!/^\d{6}$/.test(code)) { $('sms-state').textContent = '请输入 6 位数字验证码'; return; }
  $('sms-state').textContent = '正在登录…';
  call('/login/sms/verify', 'POST', {login_id: smsID, code: code, captcha_code: $('sms-captcha').value.trim()})
    .then(renderSms).catch(function (e) { $('sms-state').textContent = '失败：' + e.message; });
});
$('cookie-save').addEventListener('click', function () {
  $('msg').textContent = '正在登录并校验…';
  call('/login', 'POST', {
    bduss: $('bduss').value.trim(), ptoken: $('ptoken').value.trim(), stoken: $('stoken').value.trim()
  }).then(function (j) {
    $('msg').textContent = j.ok ? ('成功：账号 ' + (j.uid || '') + ' 已保存。') : ('失败：' + (j.error || ''));
    if (j.ok) load();
  }).catch(function (e) { $('msg').textContent = '失败：' + e.message; });
});

if (KEY) { $('gate').hidden = true; $('panel').hidden = false; load(); }
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

// pageCSS reads the host's design tokens (the CPA panel injects --bg-primary,
// --text-primary, --primary-color … into this iframe), so the page follows the
// panel's light/dark setting instead of guessing from the OS. The literals are
// only fallbacks for viewing the page outside the panel.
const pageCSS = `
:root{
  --k-bg:var(--bg-primary,#f5f6f8);
  --k-surface:var(--app-surface,#ffffff);
  --k-muted:var(--bg-tertiary,#f6faff);
  --k-border:var(--border-color,#dcdfe3);
  --k-text:var(--text-primary,#1c1e21);
  --k-text-muted:var(--text-tertiary,#65676b);
  --k-primary:var(--primary-color,#2563eb);
  --k-primary-contrast:var(--primary-contrast,#ffffff);
  --k-radius:var(--app-radius-md,10px);
  --k-ok-bg:rgba(22,163,74,.14);--k-ok-fg:#15803d;
  --k-warn-bg:rgba(224,170,20,.18);--k-warn-fg:#a16207;
  --k-err-bg:rgba(220,38,38,.10);--k-err-fg:#b42318;
  --k-high:#16a34a;--k-mid:#e0aa14;--k-low:#dc2626;
}
body{margin:0;font:14px/1.6 var(--cpamp-plugin-font-family,system-ui,-apple-system,"Segoe UI","PingFang SC",sans-serif);background:var(--k-bg);color:var(--k-text)}
main{max-width:860px;margin:0 auto;padding:24px 20px}
h1{font-size:20px;margin:0 0 4px}
h2{font-size:15px;margin:0}
h3{font-size:13px;font-weight:500;color:var(--k-text-muted);margin:14px 0 4px}
.quiet{color:var(--k-text-muted);font-size:12px}
.meta{color:var(--k-text-muted);font-size:12px;margin:6px 0 14px}
.err{color:var(--k-err-fg);background:var(--k-err-bg);border:1px solid var(--k-border);border-radius:8px;padding:8px 12px;font-size:13px}
.card{background:var(--k-surface);border:1px solid var(--k-border);border-radius:var(--k-radius);padding:16px;margin:14px 0}
.card>header{display:flex;align-items:center;gap:8px;flex-wrap:wrap;margin-bottom:8px}
.badge{background:var(--k-muted);color:var(--k-text-muted);border-radius:999px;padding:2px 10px;font-size:12px;white-space:nowrap}
.badge.ok{background:var(--k-ok-bg);color:var(--k-ok-fg)}
.badge.warn{background:var(--k-warn-bg);color:var(--k-warn-fg)}
.total{margin:0 0 10px}
.total strong{font-size:24px}
.barlabel{margin:0;font-size:12px;color:var(--k-text-muted)}
.bar{height:10px;background:var(--k-muted);border-radius:999px;overflow:hidden;margin:6px 0 2px}
.bar i{display:block;height:100%;border-radius:999px;transition:width .25s}
.bar i.high{background:var(--k-high)}
.bar i.mid{background:var(--k-mid)}
.bar i.low{background:var(--k-low)}
.switchline{display:flex;align-items:center;gap:10px;flex-wrap:wrap;font-size:13px;margin:10px 0}
.switchline label{display:inline-flex;align-items:center;gap:6px;margin:0;cursor:pointer;user-select:none}
.switchline input[type=checkbox]{width:16px;height:16px;margin:0;padding:0}
label{display:block;margin:10px 0;font-size:13px}
input,select{display:block;width:100%;box-sizing:border-box;margin-top:4px;padding:8px 10px;border:1px solid var(--k-border);border-radius:8px;font-size:14px;background:var(--k-surface);color:var(--k-text)}
button{margin:8px 8px 8px 0;padding:9px 16px;border:1px solid transparent;border-radius:8px;background:var(--k-primary);color:var(--k-primary-contrast);font-size:14px;cursor:pointer}
button.ghost,button.tab{background:var(--k-muted);color:var(--k-text);border-color:var(--k-border)}
button:hover{filter:brightness(1.06)}
.tabs{display:flex;gap:6px;margin:10px 0 4px;flex-wrap:wrap}
.tab{margin:0}
.tab.active{background:var(--k-primary);color:var(--k-primary-contrast);border-color:transparent}
#qr-box{margin-top:12px;text-align:center}
#qr-img{background:#fff;padding:8px;border-radius:8px}
#sms-captcha-img{display:block;margin-top:6px;border-radius:8px;background:#fff;max-width:160px}
.tasks{display:flex;flex-wrap:wrap;gap:6px;margin-top:6px}
.dot{font-size:12px;padding:2px 8px;border-radius:999px;border:1px solid var(--k-border);color:var(--k-text-muted)}
.dot.on{background:var(--k-primary);color:var(--k-primary-contrast);border-color:transparent}
.dot.ready{border-color:var(--k-primary);color:var(--k-primary)}
@media (prefers-color-scheme:dark){html:not([data-cpamp-plugin-host]){--k-bg:#18191a;--k-surface:#242526;--k-muted:#2a2b2c;--k-border:#3a3b3c;--k-text:#e4e6eb;--k-text-muted:#b0b3b8;--k-ok-fg:#4ade80;--k-warn-fg:#fbbf24;--k-err-fg:#f87171}}
`
