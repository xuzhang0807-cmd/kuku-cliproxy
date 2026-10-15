package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFlattenTasks(t *testing.T) {
	payload := map[string]any{"activities": []any{
		map[string]any{"activity_key": "act", "period_no": float64(7), "tabs": []any{
			map[string]any{"tasks": []any{
				map[string]any{"task_key": "k1", "task_type": "LOGIN", "task_status": "FINISHED",
					"claimable_point": float64(10), "single_reward_point": float64(10)},
				map[string]any{"task_key": "k2", "task_type": "CHAT", "task_status": "DOING",
					"claimable_point": "0"},
			}},
		}},
	}}
	tasks := flattenTasks(payload)
	if len(tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(tasks))
	}
	if tasks[0].ActivityKey != "act" || tasks[0].PeriodID != 7 || tasks[0].TaskKey != "k1" {
		t.Fatalf("bad first task: %+v", tasks[0])
	}
	if tasks[1].Claimable != 0 || tasks[1].TaskType != "CHAT" {
		t.Fatalf("bad second task: %+v", tasks[1])
	}
	// A missing/garbage payload must not panic and must yield nothing claimable.
	if got := flattenTasks(map[string]any{"activities": "nope"}); got != nil {
		t.Fatalf("expected nil for malformed payload, got %+v", got)
	}
}

func TestBuildAgentRequestValidates(t *testing.T) {
	body := &chatBody{
		Tools: json.RawMessage(`[{"type":"function","function":{"name":"shell","description":"run","parameters":{"type":"object"}}}]`),
		Messages: []chatMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
	}
	ag, err := buildAgentRequest(body)
	if err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if len(ag.Tools) != 1 || ag.Tools[0].Name != "shell" || !ag.Parallel {
		t.Fatalf("bad agent request: %+v", ag)
	}

	// Unsupported server-hosted tool type must be rejected, not silently dropped.
	bad := &chatBody{Tools: json.RawMessage(`[{"type":"web_search"}]`), Messages: body.Messages}
	if _, err := buildAgentRequest(bad); err == nil {
		t.Fatal("unknown tool type must be rejected")
	}

	// Duplicate names are ambiguous.
	dup := &chatBody{Tools: json.RawMessage(`[{"type":"function","function":{"name":"a"}},{"type":"function","function":{"name":"a"}}]`), Messages: body.Messages}
	if _, err := buildAgentRequest(dup); err == nil {
		t.Fatal("duplicate tool names must be rejected")
	}

	// tool_choice naming an undefined tool is a client bug, not a silent no-op.
	named := &chatBody{
		Tools:      json.RawMessage(`[{"type":"function","function":{"name":"a"}}]`),
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"b"}}`),
		Messages:   body.Messages,
	}
	if _, err := buildAgentRequest(named); err == nil {
		t.Fatal("tool_choice of an undefined tool must be rejected")
	}

	// tool_choice "required" with no tools cannot be honoured.
	req := &chatBody{ToolChoice: json.RawMessage(`"required"`), Messages: body.Messages}
	if _, err := buildAgentRequest(req); err == nil {
		t.Fatal("required without tools must be rejected")
	}
}

func TestVisibleAgentMessages(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: json.RawMessage(`"be brief"`)},
		{Role: "user", Content: json.RawMessage(`"run it"`)},
		{Role: "assistant", Content: json.RawMessage(`""`), ToolCalls: json.RawMessage(`[{"id":"call_1","type":"function","function":{"name":"shell","arguments":"{}"}}]`)},
		{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"done"`)},
	}
	out, err := visibleAgentMessages(msgs)
	if err != nil {
		t.Fatalf("valid history rejected: %v", err)
	}
	if len(out) != 4 || out[3]["tool_call_id"] != "call_1" {
		t.Fatalf("bad history: %+v", out)
	}

	// A tool result with no matching pending call must be rejected.
	orphan := []chatMessage{{Role: "tool", ToolCallID: "nope", Content: json.RawMessage(`"x"`)}}
	if _, err := visibleAgentMessages(orphan); err == nil {
		t.Fatal("orphan tool result must be rejected")
	}

	// An unanswered pending call must be rejected.
	pending := []chatMessage{
		{Role: "assistant", Content: json.RawMessage(`""`), ToolCalls: json.RawMessage(`[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]`)},
	}
	if _, err := visibleAgentMessages(pending); err == nil {
		t.Fatal("unanswered pending call must be rejected")
	}
}

func TestMcpToolsAndFingerprint(t *testing.T) {
	ag := &agentRequest{Tools: []agentTool{
		{Type: "function", Name: "shell", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Type: "custom", Name: "apply_patch"},
	}}
	tools := ag.mcpTools()
	if len(tools) != 2 || tools[0]["name"] != "shell" {
		t.Fatalf("bad tool list: %+v", tools)
	}
	// A custom tool with no schema must still expose a callable input schema.
	schema, _ := json.Marshal(tools[1]["inputSchema"])
	if !strings.Contains(string(schema), `"input"`) {
		t.Fatalf("custom tool needs a default input schema: %s", schema)
	}
	// Same tools → same connector name; a changed tool set → a different one.
	if ag.fingerprint() != ag.fingerprint() {
		t.Fatal("fingerprint must be stable")
	}
	other := &agentRequest{Tools: []agentTool{{Type: "function", Name: "shell", Parameters: json.RawMessage(`{"type":"object","properties":{"x":{}}}`)}}}
	if other.fingerprint() == ag.fingerprint() {
		t.Fatal("different tool sets must produce different connectors")
	}
}

func TestRenderAgent(t *testing.T) {
	ag := &agentRequest{
		Tools:      []agentTool{{Type: "function", Name: "shell"}},
		Choice:     "named",
		ChoiceName: "shell",
		Parallel:   true,
		Messages:   []map[string]any{{"role": "user", "content": "hi"}},
	}
	prompt := renderAgent(ag, "external_agent_abc")
	for _, want := range []string{"external_agent_abc", `"shell"`, "CONVERSATION_JSON:", `"role":"user"`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "no client tools") {
		t.Fatal("tools were available, prompt must not claim otherwise")
	}

	// No tools → the model must be told not to call anything.
	empty := renderAgent(&agentRequest{Choice: "auto"}, "c")
	if !strings.Contains(empty, "No client tools") {
		t.Fatalf("empty tool set must be stated: %s", empty)
	}
}

func TestFinishOfAndToolCallChunks(t *testing.T) {
	plain := &chatResult{Finish: "stop"}
	if finishOf(plain) != "stop" {
		t.Fatalf("want stop, got %q", finishOf(plain))
	}
	withCalls := &chatResult{Finish: "stop", Calls: []collectedCall{{ID: "call_1", Type: "function", Name: "shell", Arguments: `{"a":1}`}}}
	if finishOf(withCalls) != "tool_calls" {
		t.Fatalf("calls must force finish_reason tool_calls, got %q", finishOf(withCalls))
	}
	chunks := toolCallChunks(false, "id", 1, "m", withCalls)
	if len(chunks) != 1 || !strings.Contains(string(chunks[0].Payload), `"tool_calls"`) {
		t.Fatalf("bad tool call chunk: %+v", chunks)
	}
	if len(toolCallChunks(false, "id", 1, "m", plain)) != 0 {
		t.Fatal("no calls must emit no chunk")
	}
}

func TestDeviceIDStable(t *testing.T) {
	a := &storedAuth{UID: "42"}
	if a.deviceID() != a.deviceID() {
		t.Fatal("derived device id must be stable")
	}
	if a.deviceID() == (&storedAuth{UID: "43"}).deviceID() {
		t.Fatal("different accounts need different device ids")
	}
	explicit := &storedAuth{UID: "42", DeviceID: "12345"}
	if explicit.deviceID() != "12345" {
		t.Fatal("a stored device id must win")
	}
}

func TestSameDay(t *testing.T) {
	now := time.Now()
	if sameDay(0, now) {
		t.Fatal("a zero timestamp is never the same day")
	}
	if !sameDay(now.UnixMilli(), now) {
		t.Fatal("today must match today")
	}
	if sameDay(now.AddDate(0, 0, -1).UnixMilli(), now) {
		t.Fatal("yesterday must not match")
	}
}
