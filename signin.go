package main

// Daily credit claim (签到) — PHASE 3, not implemented yet.
//
// kuku2api claims free points via /api/genflowpro/freepoint/* (homenew lists the
// tasks, rewardClaim claims one, taskComplete reports a performed task). Doing
// that from a plugin needs to enumerate the host's kuku credentials on a timer;
// the host auth-list callback exists (host.auth.list) but has no documented
// request/response type in SDK v7.2.157, so the enumeration step is deliberately
// deferred rather than guessed.
//
// Until then this only stores the operator's on/off choice so the config surface
// is stable, and the scheduler is a no-op.

import (
	"sync"
)

var (
	signinMu      sync.Mutex
	signinEnabled bool
	signinStop    chan struct{}
)

func applyDailyBonusConfig(on bool) {
	signinMu.Lock()
	defer signinMu.Unlock()
	signinEnabled = on
}

func dailyBonusEnabled() bool {
	signinMu.Lock()
	defer signinMu.Unlock()
	return signinEnabled
}

// startSigninScheduler starts the daily-claim ticker when enabled. No-op until
// phase 3 lands.
func startSigninScheduler() {
	if !dailyBonusEnabled() {
		return
	}
	hostLog("info", "kuku: 自动签到已开启但尚未实现（phase 3），当前不执行任何操作", nil)
}

// stopSigninScheduler stops the ticker if running.
func stopSigninScheduler() {
	signinMu.Lock()
	defer signinMu.Unlock()
	if signinStop != nil {
		close(signinStop)
		signinStop = nil
	}
}

// claimTasksForAccount is the phase-3 entry point: given one account it lists
// the claimable free-point tasks and claims them. Left unimplemented on purpose.
func claimTasksForAccount(_ *storedAuth) error {
	return nil
}
