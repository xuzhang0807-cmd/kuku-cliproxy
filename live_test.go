package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// liveAuth loads the credential the live tests run against, or skips. Point
// KUKU_LIVE_AUTH at a kuku-<uid>.json from the host auth store.
func liveAuth(t *testing.T) *storedAuth {
	t.Helper()
	path := os.Getenv("KUKU_LIVE_AUTH")
	if path == "" {
		t.Skip("set KUKU_LIVE_AUTH=/path/to/kuku-<uid>.json to run against the real upstream")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var sa storedAuth
	if err := json.Unmarshal(raw, &sa); err != nil {
		t.Fatal(err)
	}
	return &sa
}

func liveContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// TestLiveChat drives the plugin's real chat path against the live service: the
// model catalogue, one plain turn, and one turn with tools (the native MCP
// bridge). It spends points, which is the point of running it. Set
// KUKU_LIVE_MODEL to pin a model instead of using the first in the catalogue.
func TestLiveChat(t *testing.T) {
	sa := liveAuth(t)
	ctx := liveContext(t)

	catalog, err := fetchCatalog(ctx, sa)
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	if len(catalog) == 0 {
		t.Fatal("catalogue came back empty")
	}
	shown := make([]string, 0, 5)
	for _, m := range catalog[:min(5, len(catalog))] {
		shown = append(shown, m.ModelName)
	}
	t.Logf("catalogue: %d models, first: %v", len(catalog), shown)

	model := catalog[0].ModelName
	if m := strings.TrimSpace(os.Getenv("KUKU_LIVE_MODEL")); m != "" {
		model = m
	}

	// --- 1. plain turn -------------------------------------------------------
	res, err := runChat(ctx, sa, &chatBody{
		Model:    model,
		Messages: []chatMessage{{Role: "user", Content: json.RawMessage(`"只回复两个字：你好"`)}},
	}, nil)
	if err != nil {
		t.Fatalf("plain chat: %v", err)
	}
	t.Logf("plain: model=%s finish=%s usage=%v consume=%v content=%q reasoning=%d chars",
		res.Model, res.Finish, res.Usage, res.Consume, res.Content, len(res.Reasoning))
	if strings.TrimSpace(res.Content) == "" && strings.TrimSpace(res.Reasoning) == "" {
		t.Fatalf("plain chat produced neither content nor reasoning: %+v", res)
	}

	// --- 2. tool call round trip --------------------------------------------
	tools := json.RawMessage(`[{"type":"function","function":{
		"name":"get_weather",
		"description":"查询指定城市的当前天气",
		"parameters":{"type":"object","properties":{"city":{"type":"string","description":"城市名"}},"required":["city"]}
	}}]`)
	tres, err := runChat(ctx, sa, &chatBody{
		Model:    model,
		Tools:    tools,
		Messages: []chatMessage{{Role: "user", Content: json.RawMessage(`"北京现在天气怎么样？必须调用 get_weather 工具查询，不要自己编造。"`)}},
	}, nil)
	if err != nil {
		t.Fatalf("tool chat: %v", err)
	}
	t.Logf("tools: finish=%s calls=%d content=%q", tres.Finish, len(tres.Calls), tres.Content)
	for i, c := range tres.Calls {
		t.Logf("  call[%d] id=%s name=%s args=%s", i, c.ID, c.Name, firstNonEmpty(c.Arguments, c.Input))
	}
	if len(tres.Calls) == 0 {
		t.Fatalf("the native MCP bridge returned no tool call (finish=%s content=%q)", tres.Finish, tres.Content)
	}
	if tres.Calls[0].Name != "get_weather" {
		t.Errorf("want the get_weather call, got %q", tres.Calls[0].Name)
	}
	if strings.TrimSpace(tres.Calls[0].ID) == "" {
		t.Error("tool call has no id, the caller cannot pair a result with it")
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(tres.Calls[0].Arguments), &args); err != nil {
		t.Errorf("tool arguments are not a JSON object: %q", tres.Calls[0].Arguments)
	} else if _, ok := args["city"]; !ok {
		t.Errorf("tool arguments lost the city field: %v", args)
	}
}
