package main

// Executor: the OpenAI chat-completions surface backed by the kuku upstream.
//
// Phase 1 scope: plain text chat, streaming and non-streaming. Client tool
// calls (the native MCP connector bridge) are phase 2 and are rejected with a
// clear error rather than silently dropped — see docs in README.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// effortToThink mirrors kuku2api's EFFORT_TO_THINK.
var effortToThink = map[string]int{"minimal": 1, "low": 1, "medium": 2, "high": 3, "xhigh": 4}

type chatBody struct {
	Model           string          `json:"model"`
	Messages        []chatMessage   `json:"messages"`
	Stream          bool            `json:"stream"`
	ThinkMode       json.RawMessage `json:"think_mode"`
	ReasoningEffort string          `json:"reasoning_effort"`
	Reasoning       *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	Instructions string          `json:"instructions"`
	Tools        json.RawMessage `json:"tools"`
	ToolChoice   json.RawMessage `json:"tool_choice"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatResult struct {
	Content    string
	Reasoning  string
	Usage      map[string]int64
	Finish     string
	Consume    *float64
	SessionID  string
	ReplyID    string
	Model      string
}

// -----------------------------------------------------------------------------
// Non-streaming executor
// -----------------------------------------------------------------------------

func handleExecExecute(raw []byte) ([]byte, error) {
	req, body, sa, err := parseExecRequest(raw)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), streamTimeout+30*time.Second)
	defer cancel()

	res, err := runChat(ctx, sa, body, nil)
	if err != nil {
		return nil, err
	}
	payload := buildCompletion(req.Model, res)
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

// -----------------------------------------------------------------------------
// Streaming executor
// -----------------------------------------------------------------------------

func handleExecStream(raw []byte) ([]byte, error) {
	var req struct {
		pluginapi.ExecutorRequest
		StreamID string `json:"stream_id,omitempty"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	_, body, sa, err := parseExecRequest(raw)
	if err != nil {
		return nil, err
	}
	sseFramed := clientNeedsSSEFrame(req.Metadata)
	headers := http.Header{}
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("X-Accel-Buffering", "no")

	// No async stream id → collect synchronously.
	if req.StreamID == "" {
		ctx, cancel := context.WithTimeout(context.Background(), streamTimeout+30*time.Second)
		defer cancel()
		var chunks []pluginapi.ExecutorStreamChunk
		id := "chatcmpl-" + randUUID()
		created := time.Now().Unix()
		_, err := runChat(ctx, sa, body, func(content, reasoning string) {
			for _, delta := range deltasFor(content, reasoning) {
				chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: encodeChunk(sseFramed, id, created, req.Model, delta, "")})
			}
		})
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: encodeChunk(sseFramed, id, created, req.Model, map[string]any{}, "stop")})
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}

	// Async: return immediately; a goroutine pumps the upstream and emits chunks.
	streamWG.Add(1)
	go pumpChat(sa, body, req.Model, req.StreamID, sseFramed)
	return okEnvelope(streamResponse{Headers: headers})
}

// streamResponse is the wire shape of executor.execute_stream: the host reads
// "chunks" as a JSON array. (pluginapi.ExecutorStreamResponse uses a channel for
// in-process use, which does not marshal.)
type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

func pumpChat(sa *storedAuth, body *chatBody, model, streamID string, sseFramed bool) {
	defer streamWG.Done()
	ctx, cancel := context.WithTimeout(context.Background(), streamTimeout+30*time.Second)
	defer cancel()
	id := "chatcmpl-" + randUUID()
	created := time.Now().Unix()
	aborted := false
	_, err := runChat(ctx, sa, body, func(content, reasoning string) {
		if aborted {
			return
		}
		for _, delta := range deltasFor(content, reasoning) {
			if emitErr := streamEmit(streamID, encodeChunk(sseFramed, id, created, model, delta, "")); emitErr != nil {
				aborted = true
				return
			}
		}
	})
	if err != nil {
		streamEmitError(streamID, err.Error())
		streamClose(streamID)
		return
	}
	if !aborted {
		_ = streamEmit(streamID, encodeChunk(sseFramed, id, created, model, map[string]any{}, "stop"))
	}
	streamClose(streamID)
}

// deltasFor splits a frame's content/reasoning into OpenAI delta objects.
func deltasFor(content, reasoning string) []map[string]any {
	var out []map[string]any
	if reasoning != "" {
		out = append(out, map[string]any{"reasoning_content": reasoning})
	}
	if content != "" {
		out = append(out, map[string]any{"content": content})
	}
	return out
}

func encodeChunk(sseFramed bool, id string, created int64, model string, delta map[string]any, finish string) []byte {
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishOrNil(finish)}},
	}
	b, _ := json.Marshal(chunk)
	if sseFramed {
		return append(append([]byte("data: "), b...), '\n', '\n')
	}
	return b
}

func finishOrNil(finish string) any {
	if finish == "" {
		return nil
	}
	return finish
}

func buildCompletion(model string, res *chatResult) []byte {
	usage := map[string]int64{}
	for k, v := range res.Usage {
		usage[k] = v
	}
	obj := map[string]any{
		"id":      "chatcmpl-" + randUUID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": res.Content},
			"finish_reason": firstNonEmpty(res.Finish, "stop"),
		}},
		"usage": usage,
	}
	b, _ := json.Marshal(obj)
	return b
}

// -----------------------------------------------------------------------------
// Shared request parsing and the upstream turn
// -----------------------------------------------------------------------------

func parseExecRequest(raw []byte) (pluginapi.ExecutorRequest, *chatBody, *storedAuth, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, nil, nil, err
	}
	sa, ok := parseStored(req.StorageJSON)
	if !ok {
		return req, nil, nil, fmt.Errorf("kuku credential is missing or unreadable")
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = req.OriginalRequest
	}
	var body chatBody
	if err := json.Unmarshal(payload, &body); err != nil {
		return req, nil, nil, fmt.Errorf("invalid chat request body: %w", err)
	}
	return req, &body, sa, nil
}

// runChat performs one full upstream turn. onDelta (optional) receives content
// and reasoning increments as they arrive.
func runChat(ctx context.Context, sa *storedAuth, body *chatBody, onDelta func(content, reasoning string)) (*chatResult, error) {
	if hasTools(body) {
		return nil, fmt.Errorf("kuku: 客户端工具调用（原生 MCP 桥接）尚未实现，请先关闭 tools 或使用纯文本对话")
	}
	modelID := strings.TrimSpace(stripModelPrefix(body.Model))
	if modelID == "" {
		modelID = strings.TrimSpace(stripModelPrefix(""))
	}
	text, customInstructions, err := buildPrompt(body)
	if err != nil {
		return nil, err
	}
	think := resolveThink(body)

	client := sa.client()
	session, err := client.allocateIDs(ctx, "genfp", 3)
	if err != nil {
		return nil, err
	}
	reply, err := client.allocateIDs(ctx, "genfp", 3)
	if err != nil {
		return nil, err
	}
	if session.ChatID == "" || reply.QueryID == "" {
		return nil, fmt.Errorf("kuku: upstream did not return a session id")
	}

	in := sendmsgInput{
		SessionID:          session.ChatID,
		ReplyID:            reply.QueryID,
		ClientSessionID:    randUUID(),
		DeviceID:           "",
		Text:               text,
		ModelName:          modelID,
		ModelDisplayName:   modelID,
		ThinkMode:          think,
		CustomInstructions: customInstructions,
	}
	if err := client.sendmsgText(ctx, in); err != nil {
		return nil, err
	}

	res := &chatResult{SessionID: session.ChatID, ReplyID: reply.QueryID, Model: modelID, Finish: "stop"}
	completed := false
	err = client.chatSSE(ctx, in, "", func(frame sseFrame) error {
		typ := str(frame.Data["type"])
		if inner, ok := frame.Data["data"].(map[string]any); ok {
			if rid := str(inner["reply_id"]); rid != "" && rid != reply.QueryID {
				return nil // a different turn on the same session
			}
		}
		if frame.Event == "done" || ((typ == "TURN_DONE" || typ == "FINISH") && true) {
			completed = true
			return errStopStream
		}
		d := frameToDelta(frame.Data)
		if d == nil {
			return nil
		}
		if d.Err != nil {
			return fmt.Errorf("kuku: 上游对话生成失败")
		}
		if d.CommercialReason != "" || d.CommercialErrno != 0 {
			if d.CommercialErrno == 11002 || d.CommercialReason == "insufficient_balance" {
				return fmt.Errorf("kuku: 账号积分不足")
			}
			return fmt.Errorf("kuku: 上游返回商业限制（%s）", firstNonEmpty(d.CommercialReason, "unknown"))
		}
		if d.RequiresClient {
			return fmt.Errorf("kuku: 上游请求电脑客户端执行操作，当前后端无法继续")
		}
		if d.Usage != nil {
			res.Usage = d.Usage
		}
		if d.FinishReason != "" {
			res.Finish = d.FinishReason
		}
		if d.ConsumePoints != nil {
			res.Consume = d.ConsumePoints
		}
		if d.ReasoningContent != "" {
			res.Reasoning += d.ReasoningContent
			if onDelta != nil {
				onDelta("", d.ReasoningContent)
			}
		}
		if d.Content != "" {
			res.Content += d.Content
			if onDelta != nil {
				onDelta(d.Content, "")
			}
		}
		return nil
	})
	if err != nil && err != errStopStream {
		return nil, err
	}
	if !completed && err == nil {
		// Stream ended without a terminal frame: the turn is unconfirmed.
		return nil, fmt.Errorf("kuku: 上游对话流中断，未确认完成")
	}
	return res, nil
}

// errStopStream is the sentinel a frame handler returns to end a stream cleanly.
var errStopStream = fmt.Errorf("stop stream")

func hasTools(body *chatBody) bool {
	if len(body.Tools) > 0 && string(body.Tools) != "null" && string(body.Tools) != "[]" {
		return true
	}
	for _, m := range body.Messages {
		if m.Role == "tool" || m.Role == "function" || len(m.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// resolveThink applies kuku2api's precedence: explicit think_mode wins, then
// reasoning.effort / reasoning_effort. Returns 0 when unspecified (upstream default).
func resolveThink(body *chatBody) int {
	if len(body.ThinkMode) > 0 && string(body.ThinkMode) != "null" {
		var n int
		if json.Unmarshal(body.ThinkMode, &n) == nil {
			return n
		}
	}
	effort := body.ReasoningEffort
	if effort == "" && body.Reasoning != nil {
		effort = body.Reasoning.Effort
	}
	if effort != "" {
		if v, ok := effortToThink[effort]; ok {
			return v
		}
	}
	return 0
}

// buildPrompt returns the text sent to upstream and the custom instructions.
// Mirrors kuku2api's plain-chat path: a fresh session sends the whole
// conversation as JSON, a single message sends just that message.
func buildPrompt(body *chatBody) (string, string, error) {
	if len(body.Messages) == 0 {
		return "", "", fmt.Errorf("messages must be a non-empty array")
	}
	var visible []map[string]string
	var instructions []string
	lastUser := ""
	for _, m := range body.Messages {
		text, err := messageText(m.Content)
		if err != nil {
			return "", "", err
		}
		switch m.Role {
		case "system", "developer":
			if text != "" {
				instructions = append(instructions, text)
			}
		case "user":
			if text != "" {
				lastUser = text
			}
		}
		if text != "" {
			visible = append(visible, map[string]string{"role": m.Role, "content": text})
		}
	}
	if strings.TrimSpace(lastUser) == "" {
		return "", "", fmt.Errorf("a non-empty user message is required")
	}
	if body.Instructions != "" {
		instructions = append(instructions, body.Instructions)
	}

	var text string
	if len(visible) > 1 {
		conv, _ := json.Marshal(visible)
		text = "Answer the latest user message using this complete conversation and its system/developer instructions. Do not assume files exist on this computer.\nCONVERSATION_JSON: " + string(conv)
	} else {
		text = lastUser
	}
	return text, strings.Join(instructions, "\n\n"), nil
}

// messageText extracts plain text from a chat message content field (string or
// array of {type:"text",text}). Images/other parts are rejected.
func messageText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("unsupported message content")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" && p.Type != "input_text" && p.Type != "output_text" {
			return "", fmt.Errorf("kuku: 目前仅支持文本内容，图片/文件附件尚未实现")
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

// clientNeedsSSEFrame reports whether chunk payloads must carry their own
// "data: " framing. CPA's chat-completions passthrough adds the prefix itself,
// but cross-format translators only consume already-framed payloads.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}
