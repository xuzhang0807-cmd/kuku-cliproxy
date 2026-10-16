package main

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// TestLiveQuota exercises the plugin's own upstream client against the real
// service, so the decoder is verified against the payload upstream actually
// sends rather than a hand-written fixture. Skipped unless KUKU_LIVE_AUTH
// points at a stored credential (kuku-<uid>.json). Read-only: spends nothing.
func TestLiveQuota(t *testing.T) {
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
	row := fetchAccount(&sa)
	tasks := "none"
	if row.Tasks != nil {
		tasks = fmt.Sprintf("%d/%d claimable=%d earned=%v", row.Tasks.Done, row.Tasks.Total, row.Tasks.Claimable, row.Tasks.Earned)
	}
	t.Logf("label=%q balance=%v assets=%d tasks=%s lit=%v err=%q", row.Label, row.Balance, len(row.Assets), tasks, row.Lit, row.Error)
	if row.Error != "" {
		t.Fatalf("upstream read failed: %s", row.Error)
	}
	if len(row.Assets) == 0 {
		t.Fatal("no asset buckets decoded from the vip-remain payload")
	}
	if row.Balance <= 0 {
		t.Fatalf("expected a positive token balance, got %v", row.Balance)
	}
}

func TestBalanceFrom(t *testing.T) {
	// Captured verbatim from /bizapi/gfpro/getgfvipremain on a real account.
	// Note the point fields are JSON *strings*, and assetType 1 is "token" —
	// the bucket chat calls bill against (kuku2api's balance_points).
	raw := `{"isVip":false,"uname":"Edsugar","list":[
		{"assetType":1,"assetName":"token","totalPoint":"99.94","bonusPoint":"99.94","vipPoint":"0","chargePoint":"0","freezePoint":"0"},
		{"assetType":2,"assetName":"duration","totalPoint":"1800","bonusPoint":"0","vipPoint":"0","chargePoint":"0","freezePoint":"0"},
		{"assetType":3,"assetName":"scheduled_task","totalPoint":"2","bonusPoint":"0","vipPoint":"0","chargePoint":"0","freezePoint":"0"},
		{"assetType":4,"assetName":"realtime_t","totalPoint":"0","bonusPoint":"0","vipPoint":"0","chargePoint":"0","freezePoint":"0"}
	]}`
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatal(err)
	}
	assets, balance := balanceFrom(data)
	if len(assets) != 4 {
		t.Fatalf("want 4 assets, got %d", len(assets))
	}
	if balance != 99.94 {
		t.Fatalf("want the assetType-1 bucket (99.94), got %v", balance)
	}
	if assets[0].Name != "token" || assets[0].Bonus != 99.94 || assets[2].Type != 3 || assets[2].Total != 2 {
		t.Fatalf("bad asset decode: %+v", assets)
	}

	// Same payload in snake_case must still decode.
	snake := map[string]any{"list": []any{map[string]any{"asset_type": float64(1), "total_point": "88"}}}
	if _, balance := balanceFrom(snake); balance != 88 {
		t.Fatalf("snake_case payload not decoded, got %v", balance)
	}

	// No typed bucket: report the first one rather than a misleading zero.
	untyped := map[string]any{"list": []any{map[string]any{"totalPoint": float64(7)}}}
	if _, balance := balanceFrom(untyped); balance != 7 {
		t.Fatalf("untyped payload should fall back to the first bucket, got %v", balance)
	}

	// Nothing usable must not panic.
	for _, empty := range []map[string]any{{}, {"list": nil}, {"list": "nope"}} {
		assets, balance := balanceFrom(empty)
		if balance != 0 || len(assets) != 0 {
			t.Fatalf("empty payload must yield 0, got %v / %d", balance, len(assets))
		}
	}
}

func TestTaskSummaryOf(t *testing.T) {
	tasks := []freeTask{
		{TaskType: "LOGIN", TaskKey: "k1", Status: "FINISHED", Claimable: 10, Reward: 10, MaxReward: 10},
		{TaskType: "CHAT", TaskKey: "k2", Status: "DOING", Claimable: 0, Reward: 5, MaxReward: 30},
		{TaskType: "SELF_DOWNLOAD", TaskKey: "k3", Status: "FINISHED", Claimable: 0, Reward: 20, MaxReward: 20},
	}
	s := taskSummaryOf(tasks)
	if s.Total != 3 || s.Done != 2 || s.Claimable != 1 {
		t.Fatalf("bad counts: %+v", s)
	}
	if s.Earned != 60 {
		t.Fatalf("want the day's worth 60, got %v", s.Earned)
	}
	if len(s.Items) != 3 || s.Items[1].Type != "CHAT" || s.Items[1].Status != "DOING" {
		t.Fatalf("bad items: %+v", s.Items)
	}
	// The lit flag the panel shows: every task finished.
	if lit := s.Total > 0 && s.Done == s.Total; lit {
		t.Fatal("2 of 3 finished must not read as lit")
	}
	allDone := taskSummaryOf(tasks[:1])
	if lit := allDone.Total > 0 && allDone.Done == allDone.Total; !lit {
		t.Fatal("a single finished task must read as lit")
	}
	if empty := taskSummaryOf(nil); empty.Total != 0 || len(empty.Items) != 0 {
		t.Fatalf("no tasks must be an empty summary: %+v", empty)
	}
}

func TestTaskDoneTolerantStatus(t *testing.T) {
	if !(freeTask{Status: "FINISHED"}).taskDone() {
		t.Fatal("FINISHED is done")
	}
	if !(freeTask{Status: "finished"}).taskDone() {
		t.Fatal("status comparison must be case-insensitive")
	}
	if (freeTask{Status: "DOING"}).taskDone() || (freeTask{}).taskDone() {
		t.Fatal("only FINISHED counts as done")
	}
}
