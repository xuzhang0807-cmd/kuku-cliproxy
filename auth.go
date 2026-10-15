package main

// Auth: the credential shape, parsing, the (login → STOKEN → save) flow and
// refresh. Ported from kuku2api's passport.mjs / accounts.mjs / credential-refresh.mjs.
//
// Login is NOT the standard OAuth-redirect shape the host's auth.login.start/poll
// expects (URL + poll), because 库库AI logs in with a phone number / QR code, not
// a redirect. So start returns a URL to this plugin's own login page (served as a
// management resource), the operator pastes BDUSS+PTOKEN there, the plugin mints
// and verifies the STOKEN, saves it via host.auth.save, and poll then reports
// success. See management.go.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// storedAuth is the on-disk shape of a kuku credential (the file saved into the
// host auth store). Only the fields the executor needs are persisted.
type storedAuth struct {
	BDUSS     string `json:"bduss"`
	STOKEN    string `json:"stoken"`
	PTOKEN    string `json:"ptoken,omitempty"`
	UID       string `json:"uid,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	CheckedAt int64  `json:"checked_at,omitempty"`
}

// deviceID is the stable per-account device id upstream expects on the freepoint
// and chat calls. Persisted at login; derived deterministically as a fallback so
// a credential written by other tooling still works.
func (sa *storedAuth) deviceID() string {
	if sa.DeviceID != "" {
		return sa.DeviceID
	}
	seed := sa.UID
	if seed == "" {
		seed = sa.BDUSS
	}
	sum := sha256.Sum256([]byte("kuku-device:" + seed))
	n := binary.BigEndian.Uint64(sum[:8]) % 10_000_000_000
	if n == 0 {
		n = 1
	}
	return strconv.FormatInt(int64(n), 10)
}

func parseStored(raw []byte) (*storedAuth, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var sa storedAuth
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, false
	}
	// Tolerate the flat layout other tooling writes (top-level BDUSS/STOKEN).
	if sa.BDUSS == "" {
		var flat struct {
			BDUSS  string `json:"BDUSS"`
			STOKEN string `json:"STOKEN"`
			PTOKEN string `json:"PTOKEN"`
			UID    string `json:"uid"`
		}
		if json.Unmarshal(raw, &flat) == nil {
			sa.BDUSS, sa.STOKEN, sa.PTOKEN, sa.UID = flat.BDUSS, flat.STOKEN, flat.PTOKEN, flat.UID
		}
	}
	if sa.BDUSS == "" || sa.STOKEN == "" {
		return nil, false
	}
	return &sa, true
}

func (sa *storedAuth) client() *KukuClient { return newKukuClient(sa.BDUSS, sa.STOKEN) }

// accountIdentity returns a stable, filesystem-safe id for an account. It drives
// both the auth file name and the auth id, so two accounts never collide.
func accountIdentity(sa *storedAuth) string {
	if uid := strings.TrimSpace(sa.UID); uid != "" {
		return sanitizeIdentity(uid)
	}
	sum := sha256.Sum256([]byte(sa.BDUSS))
	return hex.EncodeToString(sum[:])[:12]
}

func sanitizeIdentity(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "default"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// toAuthData shapes a credential for the host. ID and FileName are per-account,
// which is what lets several kuku accounts live side by side.
func toAuthData(sa *storedAuth) pluginapi.AuthData {
	storage, _ := json.Marshal(sa)
	identity := accountIdentity(sa)
	label := "库库AI"
	if nick := strings.TrimSpace(sa.Nickname); nick != "" {
		label = "库库AI (" + nick + ")"
	} else if identity != "default" {
		label = "库库AI (" + identity + ")"
	}
	auth := pluginapi.AuthData{
		Provider:    providerName,
		ID:          providerName + "-" + identity,
		FileName:    providerName + "-" + identity + ".json",
		Prefix:      configuredModelPrefix(),
		StorageJSON: storage,
		Metadata:    map[string]any{"type": providerName},
		Label:       label,
	}
	// The host's auth list renders Attributes["email"] as the card headline;
	// AuthData has no Email field, so this is the only way in (same trick
	// workbuddy-cliproxy documents).
	if auth.Attributes == nil {
		auth.Attributes = map[string]string{}
	}
	auth.Attributes["email"] = label
	return auth
}

// -----------------------------------------------------------------------------
// Login state (state -> pending credential)
// -----------------------------------------------------------------------------

type loginCtx struct {
	expires time.Time
	ready   *storedAuth // set once the login page has minted a credential
	errMsg  string
}

var loginStates sync.Map // state(string) -> *loginCtx

func sweepLoginStates() {
	now := time.Now()
	loginStates.Range(func(k, v any) bool {
		if lc, ok := v.(*loginCtx); ok && now.After(lc.expires) {
			loginStates.Delete(k)
		}
		return true
	})
}

// markLoginReady is called by the management login route once a credential has
// been minted and saved, so auth.login.poll can report success.
func markLoginReady(state string, sa *storedAuth) {
	if v, ok := loginStates.Load(state); ok {
		if lc, ok := v.(*loginCtx); ok {
			lc.ready = sa
			return
		}
	}
	// Unknown or expired state: still record it so a late poll can succeed.
	loginStates.Store(state, &loginCtx{expires: time.Now().Add(loginTTL), ready: sa})
}

func markLoginError(state, msg string) {
	if v, ok := loginStates.Load(state); ok {
		if lc, ok := v.(*loginCtx); ok {
			lc.errMsg = msg
		}
	}
}

// -----------------------------------------------------------------------------
// Auth handlers
// -----------------------------------------------------------------------------

func handleParseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, ok := parseStored(req.RawJSON)
	if !ok {
		// Not a kuku credential; let the host try other providers.
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: toAuthData(sa)})
}

// handleStartLogin returns a URL to this plugin's login page plus an opaque
// state the page echoes back when it submits credentials.
func handleStartLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginStartRequest
	_ = json.Unmarshal(raw, &req)
	state := randUUID()
	loginStates.Store(state, &loginCtx{expires: time.Now().Add(loginTTL)})
	sweepLoginStates()

	url := resourceURL(req.BaseURL, "/login") + "?state=" + state
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       url,
		State:     state,
		ExpiresAt: time.Now().Add(loginTTL).UTC(),
		Metadata:  map[string]any{"state": state, "hint": "在打开的页面粘贴 BDUSS 与 PTOKEN 完成登录"},
	})
}

func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "missing state"})
	}
	v, ok := loginStates.Load(state)
	if !ok {
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "waiting for login"})
	}
	lc := v.(*loginCtx)
	if time.Now().After(lc.expires) {
		loginStates.Delete(state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "登录已过期，请重新发起"})
	}
	if lc.errMsg != "" {
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: lc.errMsg})
	}
	if lc.ready != nil {
		loginStates.Delete(state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: toAuthData(lc.ready)})
	}
	return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "waiting for login"})
}

// handleRefreshAuth re-mints the STOKEN from BDUSS+PTOKEN (the same mechanism
// kuku2api's credential-refresh uses) and re-verifies the login.
func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, ok := parseStored(req.StorageJSON)
	if !ok {
		return nil, fmt.Errorf("storage_parse_error: missing bduss/stoken")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	updated := *sa
	if sa.PTOKEN != "" {
		stoken, err := exchangeStoken(ctx, sa.BDUSS, sa.PTOKEN)
		if err != nil {
			return nil, fmt.Errorf("stoken refresh refused: %w", err)
		}
		updated.STOKEN = stoken
	}
	// Prove the pair actually works before persisting it.
	if err := updated.client().probe(ctx); err != nil {
		return nil, fmt.Errorf("credential verification failed: %w", err)
	}
	if uid, err := updated.client().userIdentity(ctx); err == nil && uid != "" {
		updated.UID = uid
	}
	updated.CheckedAt = time.Now().UnixMilli()

	auth := toAuthData(&updated)
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             auth,
		NextRefreshAfter: time.Now().Add(6 * time.Hour),
	})
}

// loginFromCredentials is the shared (BDUSS, PTOKEN) -> verified credential step
// used by the management login route. It mints the STOKEN when possible, else
// accepts a directly supplied STOKEN.
func loginFromCredentials(ctx context.Context, bduss, ptoken, stoken string) (*storedAuth, error) {
	bduss = strings.TrimSpace(bduss)
	ptoken = strings.TrimSpace(ptoken)
	stoken = strings.TrimSpace(stoken)
	if bduss == "" {
		return nil, fmt.Errorf("BDUSS 不能为空")
	}
	if stoken == "" {
		if ptoken == "" {
			return nil, fmt.Errorf("需要 PTOKEN（用于换取 STOKEN）或直接提供 STOKEN")
		}
		minted, err := exchangeStoken(ctx, bduss, ptoken)
		if err != nil {
			return nil, err
		}
		stoken = minted
	}
	sa := &storedAuth{BDUSS: bduss, STOKEN: stoken, PTOKEN: ptoken, CheckedAt: time.Now().UnixMilli()}
	if err := sa.client().probe(ctx); err != nil {
		return nil, fmt.Errorf("凭据校验失败：%w", err)
	}
	if uid, err := sa.client().userIdentity(ctx); err == nil {
		sa.UID = uid
	}
	sa.DeviceID = sa.deviceID()
	return sa, nil
}
