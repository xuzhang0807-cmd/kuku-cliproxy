package main

// Login-to-add-account sessions: QR scan and SMS code.
// Ported from kuku2api's src/logins.mjs (0BSD).
//
// Both flows end the same way — a (BDUSS, PTOKEN) pair that never existed in this
// process before — so both hand that pair to finishLogin, which mints the STOKEN,
// proves the pair works against 库库AI, and only then saves the credential.
// Cookies never leave this module: no route here can echo them.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	qrTTL      = 4 * time.Minute
	smsTTL     = 10 * time.Minute
	smsResend  = 60 * time.Second
	smsMaxSend = 5
)

type vcodePair struct {
	Vcodestr  string
	Vcodesign string
}

type loginSession struct {
	ID      string
	Kind    string // qr | sms
	Jar     *passportJar
	Sign    string
	Token   string
	Phone   string
	State   string // pending | sent | captcha_required | added | failed
	UID     string
	Failure string
	Created time.Time
	Expires time.Time

	Sends      int
	RetryAt    time.Time
	Vcode      *vcodePair
	LoginVcode *vcodePair

	polling bool // one upstream poll at a time; a 1s UI must not stack them
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

func sweepLogins() {
	now := time.Now()
	loginMu.Lock()
	defer loginMu.Unlock()
	for id, s := range loginSessions {
		if now.After(s.Expires) && !s.polling {
			delete(loginSessions, id)
		}
	}
}

func getLoginSession(id string) *loginSession {
	loginMu.Lock()
	defer loginMu.Unlock()
	s := loginSessions[id]
	if s == nil {
		return nil
	}
	if time.Now().After(s.Expires) && !s.polling {
		delete(loginSessions, id)
		return nil
	}
	return s
}

// finishLogin is the single tail every login flow shares. A failure is recorded
// on the session so a UI polling every second does not re-run the whole passport
// exchange once per poll for the rest of the session's TTL.
func finishLogin(ctx context.Context, s *loginSession, bduss, ptoken string) map[string]any {
	fail := func(reason string) map[string]any {
		s.State = "failed"
		s.Failure = reason
		s.Jar = nil
		s.Sign = ""
		s.Phone = ""
		s.Token = ""
		s.Vcode = nil
		s.LoginVcode = nil
		return map[string]any{"state": "failed", "message": reason}
	}
	if bduss == "" {
		return fail("登录没有返回 BDUSS")
	}
	if ptoken == "" {
		return fail("登录返回了 BDUSS 但没有 PTOKEN，无法换取 STOKEN")
	}
	sa, err := loginFromCredentials(ctx, bduss, ptoken, "")
	if err != nil {
		return fail(err.Error())
	}
	name, err := persistCredential(sa)
	if err != nil {
		return fail("保存凭据失败：" + err.Error())
	}
	s.State = "added"
	s.UID = sa.UID
	s.Jar = nil
	s.Sign = ""
	s.Phone = ""
	s.Token = ""
	s.Vcode = nil
	s.LoginVcode = nil
	s.Failure = ""
	hostLog("info", "kuku: credential added by login", map[string]any{"uid": sa.UID, "file": name, "via": s.Kind})
	return map[string]any{"state": "added", "uid": sa.UID, "file": name, "via": s.Kind}
}

// -----------------------------------------------------------------------------
// QR
// -----------------------------------------------------------------------------

func startQrLogin(ctx context.Context) (map[string]any, error) {
	sweepLogins()
	// One device cookie jar for the whole attempt: getqrcode, the unicast channel
	// and the final bdusslogin all have to look like the same browser.
	jar := newPassportJar()
	if err := seedDeviceCookie(ctx, jar); err != nil {
		return nil, err
	}
	token, _ := getApiToken(ctx, jar)
	qr, err := createQr(ctx, jar)
	if err != nil {
		return nil, err
	}
	id := "lq-" + randAlphaNum(12)
	s := &loginSession{ID: id, Kind: "qr", Jar: jar, Sign: qr.Sign, Token: token,
		State: "pending", Created: time.Now(), Expires: time.Now().Add(qrTTL)}
	loginMu.Lock()
	loginSessions[id] = s
	loginMu.Unlock()
	return map[string]any{
		"login_id":      id,
		"state":         "pending",
		"image":         dataURL("image/png", qr.ImagePNG),
		"prompt":        qr.Prompt,
		"expires_in_ms": qrTTL.Milliseconds(),
	}, nil
}

func pollQrLogin(ctx context.Context, id string) map[string]any {
	s := getLoginSession(id)
	if s == nil || s.Kind != "qr" {
		return map[string]any{"state": "unknown", "message": "登录会话不存在或已过期"}
	}
	if s.State == "added" || s.State == "failed" {
		return loginSessionView(s)
	}
	loginMu.Lock()
	if s.polling {
		loginMu.Unlock()
		return loginSessionView(s)
	}
	s.polling = true
	loginMu.Unlock()
	defer func() {
		loginMu.Lock()
		s.polling = false
		loginMu.Unlock()
	}()

	r, err := pollQr(ctx, s.Jar, s.Sign)
	if err != nil {
		return map[string]any{"state": "pending", "message": err.Error()}
	}
	if r.State != "confirmed" {
		return map[string]any{"state": r.State, "probe": r.Probe}
	}
	if _, err := bdussLogin(ctx, s.Jar, r.BDUSS); err != nil {
		return map[string]any{"state": "pending", "message": err.Error()}
	}
	// Measured: channel_v.v is a 32-char ticket, not a BDUSS. bdusslogin is what
	// materialises the real pair into this jar, so take the credentials from there
	// and only fall back to the ticket if bdusslogin handed nothing back.
	bduss := s.Jar.credential("BDUSS", "BDUSS_BFESS")
	if bduss == "" {
		bduss = r.BDUSS
	}
	return finishLogin(ctx, s, bduss, s.Jar.credential("PTOKEN", "PTOKEN_BFESS"))
}

// -----------------------------------------------------------------------------
// SMS
// -----------------------------------------------------------------------------

func startSmsLogin(ctx context.Context, phone string) map[string]any {
	sweepLogins()
	phone = strings.TrimSpace(phone)
	if !phoneRe.MatchString(phone) {
		return map[string]any{"state": "invalid_phone", "message": "请输入正确的11位中国大陆手机号"}
	}
	jar := newPassportJar()
	if err := seedDeviceCookie(ctx, jar); err != nil {
		return map[string]any{"state": "failed", "message": err.Error()}
	}
	token, err := getApiToken(ctx, jar)
	if err != nil {
		return map[string]any{"state": "failed", "message": err.Error()}
	}
	id := "ls-" + randAlphaNum(12)
	s := &loginSession{ID: id, Kind: "sms", Jar: jar, Token: token, Phone: phone,
		State: "new", Created: time.Now(), Expires: time.Now().Add(smsTTL)}
	loginMu.Lock()
	loginSessions[id] = s
	loginMu.Unlock()
	return applySmsSend(ctx, s, "", "", "")
}

func smsTiming(s *loginSession) map[string]any {
	now := time.Now()
	remaining := 0
	if s.RetryAt.After(now) {
		remaining = int(s.RetryAt.Sub(now).Seconds()) + 1
	}
	left := smsMaxSend - s.Sends
	if left < 0 {
		left = 0
	}
	return map[string]any{"login_id": s.ID, "retry_after_s": remaining, "sends_remaining": left,
		"expires_in_s": int(time.Until(s.Expires).Seconds())}
}

func applySmsSend(ctx context.Context, s *loginSession, captchaCode, captchaVcodestr, captchaVcodesign string) map[string]any {
	if s.State == "captcha_required" && strings.TrimSpace(captchaCode) == "" {
		return mergeView(s, map[string]any{"state": "captcha_required", "message": "请输入图形验证码"})
	}
	vcodeStr, vcodeSign := "", ""
	if s.Vcode != nil {
		vcodeStr, vcodeSign = s.Vcode.Vcodestr, s.Vcode.Vcodesign
	}
	r, err := sendSmsCode(ctx, s.Jar, s.Token, s.Phone, vcodeStr, vcodeSign, captchaCode)
	if err != nil {
		return mergeView(s, map[string]any{"state": "failed", "message": err.Error()})
	}
	if r.State == "captcha_required" {
		s.Vcode = &vcodePair{Vcodestr: r.Vcodestr, Vcodesign: r.Vcodesign}
		s.State = "captcha_required"
		out := map[string]any{"state": "captcha_required", "message": r.Message}
		img, imgErr := captchaDataURL(ctx, s, r.CaptchaURL)
		if img != "" {
			out["captcha_image"] = img
		} else if imgErr != "" {
			out["captcha_error"] = imgErr
		}
		return mergeView(s, out)
	}
	if r.State != "sent" {
		return mergeView(s, map[string]any{"state": r.State, "message": r.Message, "errno": r.Errno})
	}
	s.Sends++
	s.RetryAt = time.Now().Add(smsResend)
	s.LoginVcode = nil
	s.State = "sent"
	return mergeView(s, map[string]any{"state": "sent", "message": "验证码已发送到该手机号"})
}

func resendSmsLogin(ctx context.Context, id, captchaCode, captchaVcodestr, captchaVcodesign string) map[string]any {
	s := getLoginSession(id)
	if s == nil || s.Kind != "sms" {
		return map[string]any{"state": "unknown", "message": "短信登录会话不存在或已过期"}
	}
	if s.State == "added" || s.State == "failed" {
		return mergeView(s, map[string]any{"state": s.State, "message": "本次登录已结束，请重新发起"})
	}
	if s.Sends >= smsMaxSend {
		return mergeView(s, map[string]any{"state": "limit", "message": "本会话短信发送次数已达上限，请重新发起"})
	}
	if time.Now().Before(s.RetryAt) {
		return mergeView(s, map[string]any{"state": "cooldown", "message": fmt.Sprintf("请等待 %d 秒后重发", int(time.Until(s.RetryAt).Seconds())+1)})
	}
	return applySmsSend(ctx, s, captchaCode, captchaVcodestr, captchaVcodesign)
}

func verifySmsLogin(ctx context.Context, id, code, captchaCode, captchaVcodestr, captchaVcodesign string) map[string]any {
	s := getLoginSession(id)
	if s == nil || s.Kind != "sms" {
		return map[string]any{"state": "unknown", "message": "短信登录会话不存在或已过期"}
	}
	if s.State == "added" || s.State == "failed" {
		return loginSessionView(s)
	}
	code = strings.TrimSpace(code)
	if len(code) != 6 || !digitsRe.MatchString(code) {
		return mergeView(s, map[string]any{"state": "invalid_code", "message": "请输入6位数字短信验证码"})
	}
	if s.Sends == 0 {
		return mergeView(s, map[string]any{"state": "not_sent", "message": "请先发送短信验证码"})
	}
	if s.LoginVcode != nil && strings.TrimSpace(captchaCode) == "" {
		return mergeView(s, map[string]any{"state": "captcha_required", "message": "请输入登录图形验证码"})
	}
	smsStr, smsSign := "", ""
	if s.Vcode != nil {
		smsStr, smsSign = s.Vcode.Vcodestr, s.Vcode.Vcodesign
	}
	// The login captcha only carries vcodestr + the typed code (upstream sends no
	// vcodesign on this leg), unlike the send leg which needs both.
	loginStr := ""
	if s.LoginVcode != nil {
		loginStr = s.LoginVcode.Vcodestr
	}
	r, err := smsLogin(ctx, s.Jar, s.Token, s.Phone, code, smsStr, smsSign, captchaCode, loginStr)
	if err != nil {
		return mergeView(s, map[string]any{"state": "failed", "message": err.Error()})
	}
	if r.State != "logged_in" {
		if r.State != "captcha_required" {
			return mergeView(s, map[string]any{"state": r.State, "message": r.Message, "errno": r.Code})
		}
		s.LoginVcode = &vcodePair{Vcodestr: r.Vcodestr}
		out := map[string]any{"state": "captcha_required", "message": r.Message, "slide": r.Slide}
		img, imgErr := captchaDataURL(ctx, s, r.CaptchaURL)
		if img != "" {
			out["captcha_image"] = img
		} else if imgErr != "" {
			out["captcha_error"] = imgErr
		}
		return mergeView(s, out)
	}
	return finishLogin(ctx, s, r.BDUSS, r.PTOKEN)
}

// captchaDataURL fetches a graphic captcha and inlines it: the image is bound to
// this process's device cookie, so the browser cannot load it from passport.
// Returns the data URL and, when there is none, why — a silent empty result is
// what renders as a broken image in the page.
func captchaDataURL(ctx context.Context, s *loginSession, captchaURL string) (string, string) {
	buf, ctype, err := captchaImage(ctx, s.Jar, captchaURL)
	if err != nil {
		return "", err.Error()
	}
	return dataURL(ctype, buf), ""
}

func loginSessionView(s *loginSession) map[string]any {
	out := map[string]any{"state": s.State, "login_id": s.ID}
	if s.UID != "" {
		out["uid"] = s.UID
	}
	if s.Failure != "" {
		out["message"] = s.Failure
	}
	return out
}

func mergeView(s *loginSession, extra map[string]any) map[string]any {
	out := loginSessionView(s)
	for k, v := range smsTiming(s) {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
