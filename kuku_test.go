package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseStored(t *testing.T) {
	sa, ok := parseStored([]byte(`{"bduss":"B","stoken":"S","ptoken":"P","uid":"123"}`))
	if !ok || sa.BDUSS != "B" || sa.STOKEN != "S" || sa.PTOKEN != "P" || sa.UID != "123" {
		t.Fatalf("nested shape not parsed: %+v ok=%v", sa, ok)
	}
	// Flat layout from other tooling must still parse.
	sa, ok = parseStored([]byte(`{"BDUSS":"B","STOKEN":"S"}`))
	if !ok || sa.BDUSS != "B" || sa.STOKEN != "S" {
		t.Fatalf("flat shape not parsed: %+v ok=%v", sa, ok)
	}
	// Missing half of the pair is not a credential.
	if _, ok := parseStored([]byte(`{"bduss":"B"}`)); ok {
		t.Fatal("credential without stoken must be rejected")
	}
	if _, ok := parseStored(nil); ok {
		t.Fatal("empty storage must be rejected")
	}
}

func TestAccountIdentityStable(t *testing.T) {
	a := &storedAuth{BDUSS: "x", STOKEN: "y", UID: "987"}
	if accountIdentity(a) != "987" {
		t.Fatalf("uid should drive identity, got %q", accountIdentity(a))
	}
	// Without a uid the identity must still be deterministic and filesystem-safe.
	b := &storedAuth{BDUSS: "x/y z", STOKEN: "y"}
	id1, id2 := accountIdentity(b), accountIdentity(b)
	if id1 != id2 || strings.ContainsAny(id1, "/ ") {
		t.Fatalf("identity not stable/safe: %q vs %q", id1, id2)
	}
}

func TestStripModelPrefix(t *testing.T) {
	cfgMu.Lock()
	old := cfgModelPrefix
	cfgModelPrefix = "kuku"
	cfgMu.Unlock()
	defer func() { cfgMu.Lock(); cfgModelPrefix = old; cfgMu.Unlock() }()

	if got := stripModelPrefix("kuku/gateway-glm-5.3-flash"); got != "gateway-glm-5.3-flash" {
		t.Fatalf("prefix not stripped: %q", got)
	}
	if got := stripModelPrefix("other/model"); got != "other/model" {
		t.Fatalf("foreign id mangled: %q", got)
	}
}

func TestResolveThink(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{"think_mode":3}`, 3},
		{`{"reasoning":{"effort":"high"}}`, 3},
		{`{"reasoning_effort":"minimal"}`, 1},
		{`{"think_mode":2,"reasoning_effort":"high"}`, 2}, // explicit wins
		{`{}`, 0}, // unspecified → upstream default
	}
	for _, c := range cases {
		var b chatBody
		if err := json.Unmarshal([]byte(c.body), &b); err != nil {
			t.Fatalf("unmarshal %s: %v", c.body, err)
		}
		if got := resolveThink(&b); got != c.want {
			t.Fatalf("resolveThink(%s) = %d, want %d", c.body, got, c.want)
		}
	}
}

func TestFrameToDelta(t *testing.T) {
	if d := frameToDelta(map[string]any{"type": "TEXT_BLOCK_DELTA", "data": map[string]any{"delta": "hi"}}); d == nil || d.Content != "hi" {
		t.Fatalf("text delta not mapped: %+v", d)
	}
	if d := frameToDelta(map[string]any{"type": "THINKING_BLOCK_DELTA", "data": map[string]any{"delta": "think"}}); d == nil || d.ReasoningContent != "think" {
		t.Fatalf("thinking delta not mapped: %+v", d)
	}
	d := frameToDelta(map[string]any{"type": "MODEL_CALL_END", "data": map[string]any{
		"input_tokens": float64(10), "output_tokens": float64(5), "finished_reason": "completed"}})
	if d == nil || d.Usage["total_tokens"] != 15 || d.FinishReason != "stop" {
		t.Fatalf("usage/finish not mapped: %+v", d)
	}
	d = frameToDelta(map[string]any{"type": "COMMERCIAL", "data": map[string]any{"reason": "insufficient_balance", "errno": float64(11002)}})
	if d == nil || d.CommercialErrno != 11002 {
		t.Fatalf("commercial not mapped: %+v", d)
	}
	if d := frameToDelta(map[string]any{"type": "SOMETHING_ELSE"}); d != nil {
		t.Fatalf("unknown frame should map to nil, got %+v", d)
	}
}

func TestParseSSEBlock(t *testing.T) {
	frame, ok := parseSSEBlock("event: message\nid: 42\ndata: {\"type\":\"TEXT_BLOCK_DELTA\",\"data\":{\"delta\":\"x\"}}")
	if !ok || frame.Event != "message" || frame.ID != "42" || str(frame.Data["type"]) != "TEXT_BLOCK_DELTA" {
		t.Fatalf("sse block not parsed: %+v ok=%v", frame, ok)
	}
	if _, ok := parseSSEBlock(": keep-alive"); ok {
		t.Fatal("comment-only block must not parse")
	}
}

func TestBuildPrompt(t *testing.T) {
	var single chatBody
	_ = json.Unmarshal([]byte(`{"messages":[{"role":"user","content":"你好"}]}`), &single)
	text, _, err := buildPrompt(&single)
	if err != nil || text != "你好" {
		t.Fatalf("single message prompt wrong: %q err=%v", text, err)
	}

	var multi chatBody
	_ = json.Unmarshal([]byte(`{"messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"a"},
		{"role":"assistant","content":"b"},
		{"role":"user","content":"c"}]}`), &multi)
	text, instr, err := buildPrompt(&multi)
	if err != nil || !strings.Contains(text, "CONVERSATION_JSON") || instr != "be brief" {
		t.Fatalf("multi message prompt wrong: %q instr=%q err=%v", text, instr, err)
	}

	var empty chatBody
	_ = json.Unmarshal([]byte(`{"messages":[]}`), &empty)
	if _, _, err := buildPrompt(&empty); err == nil {
		t.Fatal("empty messages must error")
	}
}

func TestModelsForAuthPublishPolicy(t *testing.T) {
	cfgMu.Lock()
	oldPrefix, oldMode, oldAllow, oldExtra := cfgModelPrefix, cfgPublishMode, cfgPublishAllow, cfgExtraModels
	cfgModelPrefix, cfgPublishMode, cfgPublishAllow, cfgExtraModels = "kuku", "allow", []string{"keep-me"}, []string{"extra-one"}
	cfgMu.Unlock()
	defer func() {
		cfgMu.Lock()
		cfgModelPrefix, cfgPublishMode, cfgPublishAllow, cfgExtraModels = oldPrefix, oldMode, oldAllow, oldExtra
		cfgMu.Unlock()
	}()

	upstream := []upstreamModel{{ModelName: "keep-me"}, {ModelName: "drop-me"}}
	got := modelsForAuth(&storedAuth{UID: "1"}, upstream)
	ids := map[string]bool{}
	for _, m := range got {
		ids[m.ID] = true
	}
	if !ids["kuku/keep-me"] || !ids["kuku/extra-one"] || ids["kuku/drop-me"] {
		t.Fatalf("publish policy wrong: %+v", ids)
	}
}

// TestGlmOnlyPublish pins the operator's requirement against the catalogue the
// live service actually returns: only the glm entries may reach the panel.
func TestGlmOnlyPublish(t *testing.T) {
	cfgMu.Lock()
	oldPrefix, oldMode, oldAllow, oldExtra := cfgModelPrefix, cfgPublishMode, cfgPublishAllow, cfgExtraModels
	cfgModelPrefix, cfgPublishMode, cfgPublishAllow, cfgExtraModels = "kuku", "allow",
		[]string{"gateway-glm-5.3-flash", "glm-5.3", "gateway-glm-5.2", "gateway-glm-5.1-kuaishou"}, nil
	cfgMu.Unlock()
	defer func() {
		cfgMu.Lock()
		cfgModelPrefix, cfgPublishMode, cfgPublishAllow, cfgExtraModels = oldPrefix, oldMode, oldAllow, oldExtra
		cfgMu.Unlock()
	}()

	// The real catalogue, in upstream's order.
	catalog := []upstreamModel{}
	for _, id := range []string{
		"auto", "gateway-deepseek-v4.1-flash-volcengine", "gateway-deepseek-v4-pro-tencent",
		"gateway-deepseek-v4-flash-tencent", "gateway-glm-5.3-flash", "glm-5.3", "gateway-glm-5.2",
		"gateway-glm-5.1-kuaishou", "ernie-5.1", "ms-kimi-k3", "gateway-kimi-k2.7-code-tencent",
		"gateway-kimi-k2.6", "ali-minimax/minimax-m3",
	} {
		catalog = append(catalog, upstreamModel{ModelName: id})
	}

	got := modelsForAuth(&storedAuth{UID: "1"}, catalog)
	want := []string{"kuku/gateway-glm-5.3-flash", "kuku/glm-5.3", "kuku/gateway-glm-5.2", "kuku/gateway-glm-5.1-kuaishou"}
	if len(got) != len(want) {
		t.Fatalf("want %d published models, got %d: %+v", len(want), len(got), got)
	}
	for i, m := range got {
		if m.ID != want[i] {
			t.Errorf("published[%d] = %s, want %s", i, m.ID, want[i])
		}
	}
	// The claim turn must land on the cheapest glm, not on "auto".
	if first := got[0].Name; first != "gateway-glm-5.3-flash" {
		t.Errorf("the daily-claim turn would use %q", first)
	}
}

func TestLoginAuthBodySignature(t *testing.T) {
	body := loginAuthBody("B", "P")
	if !strings.HasPrefix(body, "appid=1&bduss=B&ptoken=P&return_type=1&tpl=genflowpro") {
		t.Fatalf("unexpected body prefix: %s", body)
	}
	if !strings.Contains(body, "&sig=") || strings.Contains(body, "sign_key") {
		t.Fatalf("body must carry sig and not sign_key: %s", body)
	}
	// Deterministic for a fixed input.
	if loginAuthBody("B", "P") != body {
		t.Fatal("signature must be deterministic")
	}
}

func TestIsAuthFailure(t *testing.T) {
	if !isAuthFailure(&UpstreamError{Code: -6}) {
		t.Fatal("errno -6 must count as auth failure")
	}
	if !isAuthFailure(&UpstreamError{Status: 401}) {
		t.Fatal("http 401 must count as auth failure")
	}
	if isAuthFailure(&UpstreamError{Code: 11002}) {
		t.Fatal("insufficient balance is not an auth failure")
	}
}
