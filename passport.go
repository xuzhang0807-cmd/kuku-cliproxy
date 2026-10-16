package main

// Passport (passport.baidu.com) login client: QR scan and SMS code.
// Ported from kuku2api's src/passport.mjs + src/logins.mjs (0BSD).
//
// Why this exists: STOKEN and PTOKEN are httpOnly, so they cannot be copied out
// of DevTools. The only scalable way to add an account is to complete a real
// login here and read the cookies off the response. Cookies never leave this
// process: no route echoes them, only a masked uid comes back.

import (
	"context"
	"crypto/md5"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const passportUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) GenFlowPro/1.6.5.130 Chrome/120.0.0.0 Electron/28.0.0.0 Safari/537.36"

func passportBase() string {
	if v := strings.TrimSpace(os.Getenv("KUKU_PASSPORT_BASE")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://passport.baidu.com"
}

// -----------------------------------------------------------------------------
// Cookie jar
// -----------------------------------------------------------------------------

// passportJar is one login attempt's cookie store. Two people scanning at the
// same time must not share a session, so a jar is never reused across attempts.
type passportJar struct {
	mu      sync.Mutex
	cookies map[string]string
}

func newPassportJar() *passportJar { return &passportJar{cookies: map[string]string{}} }

func (j *passportJar) header() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	keys := make([]string, 0, len(j.cookies))
	for k := range j.cookies {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+j.cookies[k])
	}
	return strings.Join(parts, "; ")
}

func (j *passportJar) get(k string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cookies[k]
}

func (j *passportJar) has(k string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, ok := j.cookies[k]
	return ok
}

func (j *passportJar) set(k, v string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cookies[k] = v
}

func (j *passportJar) absorb(res *http.Response) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range res.Cookies() {
		// An expired cookie must delete, not store: passport clears its session
		// cookies that way and keeping the stale value reuses a dead login.
		if c.Value == "" || c.MaxAge < 0 {
			delete(j.cookies, c.Name)
			continue
		}
		j.cookies[c.Name] = c.Value
	}
}

// credential returns the account-scoped cookie, preferring the un-suffixed name.
func (j *passportJar) credential(names ...string) string {
	for _, n := range names {
		if v := j.get(n); v != "" {
			return v
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// HTTP
// -----------------------------------------------------------------------------

type passportReply struct {
	Status int
	JSON   map[string]any
	Text   string
	Bytes  []byte
}

func passportCall(ctx context.Context, method, path string, jar *passportJar, form url.Values, headers map[string]string, raw bool) (*passportReply, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, passportBase()+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", passportUA)
	// passport hotlink-checks /v2/api/?getapi: without a Referer it answers 404
	// HTML instead of the JSON the flow needs, which reads as "endpoint missing".
	req.Header.Set("Referer", "https://kuku.baidu.com/genflowpro/login")
	if raw {
		req.Header.Set("Accept", "image/png,image/*;q=0.5")
	} else {
		req.Header.Set("Accept", "application/json, text/plain, */*")
	}
	// An empty Cookie header is its own 404 trigger on some passport routes.
	if jar != nil {
		if h := jar.header(); h != "" {
			req.Header.Set("Cookie", h)
		}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := passportHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("passport 无法访问：%w", err)
	}
	defer res.Body.Close()
	if jar != nil {
		jar.absorb(res)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("passport 响应读取失败：%w", err)
	}
	reply := &passportReply{Status: res.StatusCode, Text: string(data)}
	if raw {
		reply.Bytes = data
		return reply, nil
	}
	var obj map[string]any
	if json.Unmarshal(data, &obj) == nil {
		reply.JSON = obj
	}
	return reply, nil
}

var passportHTTP = &http.Client{Timeout: 30 * time.Second}

// -----------------------------------------------------------------------------
// JSONP
// -----------------------------------------------------------------------------

var jsonpRe = regexp.MustCompile(`(?s)^[^(]*\((.*)\)\s*;?\s*$`)
var jsKeyRe = regexp.MustCompile(`([,{\s])'([^']+)'\s*:`)
var jsLabelRe = regexp.MustCompile(`^\s*[A-Za-z_$][\w$]*\s*:\s*`)

func jsonpName() string { return "bd__cbs__" + randAlphaNum(7) }

// unwrapJSONP pulls the payload out of a passport JSONP reply. passport sometimes
// answers with *JS* rather than JSON (`loginrecord:{ 'email':[] }`), so the
// leading label is dropped and single-quoted keys normalised; upstream code is
// never executed.
func unwrapJSONP(text string) map[string]any {
	body := text
	if m := jsonpRe.FindStringSubmatch(text); m != nil {
		body = m[1]
	}
	var obj map[string]any
	if json.Unmarshal([]byte(body), &obj) == nil {
		return obj
	}
	body = jsLabelRe.ReplaceAllString(body, "")
	body = jsKeyRe.ReplaceAllString(body, `$1"$2":`)
	if json.Unmarshal([]byte(body), &obj) == nil {
		return obj
	}
	return nil
}

// parseLoginReply reads a ?login response. Form submissions answer with an HTML
// redirect script, so only the literal callback query is read; upstream JS is
// never executed and its URLs are never followed.
var loginHrefRe = regexp.MustCompile(`\bhref\s*\+=\s*("(?:\\.|[^"\\])*")`)
var intRe = regexp.MustCompile(`^-?\d+$`)

func parseLoginReply(jsonObj map[string]any, text string) map[string]any {
	if jsonObj != nil {
		return jsonObj
	}
	if direct := unwrapJSONP(text); direct != nil {
		return direct
	}
	for _, m := range loginHrefRe.FindAllStringSubmatch(text, -1) {
		var raw string
		if json.Unmarshal([]byte(m[1]), &raw) != nil {
			continue
		}
		q, err := url.ParseQuery(strings.TrimPrefix(raw, "?"))
		if err != nil {
			continue
		}
		if !intRe.MatchString(q.Get("err_no")) {
			continue
		}
		return map[string]any{
			"errInfo": map[string]any{"no": q.Get("err_no")},
			"data": map[string]any{
				"codeString": q.Get("codeString"),
				"isslide":    q.Get("isslide"),
				"msg":        firstNonEmpty(q.Get("err_msg"), q.Get("msg")),
			},
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// QR login
// -----------------------------------------------------------------------------

// seedDeviceCookie gives the jar a BAIDUID. Without a device cookie passport
// hands out a token field containing an error string instead of a real one.
func seedDeviceCookie(ctx context.Context, jar *passportJar) error {
	if jar.has("BAIDUID") || jar.has("BAIDUID_BFESS") {
		return nil
	}
	_, err := passportCall(ctx, http.MethodGet, "/", jar, nil, nil, false)
	return err
}

// getApiToken fetches the anti-CSRF token every later login call carries.
func getApiToken(ctx context.Context, jar *passportJar) (string, error) {
	if err := seedDeviceCookie(ctx, jar); err != nil {
		return "", err
	}
	q := url.Values{"apiver": {"v3"}, "tpl": {"genflowpro"}, "tt": {nowMillis()},
		"apiType": {"login"}, "loginType": {"basicLogin"}, "callback": {jsonpName()}}
	reply, err := passportCall(ctx, http.MethodGet, "/v2/api/?getapi&"+q.Encode(), jar, nil, nil, false)
	if err != nil {
		return "", err
	}
	parsed := reply.JSON
	if parsed == nil {
		parsed = unwrapJSONP(reply.Text)
	}
	token := str(asObject(parsed["data"])["token"])
	if !hexTokenRe.MatchString(token) {
		return "", fmt.Errorf("passport 未返回登录令牌（http %d）", reply.Status)
	}
	return token, nil
}

var hexTokenRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

type qrIssue struct {
	Sign     string
	ImgURL   string
	Prompt   string
	ImagePNG []byte
}

// createQr issues a login QR. Works with no credentials at all, so nothing about
// an existing account is touched here.
func createQr(ctx context.Context, jar *passportJar) (*qrIssue, error) {
	q := url.Values{"lp": {"pc"}, "qrloginfrom": {"pc"}, "gid": {""}, "oauthLog": {""},
		"apiver": {"v3"}, "tpl": {"genflowpro"}, "tt": {nowMillis()}, "callback": {jsonpName()}}
	reply, err := passportCall(ctx, http.MethodGet, "/v2/api/getqrcode?"+q.Encode(), jar, nil, nil, false)
	if err != nil {
		return nil, err
	}
	parsed := reply.JSON
	if parsed == nil {
		parsed = unwrapJSONP(reply.Text)
	}
	sign := str(parsed["sign"])
	if sign == "" || numOr(parsed["errno"], -1) != 0 {
		return nil, fmt.Errorf("二维码申请失败（errno %v）", parsed["errno"])
	}
	issue := &qrIssue{Sign: sign, ImgURL: "https://" + str(parsed["imgurl"]), Prompt: str(parsed["prompt"])}
	png, err := qrImage(ctx, jar, sign)
	if err != nil {
		return nil, err
	}
	issue.ImagePNG = png
	return issue, nil
}

// qrImage fetches the QR bitmap. Measured against the real passport: the fetch
// taken immediately after getqrcode can answer 200 with a zero-length body, and
// the same sign serves a real PNG seconds later.
func qrImage(ctx context.Context, jar *passportJar, sign string) ([]byte, error) {
	q := url.Values{"sign": {sign}, "lp": {"pc"}, "qrloginfrom": {"pc"}}
	path := "/v2/api/qrcode?" + q.Encode()
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		reply, err := passportCall(ctx, http.MethodGet, path, jar, nil, nil, true)
		if err != nil {
			last = err
		} else if reply.Status != http.StatusOK {
			last = fmt.Errorf("二维码图片 http %d", reply.Status)
		} else if len(reply.Bytes) > 0 {
			return reply.Bytes, nil
		} else {
			last = fmt.Errorf("二维码图片为空")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(250*(attempt+1)) * time.Millisecond):
		}
	}
	return nil, last
}

type qrPoll struct {
	State  string // pending | scanned | confirmed | expired
	BDUSS  string
	Target string
	Probe  string
}

// pollQr reads one tick of the unicast channel. The widget does not re-read
// getqrcode for this: it opens a channel keyed by the sign and reads
// `channel_v`, a *stringified* object whose status is "0" confirmed, "1"
// scanned, "2" expired.
func pollQr(ctx context.Context, jar *passportJar, sign string) (*qrPoll, error) {
	q := url.Values{"channel_id": {sign}, "gid": {""}, "_sdkFrom": {"1"},
		"apiver": {"v3"}, "tpl": {"genflowpro"}, "tt": {nowMillis()}, "callback": {jsonpName()}}
	reply, err := passportCall(ctx, http.MethodGet, "/channel/unicast?"+q.Encode(), jar, nil, nil, false)
	if err != nil {
		return nil, err
	}
	parsed := reply.JSON
	if parsed == nil {
		parsed = unwrapJSONP(reply.Text)
	}
	if parsed == nil {
		return nil, fmt.Errorf("二维码轮询返回了非 JSON（http %d）", reply.Status)
	}
	return parseQrChannel(parsed), nil
}

// parseQrChannel decodes one unicast payload. The widget does not re-read
// getqrcode for this: it reads `channel_v`, a *stringified* object whose status
// is "0" confirmed (then `v` carries a fresh ticket), "1" scanned, "2" expired.
// Anything else is reported verbatim rather than guessed at — a wrong guess here
// is how a login flow ships looking broken when it is only unfamiliar.
func parseQrChannel(parsed map[string]any) *qrPoll {
	ch := asObject(parsed["channel_v"])
	if ch == nil {
		if s, ok := parsed["channel_v"].(string); ok {
			ch = asObject(s)
		}
	}
	errno := numOr(parsed["errno"], -1)
	status := ""
	if ch != nil {
		status = str(ch["status"])
	}
	probe := fmt.Sprintf("errno=%d status=%s", errno, status)
	switch {
	case errno == 0 && status == "0" && str(ch["v"]) != "":
		return &qrPoll{State: "confirmed", BDUSS: str(ch["v"]), Target: str(ch["u"]), Probe: probe}
	case errno == 0 && status == "1":
		return &qrPoll{State: "scanned", Probe: probe}
	case errno == 0 && status == "2":
		return &qrPoll{State: "expired", Probe: probe}
	default:
		return &qrPoll{State: "pending", Probe: probe}
	}
}

// bdussLogin materialises the phone-approved login into the jar. Without it
// passport knows the QR was scanned but never hands the session cookies to the
// polling client.
func bdussLogin(ctx context.Context, jar *passportJar, bduss string) (*passportReply, error) {
	q := url.Values{"bduss": {bduss}, "u": {"https://kuku.baidu.com"}, "qrcode": {"1"},
		"apiver": {"v3"}, "tpl": {"genflowpro"}, "tt": {nowMillis()}, "callback": {jsonpName()}}
	return passportCall(ctx, http.MethodGet, "/v2/api/bdusslogin?"+q.Encode(), jar, nil, nil, false)
}

// -----------------------------------------------------------------------------
// SMS login
// -----------------------------------------------------------------------------

var phoneRe = regexp.MustCompile(`^1[3-9]\d{9}$`)
var digitsRe = regexp.MustCompile(`^\d+$`)

type smsSend struct {
	State      string // sent | captcha_required | unregistered | refused
	Errno      int64
	Message    string
	Vcodestr   string
	Vcodesign  string
	CaptchaURL string
}

// moonshad is reproduced from the widget (lt.js:387558): md5(phone+"Moonshadow")
// then five single (non-global) letter doublings. It looks like obfuscation and
// it is, but senddpass checks it.
func moonshad(phone string) string {
	sum := md5.Sum([]byte(phone + "Moonshadow"))
	out := hex.EncodeToString(sum[:])
	for _, p := range [][2]string{{"o", "ow"}, {"d", "do"}, {"a", "ad"}, {"h", "ha"}, {"s", "sh"}, {"n", "ns"}, {"m", "mo"}} {
		out = strings.Replace(out, p[0], p[1], 1)
	}
	return out
}

func sendSmsCode(ctx context.Context, jar *passportJar, token, phone, vcodestr, vcodesign, verifycode string) (*smsSend, error) {
	if !regexp.MustCompile(`^\d{6,15}$`).MatchString(phone) {
		return nil, fmt.Errorf("手机号格式不正确")
	}
	p := url.Values{
		"gid": {""}, "username": {phone}, "countrycode": {""}, "bdstoken": {token},
		"flag_code": {"0"}, "client": {""}, "mkey": {""}, "moonshad": {moonshad(phone)},
		"supportdv": {"1"}, "dv": {""}, "apiver": {"v3"}, "tpl": {"genflowpro"},
		"tt": {nowMillis()}, "callback": {jsonpName()},
	}
	if vcodestr != "" {
		p.Set("vcodestr", vcodestr)
		p.Set("vcodesign", vcodesign)
		p.Set("verifycode", verifycode)
	}
	reply, err := passportCall(ctx, http.MethodGet, "/v2/api/senddpass?"+p.Encode(), jar, nil, nil, false)
	if err != nil {
		return nil, err
	}
	parsed := reply.JSON
	if parsed == nil {
		parsed = unwrapJSONP(reply.Text)
	}
	if parsed == nil {
		return nil, fmt.Errorf("发送验证码失败（http %d）", reply.Status)
	}
	d := asObject(parsed["data"])
	if d == nil {
		d = parsed
	}
	errno := numOr(firstAny(parsed["errno"], d["errno"]), -1)
	out := &smsSend{Errno: errno, Message: firstNonEmpty(str(parsed["msg"]), str(d["msg"]))}
	switch errno {
	case 18, 19, 21:
		out.State = "captcha_required"
		out.Vcodestr = firstNonEmpty(str(d["vcodestr"]), str(parsed["vcodestr"]))
		out.Vcodesign = firstNonEmpty(str(d["vcodesign"]), str(parsed["vcodesign"]))
		if out.Vcodestr != "" {
			out.CaptchaURL = passportBase() + "/cgi-bin/genimage?" + out.Vcodestr
		}
	case 27:
		out.State = "unregistered"
	case 0:
		out.State = "sent"
	default:
		out.State = "refused"
	}
	return out, nil
}

type smsResult struct {
	State      string // logged_in | captcha_required | refused
	Code       int64
	Message    string
	BDUSS      string
	PTOKEN     string
	Vcodestr   string
	CaptchaURL string
	Slide      bool
}

func smsLogin(ctx context.Context, jar *passportJar, token, phone, code, smsVcodestr, smsVcodesign, captchaCode, captchaVcodestr string) (*smsResult, error) {
	form := url.Values{
		"username": {phone}, "password": {code}, "smsCodeString": {code}, "isdpass": {"1"},
		"smsVcodestr": {smsVcodestr}, "smsVcodesign": {smsVcodesign},
		"switchuname": {""}, "subpro": {""}, "idc": {""}, "gid": {""}, "mkey": {""},
		"is_voice_sms": {"0"}, "voice_sms_flag": {"0"}, "u": {"https://kuku.baidu.com"},
		"staticPage": {"https://kuku.baidu.com/m-static/genflow-login/thirdparty/pass_v3_jump.html"},
		"loginMerge": {"1"}, "memberPass": {"true"}, "isPhone": {"1"}, "charset": {"utf8"},
		"token": {token}, "apiver": {"v3"}, "tpl": {"genflowpro"}, "tt": {nowMillis()},
	}
	if captchaCode != "" {
		form.Set("codestring", captchaVcodestr)
		form.Set("verifycode", captchaCode)
	}
	reply, err := passportCall(ctx, http.MethodPost, "/v2/api/?login", jar, form, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded;charset=UTF-8",
		"Origin":       passportBase(),
		"Referer":      passportBase() + "/v2/?logintype=sms",
	}, false)
	if err != nil {
		return nil, err
	}
	if reply.Status < 200 || reply.Status >= 300 {
		return nil, fmt.Errorf("百度登录接口 HTTP %d", reply.Status)
	}
	parsed := parseLoginReply(reply.JSON, reply.Text)
	rawCode := ""
	if info := asObject(parsed["errInfo"]); info != nil {
		rawCode = firstNonEmpty(str(info["no"]), str(info["errno"]))
	}
	if rawCode == "" {
		rawCode = str(parsed["errno"])
	}
	if !intRe.MatchString(rawCode) {
		return nil, fmt.Errorf("无法解析百度登录响应（HTTP %d），请稍后重试或改用百度 App 扫码登录", reply.Status)
	}
	no := int64(mustInt(rawCode))
	if no != 0 {
		d := asObject(parsed["data"])
		vc := firstNonEmpty(str(d["codeString"]), str(d["verifyStr"]))
		slide := str(d["isslide"]) == "1"
		msg := firstNonEmpty(str(d["msg"]), str(asObject(parsed["errInfo"])["msg"]), str(parsed["msg"]),
			fmt.Sprintf("百度登录未通过（错误码 %d），请按上游提示完成验证或改用百度 App 扫码登录", no))
		res := &smsResult{Code: no, Message: msg, Slide: slide}
		if vc != "" || slide {
			res.State = "captcha_required"
			res.Vcodestr = vc
			if vc != "" {
				res.CaptchaURL = passportBase() + "/cgi-bin/genimage?" + vc
			}
		} else {
			res.State = "refused"
		}
		return res, nil
	}
	bduss := jar.credential("BDUSS", "BDUSS_BFESS")
	if bduss == "" {
		return nil, fmt.Errorf("登录已成功但未返回 BDUSS")
	}
	return &smsResult{State: "logged_in", BDUSS: bduss, PTOKEN: jar.credential("PTOKEN", "PTOKEN_BFESS")}, nil
}

// captchaImage fetches the graphic captcha. It must be served by us: passport
// binds the image to this process's device cookie, so loading it in the
// operator's browser would produce a code that never matches.
func captchaImage(ctx context.Context, jar *passportJar, captchaURL string) ([]byte, string, error) {
	reply, err := passportCall(ctx, http.MethodGet, captchaURL, jar, nil, nil, true)
	if err != nil {
		return nil, "", err
	}
	if reply.Status != http.StatusOK {
		return nil, "", fmt.Errorf("图形验证码 http %d", reply.Status)
	}
	return reply.Bytes, "image/jpeg", nil
}

func dataURL(contentType string, data []byte) string {
	if len(data) == 0 {
		return ""
	}
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func firstAny(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func numOr(v any, fallback int64) int64 {
	if n, ok := num(v); ok {
		return n
	}
	return fallback
}

func nowMillis() string { return fmt.Sprint(time.Now().UnixMilli()) }

func randAlphaNum(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, n)
	if _, err := crand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = byte(time.Now().UnixNano() >> (i % 8))
		}
	}
	for i, b := range buf {
		buf[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(buf)
}
