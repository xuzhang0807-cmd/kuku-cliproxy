package main

// Daily credit claim (签到) — phase 3.
//
// Contract ported from kuku2api (src/scheduler.mjs + gateway.mjs claimFreePoints):
// freePointHome lists the tasks, rewardClaim is what actually pays, taskComplete
// only reports progress. A CHAT task becomes claimable only after a real
// conversation, so earning it spends one short upstream turn.
//
// OFF by default: claiming is a write on the user's Baidu account and the CHAT
// earn spends points. It only runs when the operator turns it on.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// -----------------------------------------------------------------------------
// Host credential enumeration
// -----------------------------------------------------------------------------

// hostRPC calls a host callback and returns the inner result payload.
func hostRPC(method string, request []byte) (json.RawMessage, error) {
	raw, err := hostCall(method, request)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("host %s: empty response", method)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("host %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("host %s: %s %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host %s: request failed", method)
	}
	return env.Result, nil
}

// ownCredentials lists every credential in the host store that belongs to this
// plugin and loads its stored JSON.
func ownCredentials() ([]*storedAuth, error) {
	result, err := hostRPC(pluginabi.MethodHostAuthList, []byte(`{}`))
	if err != nil {
		return nil, err
	}
	var list struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(result, &list); err != nil {
		return nil, fmt.Errorf("host.auth.list: %w", err)
	}
	var out []*storedAuth
	for _, entry := range list.Files {
		if !ownCredential(entry) {
			continue
		}
		index := strings.TrimSpace(entry.AuthIndex)
		if index == "" {
			continue
		}
		req, _ := json.Marshal(map[string]string{"auth_index": index})
		got, err := hostRPC(pluginabi.MethodHostAuthGet, req)
		if err != nil {
			continue
		}
		var payload struct {
			JSON json.RawMessage `json:"json"`
		}
		if json.Unmarshal(got, &payload) != nil {
			continue
		}
		if sa, ok := parseStored(payload.JSON); ok {
			out = append(out, sa)
		}
	}
	return out, nil
}

func ownCredential(entry pluginapi.HostAuthFileEntry) bool {
	if strings.EqualFold(strings.TrimSpace(entry.Provider), providerName) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(entry.Type), providerName)
}

// -----------------------------------------------------------------------------
// Claim
// -----------------------------------------------------------------------------

type freeTask struct {
	ActivityKey string
	PeriodID    int64
	TaskKey     string
	TaskType    string
	Status      string
	Claimable   float64
	Reward      float64
}

// flattenTasks walks activities[].tabs[].tasks[] into a flat list.
func flattenTasks(data map[string]any) []freeTask {
	var out []freeTask
	activities, _ := data["activities"].([]any)
	for _, a := range activities {
		act, _ := a.(map[string]any)
		if act == nil {
			continue
		}
		periodID, _ := num(act["period_no"])
		activityKey := str(act["activity_key"])
		tabs, _ := act["tabs"].([]any)
		for _, tb := range tabs {
			tab, _ := tb.(map[string]any)
			if tab == nil {
				continue
			}
			tasks, _ := tab["tasks"].([]any)
			for _, tk := range tasks {
				t, _ := tk.(map[string]any)
				if t == nil {
					continue
				}
				claimable, _ := numFloat(t["claimable_point"])
				reward, _ := numFloat(t["single_reward_point"])
				out = append(out, freeTask{
					ActivityKey: activityKey,
					PeriodID:    periodID,
					TaskKey:     str(t["task_key"]),
					TaskType:    str(t["task_type"]),
					Status:      str(t["task_status"]),
					Claimable:   claimable,
					Reward:      reward,
				})
			}
		}
	}
	return out
}

type claimResult struct {
	UID          string   `json:"uid"`
	OK           bool     `json:"ok"`
	PointsEarned float64  `json:"points_earned"`
	Claimed      []string `json:"claimed"`
	Reported     []string `json:"reported"`
	Message      string   `json:"message,omitempty"`
}

// claimAccount claims everything currently claimable for one account.
func claimAccount(ctx context.Context, sa *storedAuth, earnChat bool) claimResult {
	rec := claimResult{UID: sa.UID}
	client := sa.client()
	deviceID := sa.deviceID()

	tasks, err := listTasks(ctx, client)
	if err != nil {
		rec.Message = err.Error()
		return rec
	}

	if earnChat {
		// LOGIN: the client reports its own daily login; mirror it so the task
		// becomes claimable. taskComplete never pays — rewardClaim does.
		if login := findTask(tasks, "LOGIN"); login != nil && login.Status != "FINISHED" && login.Claimable <= 0 {
			if _, err := client.claimTask(ctx, "LOGIN", deviceID, false); err == nil {
				rec.Reported = append(rec.Reported, "LOGIN")
				tasks, _ = listTasks(ctx, client)
			}
		}
		// CHAT: only becomes claimable after a real conversation.
		if chat := findTask(tasks, "CHAT"); chat != nil && chat.Status != "FINISHED" && chat.Claimable <= 0 {
			if model := firstPublishedModel(ctx, sa); model != "" {
				if _, err := runChat(ctx, sa, &chatBody{
					Model:     model,
					Messages:  []chatMessage{{Role: "user", Content: json.RawMessage(`"请只回复 OK，不要调用工具或执行任何任务。"`)}},
					ThinkMode: json.RawMessage("1"),
				}, nil); err == nil {
					if _, err := client.claimTask(ctx, "CHAT", deviceID, false); err == nil {
						rec.Reported = append(rec.Reported, "CHAT")
					}
					tasks, _ = listTasks(ctx, client)
				}
			}
		}
	}

	for _, t := range tasks {
		if t.Claimable <= 0 {
			continue
		}
		r, err := client.claimReward(ctx, t.ActivityKey, t.PeriodID, t.TaskKey)
		if err != nil {
			rec.Message = err.Error()
			continue
		}
		if str(r["claim_status"]) != "SUCCESS" {
			rec.Message = fmt.Sprintf("reward claim %s: %s", t.TaskKey, str(r["claim_status"]))
			continue
		}
		pts, _ := numFloat(r["claimed_point"])
		rec.PointsEarned += pts
		rec.Claimed = append(rec.Claimed, firstNonEmpty(t.TaskType, t.TaskKey))
	}
	rec.OK = true
	return rec
}

func listTasks(ctx context.Context, client *KukuClient) ([]freeTask, error) {
	data, err := client.freePointHome(ctx)
	if err != nil {
		return nil, err
	}
	return flattenTasks(data), nil
}

func findTask(tasks []freeTask, taskType string) *freeTask {
	for i := range tasks {
		if tasks[i].TaskType == taskType {
			return &tasks[i]
		}
	}
	return nil
}

// firstPublishedModel returns a model id this credential can actually serve, used
// for the CHAT earn turn. Empty when the catalog is unavailable.
func firstPublishedModel(ctx context.Context, sa *storedAuth) string {
	models, err := fetchCatalog(ctx, sa)
	if err != nil {
		return ""
	}
	for _, m := range models {
		if id := strings.TrimSpace(m.ModelName); id != "" {
			return id
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// Scheduler
// -----------------------------------------------------------------------------

const signinStateFile = "data/kuku-signin.state"

type signinState struct {
	Enabled   bool       `json:"enabled"`
	Hour      int        `json:"hour"`
	LastRunAt int64      `json:"last_run_at"`
	LastRun   *signinRun `json:"last_run,omitempty"`
}

type signinRun struct {
	When     int64         `json:"when"`
	Accounts []claimResult `json:"accounts"`
}

var (
	signinMu   sync.Mutex
	signinStop chan struct{}
	signinData = signinState{Hour: 9}
	signinOnce sync.Once
)

func loadSigninState() {
	signinOnce.Do(func() {
		raw, err := os.ReadFile(signinStateFile)
		if err != nil {
			return
		}
		var s signinState
		if json.Unmarshal(raw, &s) == nil {
			if s.Hour < 0 || s.Hour > 23 {
				s.Hour = 9
			}
			signinData = s
		}
	})
}

func persistSigninState() {
	// Best-effort: the plugin runs in the CPA container, whose data/ is bind-mounted.
	_ = os.MkdirAll(filepath.Dir(signinStateFile), 0o700)
	if raw, err := json.MarshalIndent(signinData, "", "  "); err == nil {
		_ = os.WriteFile(signinStateFile, raw, 0o600)
	}
}

func applyDailyBonusConfig(on bool) {
	loadSigninState()
	signinMu.Lock()
	signinData.Enabled = on
	signinMu.Unlock()
}

func dailyBonusEnabled() bool {
	loadSigninState()
	signinMu.Lock()
	defer signinMu.Unlock()
	return signinData.Enabled
}

func startSigninScheduler() {
	if !dailyBonusEnabled() {
		return
	}
	signinMu.Lock()
	if signinStop != nil {
		signinMu.Unlock()
		return
	}
	stop := make(chan struct{})
	signinStop = stop
	hour := signinData.Hour
	signinMu.Unlock()

	hostLog("info", "kuku: 自动签到已开启", map[string]any{"hour": hour})
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				runSigninTick()
			}
		}
	}()
}

func stopSigninScheduler() {
	signinMu.Lock()
	defer signinMu.Unlock()
	if signinStop != nil {
		close(signinStop)
		signinStop = nil
	}
}

// runSigninTick claims once per day after the configured hour.
func runSigninTick() {
	signinMu.Lock()
	enabled := signinData.Enabled
	hour := signinData.Hour
	last := signinData.LastRunAt
	signinMu.Unlock()
	if !enabled {
		return
	}
	now := time.Now()
	if now.Hour() < hour || sameDay(last, now) {
		return
	}
	runSignin()
}

// runSignin claims for every kuku credential, one account at a time.
func runSignin() *signinRun {
	run := &signinRun{When: time.Now().UnixMilli()}
	creds, err := ownCredentials()
	if err != nil {
		hostLog("warn", "kuku: 签到无法枚举凭据", map[string]any{"error": err.Error()})
	} else {
		for i, sa := range creds {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			rec := claimAccount(ctx, sa, true)
			cancel()
			run.Accounts = append(run.Accounts, rec)
			hostLog("info", "kuku: 签到结果", map[string]any{"uid": rec.UID, "ok": rec.OK,
				"points": rec.PointsEarned, "claimed": rec.Claimed, "message": rec.Message})
			// Serial with a gap so a slow upstream cannot overlap the next account.
			if i < len(creds)-1 {
				time.Sleep(15 * time.Second)
			}
		}
	}
	signinMu.Lock()
	signinData.LastRunAt = run.When
	signinData.LastRun = run
	persistSigninState()
	signinMu.Unlock()
	return run
}

func sameDay(tsMillis int64, now time.Time) bool {
	if tsMillis == 0 {
		return false
	}
	t := time.UnixMilli(tsMillis)
	return t.Year() == now.Year() && t.YearDay() == now.YearDay()
}

func numFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	}
	return 0, false
}
