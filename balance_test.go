package main

import (
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func balancedEngine(t *testing.T, aRemaining, bRemaining, aHours, bHours float64) *engine {
	t.Helper()
	cfg, err := decodeConfig([]byte("mode: active"))
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(nil, nil, cfg)
	e.now = func() time.Time { return testNow }
	for _, item := range []struct {
		id               string
		remaining, hours float64
	}{{"a", aRemaining, aHours}, {"b", bRemaining, bHours}} {
		s := testSnapshot(item.id, time.Duration(item.hours*float64(time.Hour)))
		s.Weekly.Utilization = ptr(100 - item.remaining)
		e.states[item.id] = accountState{Provider: "claude", Snapshot: &s}
	}
	return e
}

func countBalancedPicks(t *testing.T, e *engine, req pluginapi.SchedulerPickRequest, n int) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	for i := 0; i < n; i++ {
		got := e.pick(req)
		if !got.Handled {
			t.Fatalf("unexpected delegation: %+v", e.last)
		}
		counts[got.AuthID]++
	}
	return counts
}

func TestBalancedTrafficScenarios(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		aRemaining, bRemaining, aHours, bHours float64
		minShare, maxShare                     float64
	}{
		{"equal accounts split bursts", 80, 80, 72, 72, .499, .501},
		{"sooner reset favored without monopolizing", 80, 80, 48, 120, .75, .85},
		{"low balance protected outside final day", 10, 80, 48, 120, .01, .10},
		{"expiring quota used before fresh account", 80, 100, 6, 168, .98, 1},
		{"both low balances still supply demand", 2, 2, 72, 72, .499, .501},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := balancedEngine(t, tc.aRemaining, tc.bRemaining, tc.aHours, tc.bHours)
			counts := countBalancedPicks(t, e, testRequest("a", "b"), 1000)
			share := float64(counts["a"]) / 1000
			if share < tc.minShare || share > tc.maxShare {
				t.Fatalf("share %.3f outside [%.3f, %.3f]: %v", share, tc.minShare, tc.maxShare, counts)
			}
			t.Logf("A %d / B %d requests with unchanged cached quota", counts["a"], counts["b"])
		})
	}
}

func TestBalancedShortAndModelHeadroom(t *testing.T) {
	for _, kind := range []string{"short", "sonnet"} {
		t.Run(kind, func(t *testing.T) {
			e := balancedEngine(t, 80, 80, 72, 72)
			window := &quotaWindow{Utilization: ptr(95.0), ResetsAt: ptr(testNow.Add(time.Hour))}
			if kind == "short" {
				e.states["a"].Snapshot.FiveHour = window
			} else {
				e.states["a"].Snapshot.Sonnet = window
			}
			counts := countBalancedPicks(t, e, testRequest("a", "b"), 1000)
			if counts["a"] == 0 || counts["a"] >= 100 {
				t.Fatalf("low headroom should reduce share, not reserve it: %v", counts)
			}
			if only := countBalancedPicks(t, e, testRequest("a"), 20); only["a"] != 20 {
				t.Fatal(only)
			}
		})
	}
}

func TestBalancedAdaptsAfterRefreshAndReset(t *testing.T) {
	e := balancedEngine(t, 80, 80, 48, 120)
	req := testRequest("a", "b")
	before := countBalancedPicks(t, e, req, 1000)
	e.states["a"].Snapshot.Weekly.Utilization = ptr(95.0)
	after := countBalancedPicks(t, e, req, 1000)
	if before["a"] <= before["b"] || after["a"] >= after["b"] {
		t.Fatalf("did not shift away from depleted balance: %v -> %v", before, after)
	}
	// A just reset while B's plentiful quota expires in six hours.
	e.states["a"].Snapshot.Weekly = quotaWindow{Utilization: ptr(0.0), ResetsAt: ptr(testNow.Add(168 * time.Hour))}
	e.states["b"].Snapshot.Weekly = quotaWindow{Utilization: ptr(20.0), ResetsAt: ptr(testNow.Add(6 * time.Hour))}
	reset := countBalancedPicks(t, e, req, 1000)
	if reset["b"] < 980 {
		t.Fatalf("fresh account consumed ahead of expiring quota: %v", reset)
	}
}

func TestBalancedConcurrentBurstAndShadow(t *testing.T) {
	e := balancedEngine(t, 80, 80, 72, 72)
	req := testRequest("a", "b")
	var wg sync.WaitGroup
	var counts sync.Map
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			for j := 0; j < 100; j++ {
				got := e.pick(req)
				if !got.Handled {
					t.Error("delegated healthy pool")
				}
				v, _ := counts.LoadOrStore(got.AuthID, new(syncCounter))
				v.(*syncCounter).increment()
				if _, err := json.Marshal(e.status()); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	for _, id := range []string{"a", "b"} {
		v, _ := counts.Load(id)
		if v == nil || v.(*syncCounter).n != 400 {
			t.Fatalf("unbalanced concurrent burst for %s: %v", id, v)
		}
	}
	e.cfg.Mode = "shadow"
	proposals := map[string]int{}
	for i := 0; i < 20; i++ {
		if e.pick(req).Handled {
			t.Fatal("shadow changed routing")
		}
		proposals[e.last.AuthID]++
	}
	if proposals["a"] != 10 || proposals["b"] != 10 || e.routed.Load() != 800 {
		t.Fatalf("shadow did not simulate rotation: %v", proposals)
	}
}

type syncCounter struct {
	sync.Mutex
	n int
}

func (c *syncCounter) increment() { c.Lock(); c.n++; c.Unlock() }

func TestBalancedCandidateChangesAndProviderIsolation(t *testing.T) {
	e := balancedEngine(t, 80, 80, 72, 72)
	req := testRequest("a", "b")
	countBalancedPicks(t, e, req, 11)
	countBalancedPicks(t, e, testRequest("a"), 100)
	if len(e.rotation["claude"]) != 1 {
		t.Fatal("absent account kept accruing credit")
	}
	counts := countBalancedPicks(t, e, req, 100)
	if counts["a"] != 50 || counts["b"] != 50 {
		t.Fatalf("returning account caused catch-up burst: %v", counts)
	}
	codex := testRequest("c", "d")
	codex.Provider, codex.Providers = "codex", []string{"codex"}
	for i := range codex.Candidates {
		c := &codex.Candidates[i]
		c.Provider = "codex"
		s := testSnapshot(c.ID, 72*time.Hour)
		s.Identity = (credential{Type: "codex", AccountUUID: c.ID}).identity()
		e.states[c.ID] = accountState{Provider: "codex", Snapshot: &s}
	}
	for i := 0; i < 100; i++ {
		e.pick(codex)
		e.pick(req)
	}
	if len(e.rotation["codex"]) != 2 || len(e.rotation["claude"]) != 2 {
		t.Fatal("provider rotations interfered")
	}
	// Exhausted accounts never regain eligibility through rotation credit.
	e.states["a"].Snapshot.Weekly.Utilization = ptr(100.0)
	if got := countBalancedPicks(t, e, req, 100); got["b"] != 100 {
		t.Fatal(got)
	}
	e.states["b"].Snapshot.Weekly.Utilization = ptr(100.0)
	if e.pick(req).Handled {
		t.Fatal("exhausted pool did not delegate")
	}
}

func TestBalancedSustainedDemand(t *testing.T) {
	e := balancedEngine(t, 80, 80, 48, 120)
	now := testNow
	e.now = func() time.Time { return now }
	req := testRequest("a", "b")
	balances := map[string]float64{"a": 80, "b": 80}
	counts := map[string]int{}
	// Equal-cost requests, five-minute observation intervals, 24h sustained
	// demand consuming 120 of the initial 160 percentage-points of quota.
	for interval := 0; interval < 288; interval++ {
		for id, remaining := range balances {
			s := e.states[id].Snapshot
			s.Weekly.Utilization, s.ReadAt = ptr(100-remaining), now
		}
		for request := 0; request < 10; request++ {
			got := e.pick(req)
			if !got.Handled {
				t.Fatalf("delegated before capacity exhausted at %v", now)
			}
			balances[got.AuthID] -= 120.0 / 2880
			counts[got.AuthID]++
		}
		now = now.Add(5 * time.Minute)
	}
	if balances["a"] <= 0 || balances["b"] <= 0 || counts["a"] <= counts["b"] {
		t.Fatalf("premature depletion or no reset preference: balances=%v picks=%v", balances, counts)
	}
	t.Logf("After 24h: remaining=%v picks=%v", balances, counts)
}

func TestBalancedWeightContinuousAtFinalDay(t *testing.T) {
	s := testSnapshot("a", 24*time.Hour)
	s.Weekly.Utilization = ptr(90.0)
	before := balanceWeight(s, "sonnet", testNow.Add(-time.Second))
	after := balanceWeight(s, "sonnet", testNow.Add(time.Second))
	if math.Abs(before-after)/before > .001 {
		t.Fatal("24h boundary causes routing discontinuity")
	}
}
