package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var testNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func testSnapshot(id string, reset time.Duration) quotaSnapshot {
	return quotaSnapshot{
		Weekly:   quotaWindow{Utilization: ptr(25.0), ResetsAt: ptr(testNow.Add(reset))},
		FiveHour: &quotaWindow{Utilization: ptr(0.0)},
		ReadAt:   testNow, Identity: (credential{Type: "claude", AccountUUID: id}).identity(),
	}
}

func testCandidate(id string) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{ID: id, Provider: "claude", Status: "active", Metadata: map[string]any{"account_uuid": id}}
}

func testRequest(ids ...string) pluginapi.SchedulerPickRequest {
	req := pluginapi.SchedulerPickRequest{Provider: "claude", Providers: []string{"claude"}, Model: "claude-sonnet-4-6"}
	for _, id := range ids {
		req.Candidates = append(req.Candidates, testCandidate(id))
	}
	return req
}

func TestChooseWeeklyOrderAndHostEligibility(t *testing.T) {
	states := map[string]accountState{
		"a-seven-days": {Snapshot: ptr(testSnapshot("a-seven-days", 7*24*time.Hour))},
		"b-two-days":   {Snapshot: ptr(testSnapshot("b-two-days", 2*24*time.Hour))},
		"c-one-day":    {Snapshot: ptr(testSnapshot("c-one-day", 24*time.Hour))},
		"z-five-hours": {Snapshot: ptr(testSnapshot("z-five-hours", 5*time.Hour))},
	}
	req := testRequest("a-seven-days", "b-two-days", "c-one-day", "z-five-hours")
	for _, want := range []string{"z-five-hours", "c-one-day", "b-two-days", "a-seven-days"} {
		got := choose(req, states, testNow, 10*time.Minute)
		if got.AuthID != want {
			t.Fatalf("got %+v, want %s", got, want)
		}
		for i := range req.Candidates {
			if req.Candidates[i].ID == want {
				req.Candidates = append(req.Candidates[:i], req.Candidates[i+1:]...)
				break
			}
		}
	}
	if d := choose(req, states, testNow, time.Hour); d.AuthID != "" {
		t.Fatalf("selected an account absent from host candidates: %+v", d)
	}
}

func TestWeeklyResetPriorityIgnoresEarlierShortWindow(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			weeklyLater := testSnapshot("weekly-later", 96*time.Hour)
			weeklyLater.FiveHour = &quotaWindow{Utilization: ptr(10.0), ResetsAt: ptr(testNow.Add(2 * time.Hour))}
			weeklySooner := testSnapshot("weekly-sooner", 20*time.Hour)
			weeklySooner.Weekly.Utilization = ptr(20.0)
			weeklySooner.FiveHour = &quotaWindow{Utilization: ptr(0.0)}
			states := map[string]accountState{
				"weekly-later":  {Provider: provider, Snapshot: &weeklyLater},
				"weekly-sooner": {Provider: provider, Snapshot: &weeklySooner},
			}
			model := "claude-opus-4-6"
			if provider == "codex" {
				model = "gpt-5.5"
			}
			req := pluginapi.SchedulerPickRequest{
				Provider: provider, Providers: []string{provider}, Model: model,
				Candidates: []pluginapi.SchedulerAuthCandidate{
					{ID: "weekly-later", Provider: provider, Status: "active"},
					{ID: "weekly-sooner", Provider: provider, Status: "active"},
				},
			}
			if got := choose(req, states, testNow, time.Hour); got.AuthID != "weekly-sooner" {
				t.Fatalf("weekly reset in 20h must beat weekly reset in 96h regardless of 2h short window: %+v", got)
			}
			weeklySooner.FiveHour = &quotaWindow{Utilization: ptr(100.0), ResetsAt: ptr(testNow.Add(4 * time.Hour))}
			if got := choose(req, states, testNow, time.Hour); got.AuthID != "weekly-later" {
				t.Fatalf("exhausted five-hour quota must still exclude the sooner account: %+v", got)
			}
		})
	}
}

func TestChooseCodexPoolIsIndependentFromClaudePool(t *testing.T) {
	states := map[string]accountState{
		"claude":       {Provider: "claude", Snapshot: ptr(testSnapshot("claude", 24*time.Hour))},
		"codex-later":  {Provider: "codex", Snapshot: ptr(testSnapshot("codex-later", 7*24*time.Hour))},
		"codex-sooner": {Provider: "codex", Snapshot: ptr(testSnapshot("codex-sooner", 2*time.Hour))},
	}
	for _, id := range []string{"codex-later", "codex-sooner"} {
		states[id].Snapshot.Identity = (credential{Type: "codex", AccountUUID: id}).identity()
	}
	claudeRequest := testRequest("claude")
	codexRequest := testRequest("codex-later", "codex-sooner")
	codexRequest.Provider = "codex"
	codexRequest.Providers = []string{"codex"}
	for i := range codexRequest.Candidates {
		codexRequest.Candidates[i].Provider = "codex"
	}
	if got := choose(claudeRequest, states, testNow, time.Hour); got.AuthID != "claude" {
		t.Fatalf("Claude pool was not selected: %+v", got)
	}
	if got := choose(codexRequest, states, testNow, time.Hour); got.AuthID != "codex-sooner" {
		t.Fatalf("Codex pool was not selected by reset time: %+v", got)
	}
}

func TestChooseEdgeCases(t *testing.T) {
	cases := []struct {
		name   string
		change func(*pluginapi.SchedulerPickRequest, map[string]accountState)
		want   string
	}{
		{"weekly exhausted", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) {
			s["soon"].Snapshot.Weekly.Utilization = ptr(100.0)
		}, "later"},
		{"five hour exhausted", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) {
			s["soon"].Snapshot.FiveHour.Utilization = ptr(100.0)
		}, "later"},
		{"weekly reset boundary", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) {
			s["soon"].Snapshot.Weekly.ResetsAt = ptr(testNow)
		}, "later"},
		{"short reset boundary", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) {
			s["soon"].Snapshot.FiveHour.ResetsAt = ptr(testNow)
		}, "later"},
		{"stale boundary", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) {
			s["soon"].Snapshot.ReadAt = testNow.Add(-10 * time.Minute)
		}, "later"},
		{"clock rollback", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) {
			s["soon"].Snapshot.ReadAt = testNow.Add(time.Second)
		}, "later"},
		{"no known quota", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) {
			delete(s, "soon")
			delete(s, "later")
		}, ""},
		{"disabled candidate", func(r *pluginapi.SchedulerPickRequest, _ map[string]accountState) {
			r.Candidates[0].Status = "disabled"
		}, "later"},
		{"disabled metadata", func(r *pluginapi.SchedulerPickRequest, _ map[string]accountState) {
			r.Candidates[0].Metadata["disabled"] = true
		}, "later"},
		{"replaced account", func(r *pluginapi.SchedulerPickRequest, _ map[string]accountState) {
			r.Candidates[0].Metadata["account_uuid"] = "new-account"
		}, "later"},
		{"CPA 7.3.15 omits metadata", func(r *pluginapi.SchedulerPickRequest, _ map[string]accountState) { r.Candidates[0].Metadata = nil }, "soon"},
		{"different organization", func(r *pluginapi.SchedulerPickRequest, _ map[string]accountState) {
			r.Candidates[0].Metadata["organization_uuid"] = "other"
		}, "later"},
		{"token rotation same account", func(r *pluginapi.SchedulerPickRequest, _ map[string]accountState) {
			r.Candidates[0].Metadata["access_token"] = "rotated"
		}, "soon"},
		{"higher explicit priority", func(r *pluginapi.SchedulerPickRequest, _ map[string]accountState) { r.Candidates[1].Priority = 10 }, "later"},
		{"sonnet exhausted", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) {
			s["soon"].Snapshot.Sonnet = &quotaWindow{Utilization: ptr(100.0), ResetsAt: ptr(testNow.Add(time.Hour))}
		}, "later"},
		{"opus limit does not block sonnet", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) {
			s["soon"].Snapshot.Opus = &quotaWindow{Utilization: ptr(100.0), ResetsAt: ptr(testNow.Add(time.Hour))}
		}, "soon"},
		{"opus exhausted", func(r *pluginapi.SchedulerPickRequest, s map[string]accountState) {
			r.Model = "claude-opus-4-6(thinking)"
			s["soon"].Snapshot.Opus = &quotaWindow{Utilization: ptr(100.0), ResetsAt: ptr(testNow.Add(time.Hour))}
		}, "later"},
		{"codex request", func(r *pluginapi.SchedulerPickRequest, _ map[string]accountState) { r.Provider = "codex" }, ""},
		{"mixed providers", func(r *pluginapi.SchedulerPickRequest, _ map[string]accountState) {
			r.Providers = []string{"claude", "codex"}
		}, ""},
		{"mixed candidates", func(r *pluginapi.SchedulerPickRequest, _ map[string]accountState) { r.Candidates[1].Provider = "codex" }, ""},
		{"singleton providers", func(r *pluginapi.SchedulerPickRequest, _ map[string]accountState) { r.Provider = "" }, "soon"},
		{"short window absent", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) { s["soon"].Snapshot.FiveHour = nil }, "soon"},
		{"short window unstarted", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) {
			s["soon"].Snapshot.FiveHour = &quotaWindow{Utilization: ptr(0.0)}
		}, "soon"},
		{"reset rolled forward", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) {
			s["soon"].Snapshot.Weekly.ResetsAt = ptr(testNow.Add(7 * 24 * time.Hour))
		}, "later"},
		{"earlier short window does not override weekly order", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) {
			s["later"].Snapshot.FiveHour.ResetsAt = ptr(testNow.Add(time.Minute))
		}, "soon"},
		{"model-specific reset does not override weekly order", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) {
			s["later"].Snapshot.Sonnet = &quotaWindow{Utilization: ptr(10.0), ResetsAt: ptr(testNow.Add(time.Minute))}
		}, "soon"},
		{"weekly reset missing", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) {
			s["soon"].Snapshot.Weekly.ResetsAt = nil
		}, "later"},
		{"known usable preferred over unknown", func(_ *pluginapi.SchedulerPickRequest, s map[string]accountState) { delete(s, "soon") }, "later"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := testRequest("soon", "later")
			states := map[string]accountState{"soon": {Snapshot: ptr(testSnapshot("soon", 5*time.Hour))}, "later": {Snapshot: ptr(testSnapshot("later", 24*time.Hour))}}
			tc.change(&req, states)
			if got := choose(req, states, testNow, 10*time.Minute); got.AuthID != tc.want {
				t.Fatalf("got %+v, want %q", got, tc.want)
			}
			var rotation weightedRotation
			if got := rotation.choose(req, states, testNow, 10*time.Minute); got.AuthID != tc.want {
				t.Fatalf("balanced eligibility: got %+v, want %q", got, tc.want)
			}
		})
	}
}

func TestStableTieBreak(t *testing.T) {
	states := map[string]accountState{"a": {Snapshot: ptr(testSnapshot("a", time.Hour))}, "z": {Snapshot: ptr(testSnapshot("z", time.Hour))}}
	for _, req := range []pluginapi.SchedulerPickRequest{testRequest("z", "a"), testRequest("a", "z")} {
		if got := choose(req, states, testNow, time.Hour); got.AuthID != "a" {
			t.Fatalf("unstable equal reset selection: %+v", got)
		}
	}
}

type fakeHost struct {
	entries []pluginapi.HostAuthFileEntry
	creds   map[string]credential
	listErr error
	getErr  error
}

func (h *fakeHost) List() ([]pluginapi.HostAuthFileEntry, error) {
	return append([]pluginapi.HostAuthFileEntry(nil), h.entries...), h.listErr
}

func (h *fakeHost) Get(index string) (json.RawMessage, error) {
	if h.getErr != nil {
		return nil, h.getErr
	}
	raw, err := json.Marshal(h.creds[index])
	return raw, err
}

type fetchFunc func(context.Context, credential) (quotaSnapshot, error)

func (f fetchFunc) Fetch(ctx context.Context, c credential) (quotaSnapshot, error) { return f(ctx, c) }

func testEngine(t *testing.T) (*engine, *fakeHost, *time.Time, *int) {
	t.Helper()
	now := testNow
	cfg, err := decodeConfig([]byte("mode: active"))
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "index-a", Provider: "claude"}}, creds: map[string]credential{"index-a": {Type: "claude", AccountUUID: "a", AccessToken: "SECRET_TOKEN"}}}
	calls := 0
	e := newEngine(host, fetchFunc(func(_ context.Context, c credential) (quotaSnapshot, error) {
		calls++
		s := testSnapshot(c.AccountUUID, 24*time.Hour)
		s.ReadAt = now
		return s, nil
	}), cfg)
	e.now = func() time.Time { return now }
	return e, host, &now, &calls
}

func TestRefreshCadenceRemovalAndBackoff(t *testing.T) {
	e, host, now, calls := testEngine(t)
	e.refreshQuota(context.Background())
	if *calls != 1 {
		t.Fatal("startup did not refresh")
	}
	e.refreshQuota(context.Background())
	if *calls != 1 {
		t.Fatal("quota was polled before it was due")
	}
	*now = now.Add(5 * time.Minute)
	e.refreshQuota(context.Background())
	if *calls != 2 {
		t.Fatal("due quota was not refreshed")
	}
	e.fetcher = fetchFunc(func(context.Context, credential) (quotaSnapshot, error) {
		return quotaSnapshot{}, &quotaError{code: "quota_http_429", retryAfter: 12 * time.Minute}
	})
	*now = now.Add(5 * time.Minute)
	e.refreshQuota(context.Background())
	s := e.states["a"]
	if s.Failures != 1 || !s.NextRefresh.Equal(now.Add(12*time.Minute)) || s.Snapshot == nil {
		t.Fatalf("bad backoff: %+v", s)
	}
	if got := e.pick(testRequest("a")); !got.Handled {
		t.Fatal("fresh last-known quota was discarded")
	}
	*now = now.Add(5 * time.Minute)
	if got := e.pick(testRequest("a")); got.Handled {
		t.Fatal("stale last-known quota used")
	}
	host.entries = nil
	e.refreshQuota(context.Background())
	if len(e.states) != 0 {
		t.Fatal("removed auth remained in cache")
	}
}

func TestRefreshFailureSanitizationAndRevocation(t *testing.T) {
	e, host, now, _ := testEngine(t)
	e.refreshQuota(context.Background())
	host.listErr = errors.New("SECRET_TOKEN")
	e.refreshQuota(context.Background())
	status, _ := json.Marshal(e.status())
	if strings.Contains(string(status), "SECRET_TOKEN") {
		t.Fatal("host error leaked secret")
	}
	host.listErr = nil
	e.fetcher = fetchFunc(func(context.Context, credential) (quotaSnapshot, error) {
		return quotaSnapshot{}, &quotaError{code: "quota_http_401"}
	})
	*now = now.Add(5 * time.Minute)
	e.refreshQuota(context.Background())
	if e.states["a"].Snapshot != nil {
		t.Fatal("revoked credential kept trusted quota")
	}
	if got := e.pick(testRequest("a")); got.Handled {
		t.Fatal("selected revoked credential")
	}
}

func TestRefreshFiltersRoster(t *testing.T) {
	e, host, _, calls := testEngine(t)
	host.entries = append(host.entries,
		pluginapi.HostAuthFileEntry{ID: "codex", AuthIndex: "c", Provider: "codex"},
		pluginapi.HostAuthFileEntry{ID: "disabled", AuthIndex: "d", Provider: "claude", Disabled: true},
		pluginapi.HostAuthFileEntry{ID: "runtime", AuthIndex: "r", Provider: "claude", RuntimeOnly: true},
		pluginapi.HostAuthFileEntry{ID: "custom", AuthIndex: "b", Provider: "claude", BaseURL: "https://elsewhere.test"},
	)
	e.refreshQuota(context.Background())
	if *calls != 2 || len(e.states) != 2 {
		t.Fatalf("unexpected roster: %+v", e.states)
	}
	host.entries[0].Disabled = true
	e.refreshQuota(context.Background())
	if len(e.states) != 1 || e.states["codex"].Provider != "codex" {
		t.Fatal("disabled credential retained or Codex was removed")
	}
}

func TestRefreshReadsClaudeAndCodexWithProviderSpecificCredentials(t *testing.T) {
	cfg, err := decodeConfig([]byte("mode: active"))
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{
		entries: []pluginapi.HostAuthFileEntry{
			{ID: "claude", AuthIndex: "claude-index", Provider: "claude"},
			{ID: "codex", AuthIndex: "codex-index", Provider: "codex"},
		},
		creds: map[string]credential{
			"claude-index": {Type: "claude", AccountUUID: "claude", AccessToken: "claude-token"},
			"codex-index":  {Type: "codex", AccountID: "codex-account", AccessToken: "codex-token"},
		},
	}
	seen := make(map[string]string)
	e := newEngine(host, fetchFunc(func(_ context.Context, c credential) (quotaSnapshot, error) {
		seen[c.Type] = c.AccountUUID + c.AccountID
		snapshot := testSnapshot(c.AccountUUID+c.AccountID, time.Hour)
		snapshot.Provider = c.Type
		snapshot.Identity = c.identity()
		return snapshot, nil
	}), cfg)
	e.refreshQuota(context.Background())
	if seen["claude"] != "claude" || seen["codex"] != "codex-account" || len(e.states) != 2 {
		t.Fatalf("provider credentials were not read correctly: seen=%v states=%v", seen, e.states)
	}
}

func TestRosterReplacementInvalidatesCacheBeforeNextPoll(t *testing.T) {
	e, host, _, calls := testEngine(t)
	e.refreshQuota(context.Background())
	host.entries[0].ModTime = testNow.Add(time.Second)
	host.creds["index-a"] = credential{Type: "claude", AccountUUID: "replacement", AccessToken: "OTHER_TOKEN"}
	e.fetcher = fetchFunc(func(context.Context, credential) (quotaSnapshot, error) {
		return quotaSnapshot{}, errors.New("unavailable")
	})
	e.refreshQuota(context.Background())
	if *calls != 1 || e.states["a"].Snapshot != nil {
		t.Fatal("replacement inherited previous account quota")
	}
	req := testRequest("a")
	req.Candidates[0].Metadata = nil
	if got := e.pick(req); got.Handled {
		t.Fatal("replaced account was routed with old quota")
	}
}

func TestShadowAndWorkerFailureDelegate(t *testing.T) {
	e, _, _, _ := testEngine(t)
	e.refreshQuota(context.Background())
	e.cfg.Mode = "shadow"
	if got := e.pick(testRequest("a")); got.Handled {
		t.Fatal("shadow mode changed routing")
	}
	if e.last.AuthID != "a" || e.routed.Load() != 0 {
		t.Fatal("shadow decision not recorded")
	}
	e.cfg.Mode = "active"
	e.failed.Store(true)
	if got := e.pick(testRequest("a")); got.Handled {
		t.Fatal("failed worker did not delegate")
	}
}

func TestRefreshCancellation(t *testing.T) {
	e, _, _, _ := testEngine(t)
	started := make(chan struct{})
	e.fetcher = fetchFunc(func(ctx context.Context, _ credential) (quotaSnapshot, error) {
		close(started)
		<-ctx.Done()
		return quotaSnapshot{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.refreshQuota(ctx); close(done) }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not cancel")
	}
}

func TestConcurrentRefreshPicksAndStatus(t *testing.T) {
	e, _, _, _ := testEngine(t)
	e.refreshQuota(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				e.pick(testRequest("a"))
				_, _ = json.Marshal(e.status())
				e.refreshQuota(context.Background())
			}
		}()
	}
	wg.Wait()
	if e.routed.Load() != 1200 {
		t.Fatalf("lost decisions: %d", e.routed.Load())
	}
}

func TestRefreshNearResetAndPastReset(t *testing.T) {
	s := testSnapshot("a", 45*time.Second)
	if next := nextRefresh(s, testNow, 5*time.Minute); !next.Equal(testNow.Add(50 * time.Second)) {
		t.Fatalf("missed reset: %v", next)
	}
	s.Weekly.ResetsAt = ptr(testNow.Add(-time.Hour))
	if next := nextRefresh(s, testNow, 5*time.Minute); !next.Equal(testNow.Add(reconcileInterval)) {
		t.Fatalf("past reset hot loop: %v", next)
	}
}

func BenchmarkPick(b *testing.B) {
	cfg, _ := decodeConfig([]byte("mode: active"))
	e := newEngine(nil, nil, cfg)
	e.now = func() time.Time { return testNow }
	req := testRequest("a", "b", "c", "d")
	for i, c := range req.Candidates {
		e.states[c.ID] = accountState{Snapshot: ptr(testSnapshot(c.ID, time.Duration(i+1)*24*time.Hour))}
	}
	b.ReportAllocs()
	for b.Loop() {
		e.pick(req)
	}
}
