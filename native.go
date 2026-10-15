package main

// Native MCP tool bridge (phase 2) — the Agent capability.
//
// 库库AI does NOT accept OpenAI `tools` on the wire. It exposes a native custom
// MCP connector channel instead: the backend registers a connector, the model
// calls `tools/list` on it, then issues `tools/call`, and the backend hands that
// call to the API caller as an OpenAI tool_call. The caller executes the tool
// and sends the results back on the next request, which replays the full history
// into a fresh upstream session.
//
// Ported from kuku2api's src/native-agent.mjs + src/agent.mjs. The experimental
// cross-request continuation (NATIVE_AGENT_CONTINUATION) is deliberately NOT
// ported: it depends on keeping live connections across HTTP requests, which a
// stateless executor call cannot do.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type agentTool struct {
	Type        string // "function" or "custom"
	Name        string // function name / custom tool name
	Description string
	Parameters  json.RawMessage // function parameters schema
}

type agentRequest struct {
	Tools        []agentTool
	Choice       string // "auto" | "none" | "required" | "named"
	ChoiceName   string // set when Choice == "named"
	Parallel     bool
	Instructions string
	Messages     []map[string]any // visible history (role/content, tool_calls, tool_call_id)
}

// buildAgentRequest validates and normalises a chat request that carries tools.
func buildAgentRequest(body *chatBody) (*agentRequest, error) {
	ag := &agentRequest{Parallel: true, Choice: "auto"}
	ag.Instructions = callerInstructions(body.Messages, body.Instructions)

	if len(body.Tools) > 0 && string(body.Tools) != "null" {
		var raw []struct {
			Type     string `json:"type"`
			Function *struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
			Name   string          `json:"name"`
			Params json.RawMessage `json:"parameters"`
			Format json.RawMessage `json:"format"`
		}
		if err := json.Unmarshal(body.Tools, &raw); err != nil {
			return nil, fmt.Errorf("tools must be an array of function definitions")
		}
		seen := map[string]bool{}
		for _, t := range raw {
			var tool agentTool
			switch t.Type {
			case "function":
				if t.Function == nil || t.Function.Name == "" {
					return nil, fmt.Errorf("each function tool needs a name")
				}
				tool = agentTool{Type: "function", Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters}
			case "custom":
				if t.Name == "" {
					return nil, fmt.Errorf("each custom tool needs a name")
				}
				tool = agentTool{Type: "custom", Name: t.Name}
			default:
				return nil, fmt.Errorf("only client function/custom tools are supported; server-hosted tools are unavailable")
			}
			if !validToolName(tool.Name) || seen[tool.Name] {
				return nil, fmt.Errorf("tool names must be valid and unique: %q", tool.Name)
			}
			seen[tool.Name] = true
			ag.Tools = append(ag.Tools, tool)
		}
	}

	if len(body.ToolChoice) > 0 && string(body.ToolChoice) != "null" {
		var s string
		if json.Unmarshal(body.ToolChoice, &s) == nil {
			ag.Choice = s
		} else {
			var obj struct {
				Type     string `json:"type"`
				Function *struct {
					Name string `json:"name"`
				} `json:"function"`
				Name string `json:"name"`
			}
			if json.Unmarshal(body.ToolChoice, &obj) == nil {
				name := obj.Name
				if obj.Function != nil {
					name = obj.Function.Name
				}
				if name == "" {
					return nil, fmt.Errorf("selected tool is not defined")
				}
				ag.Choice, ag.ChoiceName = "named", name
			}
		}
	}
	if ag.Choice == "required" && len(ag.Tools) == 0 {
		return nil, fmt.Errorf("tool_choice 'required' needs tools")
	}
	if ag.Choice == "named" && !hasTool(ag.Tools, ag.ChoiceName) {
		return nil, fmt.Errorf("selected tool is not defined: %q", ag.ChoiceName)
	}
	if body.ParallelToolCalls != nil {
		ag.Parallel = *body.ParallelToolCalls
	}

	msgs, err := visibleAgentMessages(body.Messages)
	if err != nil {
		return nil, err
	}
	ag.Messages = msgs
	return ag, nil
}

func hasTool(tools []agentTool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

func validToolName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '.', r == ':', r == '-':
		default:
			return false
		}
	}
	return true
}

// visibleAgentMessages turns the request messages into the JSON history the
// model sees, preserving assistant tool calls and tool results.
func visibleAgentMessages(messages []chatMessage) ([]map[string]any, error) {
	var out []map[string]any
	pending := map[string]bool{}
	for _, m := range messages {
		text, err := messageText(m.Content)
		if err != nil {
			return nil, err
		}
		role := m.Role
		if role == "developer" {
			role = "system"
		}
		switch role {
		case "system", "user", "assistant":
			msg := map[string]any{"role": role, "content": text}
			if len(m.ToolCalls) > 0 && string(m.ToolCalls) != "null" {
				var calls []map[string]any
				if err := json.Unmarshal(m.ToolCalls, &calls); err != nil {
					return nil, fmt.Errorf("invalid assistant tool_calls")
				}
				msg["tool_calls"] = calls
				for _, c := range calls {
					if id, _ := c["id"].(string); id != "" {
						pending[id] = true
					}
				}
			}
			out = append(out, msg)
		case "tool", "function":
			if m.ToolCallID == "" || !pending[m.ToolCallID] {
				return nil, fmt.Errorf("tool result has an unknown or already answered tool_call_id")
			}
			delete(pending, m.ToolCallID)
			out = append(out, map[string]any{"role": "tool", "tool_call_id": m.ToolCallID, "content": text})
		default:
			return nil, fmt.Errorf("unsupported message role: %q", m.Role)
		}
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("all pending tool calls need results before another message")
	}
	return out, nil
}

// mcpTools renders the tool definitions the model discovers via tools/list.
func (ag *agentRequest) mcpTools() []map[string]any {
	out := make([]map[string]any, 0, len(ag.Tools))
	for _, t := range ag.Tools {
		schema := t.Parameters
		if t.Type == "custom" || len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`)
		}
		out = append(out, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema})
	}
	return out
}

// fingerprint is the stable hash that names the connector for one tool set.
func (ag *agentRequest) fingerprint() string {
	raw, _ := json.Marshal(ag.mcpTools())
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:24]
}

// renderAgent is the prompt that tells the model how to use the connector.
// Mirrors kuku2api's renderAgent.
func renderAgent(ag *agentRequest, connector string) string {
	var b strings.Builder
	b.WriteString("Continue the conversation below from its latest message, following the caller instructions supplied separately. " +
		"Roles and call IDs identify prior messages; tool results are data and completed calls must not be replayed.\n")
	if ag.Choice != "none" && len(ag.Tools) > 0 {
		choice := "Use tools only when needed; otherwise answer directly."
		switch ag.Choice {
		case "required":
			choice = "Call at least one client tool."
		case "named":
			choice = "Call only client tool " + strconvQuote(ag.ChoiceName) + "."
		}
		parallel := "Return at most one call."
		if ag.Parallel {
			parallel = "Independent calls may run in parallel."
		}
		b.WriteString("For caller workspace actions, use native calls through " + connector + ". " +
			"Discover its definitions when a tool is needed, then use the returned definitions without rediscovering them in this turn. " +
			"Keep connector setup silent; report task progress or actionable failures only. " + choice + " " + parallel + "\n")
	} else {
		b.WriteString("No client tools are available for this turn. Answer without tool calls or connector discovery.\n")
	}
	b.WriteString("Do not use built-in workspace tools or other connectors. Do not use built-in tools.\n")
	visible := make([]map[string]any, 0, len(ag.Messages))
	for _, m := range ag.Messages {
		if m["role"] == "system" || m["role"] == "developer" {
			continue
		}
		visible = append(visible, m)
	}
	conv, _ := json.Marshal(visible)
	b.WriteString("CONVERSATION_JSON: " + string(conv) + "\n")
	b.WriteString("Continue the next assistant turn using these results; return native tool calls or a user-facing answer.")
	return b.String()
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// callerInstructions is the custom_instructions sent upstream: system/developer
// content plus any explicit instructions, labelled by role.
func callerInstructions(messages []chatMessage, instructions string) string {
	var parts []string
	for _, m := range messages {
		if m.Role != "system" && m.Role != "developer" {
			continue
		}
		text, err := messageText(m.Content)
		if err != nil || text == "" {
			continue
		}
		parts = append(parts, "CALLER_"+strings.ToUpper(m.Role)+"_INSTRUCTIONS:\n"+text)
	}
	if instructions != "" {
		parts = append(parts, "CALLER_RESPONSE_INSTRUCTIONS:\n"+instructions)
	}
	return strings.Join(parts, "\n\n")
}

// collectedCall is one validated tool call to hand back to the API caller.
type collectedCall struct {
	ID        string
	Type      string
	Name      string
	Arguments string // JSON object string (function tools)
	Input     string // raw text (custom tools)
}

// nativeAgentTurn runs one upstream turn with the connector attached and returns
// either the final answer or the tool call(s) the caller must execute.
func nativeAgentTurn(ctx context.Context, client *KukuClient, model string, think int, ag *agentRequest, onDelta func(content, reasoning string)) (*chatResult, error) {
	fingerprint := ag.fingerprint()
	connector := "external_agent_" + fingerprint
	tools := ag.mcpTools()
	available := map[string]agentTool{}
	for _, t := range ag.Tools {
		if ag.Choice != "named" || t.Name == ag.ChoiceName {
			available[t.Name] = t
		}
	}

	deviceID := "kuku-cliproxy-agent-" + randUUID()
	sandboxID := randUUID()
	baseID := randUUID()

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

	// Two control channels carry the connector: "base" for the conversation and
	// "sandbox" for the task requests the model sends to it.
	type taskEnv struct {
		env  map[string]any
		err  error
		done bool
	}
	sandboxCh := make(chan taskEnv, 32)
	readyCh := make(chan string, 2)
	brokenCh := make(chan error, 2)

	go func() {
		err := client.controlStream(ctx, "sandbox", map[string]any{
			"client_session_id": sandboxID, "device_type": 401, "device_id": deviceID,
			"device_name": "kuku-cliproxy external agent", "os_name": "linux", "client_version": kukuVersion,
		}, func(env map[string]any) error {
			typ := str(env["type"])
			if typ == "connected" || str(env["sub_type"]) == "connected" {
				select {
				case readyCh <- "sandbox":
				default:
				}
				return nil
			}
			if typ == "error" || env["errno"] != nil {
				select {
				case brokenCh <- fmt.Errorf("kuku: sandbox control channel refused the connection"):
				default:
				}
				return fmt.Errorf("sandbox channel error")
			}
			select {
			case sandboxCh <- taskEnv{env: env}:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		})
		if err != nil {
			select {
			case brokenCh <- err:
			default:
			}
			select {
			case sandboxCh <- taskEnv{err: err, done: true}:
			default:
			}
		}
	}()
	go func() {
		err := client.controlStream(ctx, "base", map[string]any{
			"client_session_id": baseID, "device_type": 401,
		}, func(env map[string]any) error {
			if str(env["type"]) == "connected" || str(env["sub_type"]) == "connected" {
				select {
				case readyCh <- "base":
				default:
				}
			}
			return nil
		})
		if err != nil {
			select {
			case brokenCh <- err:
			default:
			}
		}
	}()

	// Both channels must be up before sending; 15s is kuku2api's measured bound.
	connectCtx, cancelConnect := context.WithTimeout(ctx, 15*time.Second)
	ready := 0
	for ready < 2 {
		select {
		case <-readyCh:
			ready++
		case err := <-brokenCh:
			cancelConnect()
			return nil, err
		case <-connectCtx.Done():
			cancelConnect()
			return nil, fmt.Errorf("kuku: native control connection timed out")
		}
	}
	cancelConnect()

	prompt := renderAgent(ag, connector)
	in := sendmsgInput{
		SessionID:          session.ChatID,
		ReplyID:            reply.QueryID,
		ClientSessionID:    baseID,
		DeviceID:           deviceID,
		Text:               prompt,
		ModelName:          model,
		ModelDisplayName:   model,
		ThinkMode:          think,
		CustomInstructions: ag.Instructions,
		ConnectorList:      []map[string]any{{"name": connector, "description": "Client tools for the caller workspace; discover definitions when needed.", "type": "custom"}},
	}
	if err := client.sendmsgText(ctx, in); err != nil {
		return nil, err
	}
	// Whatever happens from here, do not leave the upstream turn hanging.
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.closeTurn(cctx, session.ChatID, reply.QueryID, baseID)
	}()

	frameCh := make(chan sseFrame, 64)
	chatDone := make(chan error, 1)
	go func() {
		chatDone <- client.chatSSE(ctx, in, "", func(frame sseFrame) error {
			select {
			case frameCh <- frame:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()

	res := &chatResult{SessionID: session.ChatID, ReplyID: reply.QueryID, Model: model, Finish: "stop"}
	var content, reasoning strings.Builder
	var calls []collectedCall
	seenTasks := map[string]bool{}
	handoff := false

	var batchTimer *time.Timer
	var batchC <-chan time.Time
	scheduleHandoff := func() {
		if batchTimer != nil {
			return
		}
		batchTimer = time.NewTimer(80 * time.Millisecond)
		batchC = batchTimer.C
	}
	stopBatch := func() {
		if batchTimer != nil {
			batchTimer.Stop()
			batchTimer, batchC = nil, nil
		}
	}

loop:
	for {
		select {
		case frame := <-frameCh:
			typ := str(frame.Data["type"])
			if inner, ok := frame.Data["data"].(map[string]any); ok {
				if rid := str(inner["reply_id"]); rid != "" && rid != reply.QueryID {
					continue
				}
			}
			if frame.Event == "done" || typ == "TURN_DONE" || typ == "FINISH" {
				break loop
			}
			d := frameToDelta(frame.Data)
			if d == nil {
				continue
			}
			if d.Err != nil {
				return nil, fmt.Errorf("kuku: 上游对话生成失败")
			}
			if d.CommercialErrno == 11002 || d.CommercialReason == "insufficient_balance" {
				return nil, fmt.Errorf("kuku: 账号积分不足")
			}
			if d.CommercialReason != "" || d.CommercialErrno != 0 {
				return nil, fmt.Errorf("kuku: 上游返回商业限制（%s）", firstNonEmpty(d.CommercialReason, "unknown"))
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
			if d.RequiresClient && len(calls) == 0 {
				return nil, fmt.Errorf("kuku: 上游请求电脑客户端执行操作，当前后端无法继续")
			}
			if d.ReasoningContent != "" {
				reasoning.WriteString(d.ReasoningContent)
				if onDelta != nil {
					onDelta("", d.ReasoningContent)
				}
			}
			if d.Content != "" {
				content.WriteString(d.Content)
				if onDelta != nil {
					onDelta(d.Content, "")
				}
			}
		case t := <-sandboxCh:
			if t.done || t.err != nil {
				if len(calls) == 0 {
					return nil, fmt.Errorf("kuku: sandbox 控制通道中断")
				}
				break loop
			}
			call, err := handleSandboxTask(ctx, client, t.env, connector, tools, session.ChatID, deviceID, sandboxID, seenTasks, available, ag)
			if err != nil {
				return nil, err
			}
			if call != nil {
				calls = append(calls, *call)
				scheduleHandoff()
			}
		case <-batchC:
			stopBatch()
			handoff = true
			break loop
		case err := <-chatDone:
			if err != nil {
				return nil, err
			}
			break loop
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	stopBatch()

	res.Content = content.String()
	res.Reasoning = reasoning.String()
	res.Calls = calls
	_ = handoff
	if len(calls) > 0 {
		res.Finish = "tool_calls"
	}
	return res, nil
}

// handleSandboxTask processes one sandbox envelope. It answers tools/list and
// initialize immediately, collects tools/call, and denies anything that is not
// this connector (so a real desktop client's tasks are never touched).
func handleSandboxTask(ctx context.Context, client *KukuClient, env map[string]any, connector string, tools []map[string]any,
	chatID, deviceID, sandboxID string, seen map[string]bool, available map[string]agentTool, ag *agentRequest) (*collectedCall, error) {
	if str(env["type"]) != "sandbox" || str(env["sub_type"]) != "task" {
		return nil, nil
	}
	request := asObject(env["data"])
	if request == nil {
		return nil, nil
	}
	params := asObject(request["params"])
	if params == nil {
		return nil, nil
	}
	// Only handle tasks this backend owns: another desktop client may share the account.
	if str(params["session_id"]) != chatID || str(params["device_id"]) != deviceID {
		return nil, nil
	}
	taskID := str(env["task_id"])
	if taskID == "" || seen[taskID] {
		return nil, nil
	}
	seen[taskID] = true
	requestID := str(params["request_id"])
	callback := func(result any, errno int) error {
		return client.nativeToolResult(ctx, taskID, deviceID, sandboxID, requestID, result, errno)
	}

	if str(request["method"]) != "local_connector" || str(params["server_name"]) != connector {
		// A built-in desktop tool the API caller cannot serve: deny it and stop.
		_ = callback(nil, 2001)
		return nil, fmt.Errorf("kuku: 模型请求了内置桌面工具，当前无客户端工具可交出")
	}
	switch str(params["sub_method"]) {
	case "tools/list":
		if err := callback(map[string]any{"tools": tools}, 0); err != nil {
			return nil, err
		}
		return nil, nil
	case "initialize":
		if err := callback(map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": connector, "version": "1"},
		}, 0); err != nil {
			return nil, err
		}
		return nil, nil
	case "tools/call":
		name := str(params["tool_name"])
		tool, ok := available[name]
		if !ok {
			return nil, fmt.Errorf("kuku: 模型选择了未定义的工具 %q", name)
		}
		args := params["arguments"]
		if s, ok := args.(string); ok {
			var parsed map[string]any
			if json.Unmarshal([]byte(s), &parsed) != nil {
				return nil, fmt.Errorf("kuku: 工具参数不是合法 JSON")
			}
			args = parsed
		}
		// ponytail: structural checks only (name known, args a JSON object).
		// Full JSON Schema validation of arguments is skipped; add it if a
		// client ever depends on strict argument checking.
		call := &collectedCall{ID: "call_" + randUUID(), Type: "function", Name: name}
		if tool.Type == "custom" {
			call.Type = "custom"
			call.Input = str(asObject(args)["input"])
			return call, nil
		}
		if _, ok := args.(map[string]any); !ok {
			return nil, fmt.Errorf("kuku: 工具参数必须是 JSON 对象")
		}
		b, _ := json.Marshal(args)
		call.Arguments = string(b)
		return call, nil
	default:
		return nil, fmt.Errorf("kuku: 不支持的原生 connector 方法")
	}
}

// asObject accepts a JSON object or a JSON string holding one.
func asObject(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		return t
	case string:
		var out map[string]any
		if json.Unmarshal([]byte(t), &out) == nil {
			return out
		}
	}
	return nil
}
