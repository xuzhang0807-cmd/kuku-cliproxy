package main

// Upstream client for 百度库库AI (GenFlowPro).
//
// Ported from kuku2api's src/upstream.mjs (0BSD). Everything here is plain HTTP
// against kuku.baidu.com; the only credential is the (BDUSS, STOKEN) cookie
// pair. Business failures arrive as HTTP 200 with status.code != 0, so the HTTP
// status alone is never trusted.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	kukuAppID     = "123971023"
	kukuClientTyp = "401"
	kukuVersion   = "1.6.5.130"
	kukuUA        = "genflow;1.6.5;PC;PC-Windows;10.0.26200;GenFlowPro"
	kukuChannel   = "kuku_pc_genflowpro_v1"
	kukuProtocolV = "cd1755270722192e4536f605"

	requestTimeout = 30 * time.Second
	streamTimeout  = 180 * time.Second
)

// UpstreamError carries a bounded upstream code plus the flags the caller needs
// to decide whether a retry is safe. Never carries the raw body in Message.
type UpstreamError struct {
	Message             string
	Code                int64
	Status              int
	Network             bool
	InsufficientBalance bool
	RetryUnsafe         bool
	DefinitiveRefusal   bool
}

func (e *UpstreamError) Error() string { return e.Message }

func isAuthFailure(err error) bool {
	var ue *UpstreamError
	if !asUpstream(err, &ue) {
		return false
	}
	// Measured per family: /wenchain/* -> 1000004, /bizapi/* -> 200001,
	// /api/genflowpro/* -> errno -6 "未登录" (client also treats -7/-8 as auth-class).
	switch ue.Code {
	case 1000004, 200001, -6, -7, -8:
		return true
	}
	return ue.Status == 401
}

func asUpstream(err error, target **UpstreamError) bool {
	for err != nil {
		if ue, ok := err.(*UpstreamError); ok {
			*target = ue
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// KukuClient is one account's credential plus the HTTP plumbing to call it.
type KukuClient struct {
	BDUSS  string
	STOKEN string
	base   string
	http   *http.Client
}

func newKukuClient(bduss, stoken string) *KukuClient {
	return &KukuClient{BDUSS: bduss, STOKEN: stoken, base: upstreamBase()}
}

func upstreamBase() string {
	// ponytail: no env override surfaced to operators yet; add a config field
	// when a mirror is actually needed.
	return defaultUpstreamBase
}

var sharedHTTP = &http.Client{
	Timeout: 0, // per-request deadlines via context
	Transport: &http.Transport{
		MaxIdleConns:        50,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 10,
	},
}

func (c *KukuClient) headers() http.Header {
	h := http.Header{}
	h.Set("Cookie", fmt.Sprintf("BDUSS=%s; STOKEN=%s; gfprotpl=genflowpro", c.BDUSS, c.STOKEN))
	h.Set("User-Agent", kukuUA)
	h.Set("Accept", "*/*")
	h.Set("Content-Type", "application/json")
	return h
}

// logid is the per-request opaque id upstream expects in the query string.
func kukuLogid() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(89999))
	return base64.StdEncoding.EncodeToString([]byte(
		fmt.Sprintf("%d,%d", time.Now().Unix(), 1000+n.Int64())))
}

func kukuQuery(extra map[string]string) string {
	v := url.Values{}
	v.Set("app_id", kukuAppID)
	v.Set("clienttype", kukuClientTyp)
	v.Set("channel", "00000000000000000000000000000000")
	v.Set("version", kukuVersion)
	v.Set("logid", kukuLogid())
	for k, val := range extra {
		v.Set(k, val)
	}
	return v.Encode()
}

// jsonCall POSTs/GETs path and returns the inner `data` object. body/form are
// mutually exclusive. Business failure (status.code/errno != 0) becomes an error.
func (c *KukuClient) jsonCall(ctx context.Context, method, path string, body any, form url.Values, query map[string]string) (map[string]any, error) {
	full := c.base + path + "?" + kukuQuery(query)
	var reader io.Reader
	headers := c.headers()
	if form != nil {
		reader = strings.NewReader(form.Encode())
		headers.Set("Content-Type", "application/x-www-form-urlencoded")
	} else if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, full, reader)
	if err != nil {
		return nil, err
	}
	req.Header = headers
	res, err := sharedHTTP.Do(req)
	if err != nil {
		return nil, &UpstreamError{Message: "network: " + err.Error(), Network: true, Status: 502,
			RetryUnsafe: path == pathSendmsg}
	}
	defer res.Body.Close()
	text, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode >= 400 {
		return nil, &UpstreamError{Message: fmt.Sprintf("http %d", res.StatusCode), Status: res.StatusCode,
			RetryUnsafe:       path == pathSendmsg && res.StatusCode != 400 && res.StatusCode != 401 && res.StatusCode != 403 && res.StatusCode != 429,
			DefinitiveRefusal: path == pathSendmsg && (res.StatusCode == 400 || res.StatusCode == 401 || res.StatusCode == 403 || res.StatusCode == 429)}
	}
	var parsed map[string]any
	if err := json.Unmarshal(text, &parsed); err != nil {
		return nil, &UpstreamError{Message: "non-json body", Status: 502, RetryUnsafe: path == pathSendmsg}
	}
	// Two success envelopes: /wenchain/* and /bizapi/* answer {status:{code}},
	// /api/genflowpro/* answers {errno,show_msg} with no status block.
	var code int64
	var haveCode bool
	if st, ok := parsed["status"].(map[string]any); ok {
		code, haveCode = num(st["code"])
	} else if v, ok := num(parsed["errno"]); ok {
		code, haveCode = v, true
	}
	if haveCode && code != 0 {
		msg := ""
		if st, ok := parsed["status"].(map[string]any); ok {
			msg, _ = st["msg"].(string)
		}
		if msg == "" {
			msg, _ = parsed["show_msg"].(string)
		}
		refused := path == pathSendmsg
		return nil, &UpstreamError{Message: fmt.Sprintf("code=%d %s", code, msg), Code: code, Status: 502,
			DefinitiveRefusal: refused, RetryUnsafe: path == pathSendmsg && !refused}
	}
	if d, ok := parsed["data"].(map[string]any); ok {
		return d, nil
	}
	return map[string]any{}, nil
}

func num(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case json.Number:
		return t.String()
	}
	return ""
}

// -----------------------------------------------------------------------------
// Endpoints
// -----------------------------------------------------------------------------

const (
	pathIDAlloc      = "/wenchain/genflow/idallochstr"
	pathModelList    = "/wenchain/genflowpro/model/list"
	pathProbe        = "/wenchain/genflowpro/clientmessage/list"
	pathProfile      = "/api/genflowpro/settings/profile"
	pathVipRemain    = "/bizapi/gfpro/getgfvipremain"
	pathSendmsg      = "/wenchain/genflowpro/sendmsg"
	pathSSE          = "/wenchain/genflowpro/sse/getchatcontent"
	pathFreePoint    = "/api/genflowpro/freepoint/homenew"
	pathClaimReward  = "/api/genflowpro/freepoint/rewardClaim"
	pathTaskDone     = "/api/genflowpro/freepoint/taskComplete"
	pathPassportAuth = "/v3/login/api/auth"
)

type allocatedIDs struct {
	ChatID  string
	QueryID string
}

// allocateIDs reserves a chat id and a query (reply) id from upstream.
func (c *KukuClient) allocateIDs(ctx context.Context, channel string, genType int) (allocatedIDs, error) {
	d, err := c.jsonCall(ctx, http.MethodPost, pathIDAlloc,
		map[string]any{"channel": channel, "gen_type": genType}, nil, nil)
	if err != nil {
		return allocatedIDs{}, err
	}
	return allocatedIDs{ChatID: str(d["chat_id"]), QueryID: str(d["query_id"])}, nil
}

// modelList returns the raw model catalog payload.
func (c *KukuClient) modelList(ctx context.Context) (map[string]any, error) {
	return c.jsonCall(ctx, http.MethodGet, pathModelList, nil, nil, map[string]string{"channel": "genfp"})
}

// probe is the read-only liveness check: a dead credential answers "not login".
func (c *KukuClient) probe(ctx context.Context) error {
	_, err := c.jsonCall(ctx, http.MethodPost, pathProbe, map[string]any{}, nil, nil)
	return err
}

// userIdentity returns the stable upstream user id (settings/profile.uk).
func (c *KukuClient) userIdentity(ctx context.Context) (string, error) {
	d, err := c.jsonCall(ctx, http.MethodGet, pathProfile, nil, nil, nil)
	if err != nil {
		return "", err
	}
	uk := str(d["uk"])
	if uk == "" || uk == "0" || strings.HasPrefix(uk, "-") {
		return "", fmt.Errorf("invalid upstream user identity")
	}
	return uk, nil
}

// vipRemain returns the raw balance payload.
func (c *KukuClient) vipRemain(ctx context.Context) (map[string]any, error) {
	return c.jsonCall(ctx, http.MethodGet, pathVipRemain, nil, nil, nil)
}

// freePointHome lists the free-point tasks (read-only).
func (c *KukuClient) freePointHome(ctx context.Context) (map[string]any, error) {
	return c.jsonCall(ctx, http.MethodGet, pathFreePoint, nil, nil, nil)
}

// claimReward is the call that actually pays a claimable task.
func (c *KukuClient) claimReward(ctx context.Context, activityKey string, periodID int64, taskKey string) (map[string]any, error) {
	form := url.Values{}
	form.Set("activity_key", activityKey)
	form.Set("period_id", strconv.FormatInt(periodID, 10))
	form.Set("task_key", taskKey)
	return c.jsonCall(ctx, http.MethodPost, pathClaimReward, nil, form, nil)
}

// claimTask reports that a task type was performed (progress, not payment).
func (c *KukuClient) claimTask(ctx context.Context, taskType, deviceID string, autoClaim bool) (map[string]any, error) {
	form := url.Values{}
	form.Set("task_type", taskType)
	form.Set("device_id", deviceID)
	if autoClaim {
		form.Set("auto_claim", "true")
	}
	return c.jsonCall(ctx, http.MethodPost, pathTaskDone, nil, form, nil)
}

// -----------------------------------------------------------------------------
// Conversation
// -----------------------------------------------------------------------------

// sendmsgText opens one upstream turn with a plain text prompt. connectorList is
// reserved for the native MCP tool bridge (phase 2); pass nil for plain chat.
func (c *KukuClient) sendmsgText(ctx context.Context, in sendmsgInput) error {
	envelope := map[string]any{
		"type":              "message",
		"sub_type":          "chat_create",
		"client_session_id": in.ClientSessionID,
		"session_id":        in.SessionID,
		"reply_id":          in.ReplyID,
		"uk":                "",
		"cid":               0,
		"channel":           kukuChannel,
		"device_type":       mustInt(kukuClientTyp),
		"created_at":        time.Now().UnixMilli(),
		"msg_type":          1,
		"sync_type":         0,
		"sync_id":           "",
		"v":                 kukuProtocolV,
		"data": map[string]any{
			"project_type":        1,
			"text":                in.Text,
			"rich_input_params":   []any{textBlock(in.Text)},
			"fsid":                []any{},
			"quotes":              []any{},
			"skills":              []any{},
			"experts":             []any{},
			"model_name":          in.ModelName,
			"model_display_name":  in.ModelDisplayName,
			"think_mode":          in.ThinkMode,
			"show_text":           "",
			"device_id":           in.DeviceID,
			"premake_data":        map[string]any{},
			"custom_instructions": in.CustomInstructions,
			"memory_sign":         false,
			"skill_dig_sign":      false,
			"client_added": mustJSON(map[string]any{
				"client_session_id": in.ClientSessionID,
				"permission_type":   0,
				"clienttype":        mustInt(kukuClientTyp),
				"version":           kukuVersion,
				"device_os":         "windows",
			}),
		},
	}
	if len(in.ConnectorList) > 0 {
		envelope["data"].(map[string]any)["connector_list"] = in.ConnectorList
	}
	_, err := c.jsonCall(ctx, http.MethodPost, pathSendmsg, envelope, nil, nil)
	return err
}

type sendmsgInput struct {
	SessionID          string
	ReplyID            string
	ClientSessionID    string
	DeviceID           string
	Text               string
	ModelName          string
	ModelDisplayName   string
	ThinkMode          int
	CustomInstructions string
	// ConnectorList registers the native MCP connector for the tool bridge.
	ConnectorList []map[string]any
}

func textBlock(text string) map[string]any {
	return map[string]any{"id": "textId", "version": "1.0", "type": "text", "text": text,
		"data": map[string]any{"content": text}}
}

// sseFrame is one parsed Server-Sent Event.
type sseFrame struct {
	Event string
	ID    string
	Data  map[string]any
}

// chatSSE opens the conversation stream for one turn and calls onFrame for each
// parsed frame until the stream ends or onFrame returns an error.
func (c *KukuClient) chatSSE(ctx context.Context, in sendmsgInput, msgID string, onFrame func(sseFrame) error) error {
	payload := map[string]any{
		"project_type":        1,
		"text":                in.Text,
		"rich_input_params":   []any{textBlock(in.Text)},
		"fsid":                []any{},
		"quotes":              []any{},
		"skills":              []any{},
		"experts":             []any{},
		"model_name":          in.ModelName,
		"model_display_name":  in.ModelDisplayName,
		"think_mode":          in.ThinkMode,
		"show_text":           "",
		"device_id":           in.DeviceID,
		"premake_data":        map[string]any{},
		"session_id":          in.SessionID,
		"custom_instructions": in.CustomInstructions,
		"memory_sign":         false,
		"skill_dig_sign":      false,
		"msg_type":            1,
		"reply_id":            in.ReplyID,
		"client_session_id":   in.ClientSessionID,
		"traceparent":         "",
	}
	if msgID != "" {
		payload["msg_id"] = msgID
	}
	return c.streamSSE(ctx, pathSSE, payload, onFrame)
}

// streamSSE POSTs payload and parses the event-stream response frame by frame.
func (c *KukuClient) streamSSE(ctx context.Context, path string, payload any, onFrame func(sseFrame) error) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	full := c.base + path + "?" + kukuQuery(nil)
	reqCtx, cancel := context.WithTimeout(ctx, streamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, full, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header = c.headers()
	res, err := sharedHTTP.Do(req)
	if err != nil {
		return &UpstreamError{Message: "network: " + err.Error(), Network: true, Status: 502, RetryUnsafe: true}
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return &UpstreamError{Message: fmt.Sprintf("http %d", res.StatusCode), Status: res.StatusCode}
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		var code int64
		if st, ok := decodeStatus(body); ok {
			code = st
		}
		return &UpstreamError{Message: fmt.Sprintf("expected event-stream, got %s", ct), Code: code, Status: 502}
	}

	reader := bufio.NewReaderSize(res.Body, 1<<20)
	var block bytes.Buffer
	flush := func() error {
		frame, ok := parseSSEBlock(block.String())
		block.Reset()
		if !ok {
			return nil
		}
		return onFrame(frame)
	}
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				if e := flush(); e != nil {
					return e
				}
			} else {
				block.WriteString(trimmed)
				block.WriteByte('\n')
			}
		}
		if err != nil {
			if err == io.EOF {
				if block.Len() > 0 {
					return flush()
				}
				return nil
			}
			return &UpstreamError{Message: "network: " + err.Error(), Network: true, Status: 502, RetryUnsafe: true}
		}
	}
}

func decodeStatus(body []byte) (int64, bool) {
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		return 0, false
	}
	if st, ok := parsed["status"].(map[string]any); ok {
		return num(st["code"])
	}
	return num(parsed["errno"])
}

// parseSSEBlock parses one SSE event block ("event: x\ndata: {...}\n\n").
func parseSSEBlock(block string) (sseFrame, bool) {
	var frame sseFrame
	var dataLines []string
	for _, line := range strings.Split(block, "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			frame.Event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "id:"):
			frame.ID = strings.TrimSpace(line[len("id:"):])
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(line[len("data:"):]))
		}
	}
	if len(dataLines) == 0 {
		return sseFrame{}, false
	}
	joined := strings.Join(dataLines, "\n")
	var data map[string]any
	if json.Unmarshal([]byte(joined), &data) != nil {
		return sseFrame{}, false
	}
	frame.Data = data
	return frame, true
}

// frameToDelta maps an upstream frame's data.type to an OpenAI-ish delta. Kept
// deliberately close to kuku2api's frameToDelta.
type frameDelta struct {
	Content          string
	ReasoningContent string
	Usage            map[string]int64
	FinishReason     string
	ConsumePoints    *float64
	RequiresClient   bool
	CommercialReason string
	CommercialErrno  int64
	Err              map[string]any
}

func frameToDelta(data map[string]any) *frameDelta {
	if data == nil {
		return nil
	}
	typ, _ := data["type"].(string)
	inner, _ := data["data"].(map[string]any)
	switch typ {
	case "TEXT_BLOCK_DELTA":
		return &frameDelta{Content: str(inner["delta"])}
	case "THINKING_BLOCK_DELTA":
		return &frameDelta{ReasoningContent: str(inner["delta"])}
	case "MODEL_CALL_END":
		in, _ := num(inner["input_tokens"])
		out, _ := num(inner["output_tokens"])
		rt, _ := num(inner["reasoning_tokens"])
		cr, _ := num(inner["cache_read_tokens"])
		return &frameDelta{
			Usage:        map[string]int64{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out, "reasoning_tokens": rt, "cache_read_tokens": cr},
			FinishReason: mapFinish(inner),
		}
	case "ACTUAL_POINT":
		if v, ok := inner["consume_points"].(float64); ok {
			return &frameDelta{ConsumePoints: &v}
		}
		return &frameDelta{}
	case "REQUIRE_EXTERNAL_EXECUTION", "AWAITING_INPUT":
		return &frameDelta{RequiresClient: true}
	case "COMMERCIAL_NOTICE":
		return &frameDelta{CommercialReason: str(inner["reason"])}
	case "COMMERCIAL":
		e, _ := num(inner["errno"])
		return &frameDelta{CommercialReason: str(inner["reason"]), CommercialErrno: e}
	case "ERROR", "STREAM_ERROR":
		return &frameDelta{Err: inner}
	}
	return nil
}

func mapFinish(inner map[string]any) string {
	r := str(inner["finished_reason"])
	if r == "" {
		if arr, ok := inner["finish_reasons"].([]any); ok && len(arr) > 0 {
			r = str(arr[0])
		}
	}
	if r == "" || r == "completed" || r == "stop" {
		return "stop"
	}
	if r == "length" {
		return "length"
	}
	return r
}

// -----------------------------------------------------------------------------
// Native MCP control channels (phase 2)
// -----------------------------------------------------------------------------

const (
	pathControlBase    = "/wenchain/genflowpro/sse/msgexchange"
	pathControlSandbox = "/api/genflowprosse/call"
	pathNativeCallback = "/api/genflowprosse/callback"
)

// controlStream opens one of the two native control channels and calls onEnvelope
// for every parsed envelope. kind is "sandbox" or "base".
func (c *KukuClient) controlStream(ctx context.Context, kind string, payload any, onEnvelope func(map[string]any) error) error {
	path := pathControlBase
	if kind == "sandbox" {
		path = pathControlSandbox
	}
	return c.streamSSE(ctx, path, payload, func(frame sseFrame) error {
		return onEnvelope(frame.Data)
	})
}

// nativeToolResult answers one native sandbox task. errno != 0 denies the task
// (e.g. a built-in desktop tool the caller cannot serve).
func (c *KukuClient) nativeToolResult(ctx context.Context, taskID, deviceID, clientSessionID, requestID string, result any, errno int) error {
	inner := map[string]any{"logid": kukuLogid(), "errno": errno, "errmsg": "", "data": nil}
	if errno == 0 {
		inner["data"] = map[string]any{"jsonrpc": "2.0", "id": requestID, "result": result}
	} else {
		inner["errmsg"] = "This tool is not available to this client"
	}
	body := map[string]any{
		"type":              "sandbox",
		"sub_type":          "result",
		"task_id":           taskID,
		"device_id":         deviceID,
		"client_session_id": clientSessionID,
		"client_type":       mustInt(kukuClientTyp),
		"payload":           []any{mustJSON(inner)},
	}
	_, err := c.jsonCall(ctx, http.MethodPost, pathNativeCallback, body, nil, nil)
	return err
}

// closeTurn ends the upstream turn once the client tool call has been handed off,
// so the generation task is not left hanging.
func (c *KukuClient) closeTurn(ctx context.Context, sessionID, replyID, clientSessionID string) error {
	body := map[string]any{
		"type":              "message",
		"sub_type":          "close",
		"session_id":        sessionID,
		"reply_id":          replyID,
		"client_session_id": clientSessionID,
		"channel":           kukuChannel,
		"device_type":       mustInt(kukuClientTyp),
		"msg_type":          1,
		"data":              map[string]any{"session_id": sessionID, "reply_id": replyID},
	}
	_, err := c.jsonCall(ctx, http.MethodPost, pathSendmsg, body, nil, nil)
	return err
}

// -----------------------------------------------------------------------------
// Passport: mint STOKEN from (BDUSS, PTOKEN)
// -----------------------------------------------------------------------------

// engineSignKey is the key the client's native engine uses to sign the STOKEN
// exchange; without it passport answers errno 110003 even for valid cookies.
const engineSignKey = "34868b316de55815273b2616954b0286"

// engineUA is the engine's own UA; the exchange leg may be fingerprinted.
const engineUA = "genflow;1.6.5;PC;PC-Windows;10.0.26200;GenFlowPro"

func loginAuthBody(bduss, ptoken string) string {
	signed := fmt.Sprintf("appid=1&bduss=%s&ptoken=%s&return_type=1&tpl=genflowpro&tpl_list=genflowpro|netdisk&sign_key=%s",
		bduss, ptoken, engineSignKey)
	sum := md5.Sum([]byte(signed))
	sig := fmt.Sprintf("%x", sum)
	return fmt.Sprintf("appid=1&bduss=%s&ptoken=%s&return_type=1&tpl=genflowpro&tpl_list=genflowpro|netdisk&sig=%s",
		bduss, ptoken, sig)
}

// exchangeStoken turns a logged-in (BDUSS, PTOKEN) pair into the STOKEN this
// product needs. This is exactly the client's own fallback body.
func exchangeStoken(ctx context.Context, bduss, ptoken string) (string, error) {
	if bduss == "" || ptoken == "" {
		return "", fmt.Errorf("bduss and ptoken are both required")
	}
	form := loginAuthBody(bduss, ptoken)
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, defaultPassportBase+pathPassportAuth, strings.NewReader(form))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", engineUA)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("Cookie", "gfprotpl=genflowpro")
	res, err := sharedHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		return "", fmt.Errorf("stoken exchange refused: unparsable response")
	}
	errno := int64(0)
	if v, ok := num(parsed["errno"]); ok {
		errno = v
	}
	sl, _ := parsed["stoken_list"].(map[string]any)
	stoken := str(sl["genflowpro"])
	if errno != 0 || stoken == "" {
		// Never echo the raw body: passport reflects request fragments in errors.
		msg := str(parsed["errmsg"])
		if msg == "" {
			msg = str(parsed["show_msg"])
		}
		if msg == "" {
			msg = "no stoken in response"
		}
		return "", fmt.Errorf("stoken exchange refused: %s", msg)
	}
	return stoken, nil
}

func mustInt(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// randUUID returns a random RFC4122 v4-ish string for client_session_id values.
func randUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var _ = sync.Mutex{}
