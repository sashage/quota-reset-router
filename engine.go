package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const reconcileInterval = 30 * time.Second

type authHost interface {
	List() ([]pluginapi.HostAuthFileEntry, error)
	Get(string) (json.RawMessage, error)
}

type accountState struct {
	Index       string         `json:"auth_index"`
	Provider    string         `json:"provider"`
	Stamp       string         `json:"-"`
	Snapshot    *quotaSnapshot `json:"quota,omitempty"`
	LastError   string         `json:"last_error,omitempty"`
	NextRefresh time.Time      `json:"next_refresh"`
	Failures    int            `json:"consecutive_failures"`
}

type decision struct {
	AuthID string             `json:"auth_id,omitempty"`
	Reason string             `json:"reason"`
	Shares map[string]float64 `json:"traffic_shares,omitempty"`
}

type engine struct {
	host     authHost
	fetcher  quotaFetcher
	cfg      config
	now      func() time.Time
	mu       sync.RWMutex
	states   map[string]accountState
	listErr  string
	last     decision
	picks    atomic.Uint64
	routed   atomic.Uint64
	failed   atomic.Bool
	refresh  sync.Mutex
	rotation weightedRotation // guarded by mu, like states and last
}

func newEngine(host authHost, fetcher quotaFetcher, cfg config) *engine {
	return &engine{host: host, fetcher: fetcher, cfg: cfg, now: time.Now, states: make(map[string]accountState)}
}

func (e *engine) run(ctx context.Context) {
	e.refreshQuota(ctx)
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.refreshQuota(ctx)
		}
	}
}

func (e *engine) refreshQuota(ctx context.Context) {
	e.refresh.Lock()
	defer e.refresh.Unlock()
	if ctx.Err() != nil {
		return
	}
	entries, err := e.host.List()
	if err != nil {
		e.mu.Lock()
		e.listErr = "credential_list_failed"
		e.mu.Unlock()
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	eligible := make(map[string]bool)
	e.mu.Lock()
	e.listErr = ""
	for _, entry := range entries {
		provider := entry.Provider
		if provider == "" {
			provider = entry.Type
		}
		provider = strings.ToLower(strings.TrimSpace(provider))
		if (provider != "claude" && provider != "codex") || entry.Disabled || entry.Status == "disabled" || entry.RuntimeOnly || entry.ID == "" || entry.AuthIndex == "" || entry.BaseURL != "" {
			continue
		}
		eligible[entry.ID] = true
		state := e.states[entry.ID]
		stamp := fmt.Sprintf("%q:%q:%q:%d:%d", entry.Path, entry.Account, entry.Email, entry.Size, entry.ModTime.UnixNano())
		if state.Index != entry.AuthIndex || state.Provider != provider || state.Stamp != stamp {
			state = accountState{Index: entry.AuthIndex, Provider: provider, Stamp: stamp}
		}
		e.states[entry.ID] = state
	}
	for id := range e.states {
		if !eligible[id] {
			delete(e.states, id)
		}
	}
	e.mu.Unlock()
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if !eligible[entry.ID] {
			continue
		}
		provider := entry.Provider
		if provider == "" {
			provider = entry.Type
		}
		provider = strings.ToLower(strings.TrimSpace(provider))
		e.mu.RLock()
		state := e.states[entry.ID]
		e.mu.RUnlock()
		if e.now().Before(state.NextRefresh) {
			continue
		}
		snap, err := e.readQuota(ctx, entry.AuthIndex, provider)
		if ctx.Err() != nil {
			return
		}
		now := e.now()
		if err != nil {
			state.Failures++
			state.LastError = "quota_refresh_failed"
			var qerr *quotaError
			delay := time.Minute * time.Duration(1<<min(state.Failures-1, 4))
			if errors.As(err, &qerr) {
				state.LastError = qerr.code
				delay = max(delay, qerr.retryAfter)
				if qerr.code == "quota_http_401" || qerr.code == "quota_http_403" {
					state.Snapshot = nil
				}
			}
			state.NextRefresh = now.Add(delay)
		} else {
			state.Snapshot = &snap
			state.LastError = ""
			state.Failures = 0
			state.NextRefresh = nextRefresh(snap, now, e.cfg.PollInterval)
		}
		e.mu.Lock()
		e.states[entry.ID] = state
		e.mu.Unlock()
	}
}

func (e *engine) readQuota(ctx context.Context, index, provider string) (quotaSnapshot, error) {
	raw, err := e.host.Get(index)
	if err != nil {
		return quotaSnapshot{}, errors.New("credential_read_failed")
	}
	var cred credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return quotaSnapshot{}, errors.New("credential_decode_failed")
	}
	if strings.TrimSpace(cred.Type) == "" {
		cred.Type = provider
	}
	ctx, cancel := context.WithTimeout(ctx, e.cfg.RequestTimeout)
	defer cancel()
	return e.fetcher.Fetch(ctx, cred)
}

func nextRefresh(s quotaSnapshot, now time.Time, interval time.Duration) time.Time {
	next := now.Add(interval)
	for _, w := range []*quotaWindow{&s.Weekly, s.FiveHour, s.Sonnet, s.Opus} {
		if w != nil && w.ResetsAt != nil && w.ResetsAt.Add(5*time.Second).Before(next) {
			next = w.ResetsAt.Add(5 * time.Second)
		}
	}
	return maxTime(next, now.Add(reconcileInterval))
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func providerOnly(req pluginapi.SchedulerPickRequest, provider string) bool {
	if req.Provider != provider && !(req.Provider == "" && len(req.Providers) == 1 && req.Providers[0] == provider) {
		return false
	}
	for _, p := range req.Providers {
		if p != provider {
			return false
		}
	}
	for _, c := range req.Candidates {
		if c.Provider != provider {
			return false
		}
	}
	return true
}

func candidateIdentity(c pluginapi.SchedulerAuthCandidate) string {
	account, _ := c.Metadata["account_uuid"].(string)
	org, _ := c.Metadata["organization_uuid"].(string)
	token, _ := c.Metadata["access_token"].(string)
	if account == "" && token == "" {
		return ""
	}
	return (credential{Type: c.Provider, AccountUUID: account, Organization: org, AccessToken: token}).identity()
}

func choose(req pluginapi.SchedulerPickRequest, states map[string]accountState, now time.Time, maxAge time.Duration) decision {
	candidates, reason := eligibleCandidates(req, states, now, maxAge)
	var best *pluginapi.SchedulerAuthCandidate
	var reset time.Time
	for i := range candidates {
		c := &candidates[i]
		r := *states[c.ID].Snapshot.Weekly.ResetsAt
		if best == nil || r.Before(reset) || r.Equal(reset) && c.ID < best.ID {
			best, reset = c, r
		}
	}
	if best == nil {
		return decision{Reason: reason}
	}
	return decision{AuthID: best.ID, Reason: "earliest_weekly_reset"}
}

// Both policies honor the same host eligibility, identity and priority rules.
func eligibleCandidates(req pluginapi.SchedulerPickRequest, states map[string]accountState, now time.Time, maxAge time.Duration) ([]pluginapi.SchedulerAuthCandidate, string) {
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider == "" && len(req.Providers) == 1 {
		provider = strings.ToLower(strings.TrimSpace(req.Providers[0]))
	}
	if provider != "claude" && provider != "codex" || !providerOnly(req, provider) {
		return nil, "provider_not_exclusively_supported"
	}
	var candidates []pluginapi.SchedulerAuthCandidate
	for i := range req.Candidates {
		c := &req.Candidates[i]
		if c.ID == "" || strings.EqualFold(c.Status, "disabled") {
			continue
		}
		if disabled, _ := c.Metadata["disabled"].(bool); disabled {
			continue
		}
		s := states[c.ID].Snapshot
		identity := candidateIdentity(*c)
		// CPA 7.3.15 omits candidate metadata; roster changes invalidate cached credentials instead.
		stateProvider := states[c.ID].Provider
		if stateProvider == "" {
			stateProvider = provider
		}
		if stateProvider != provider || s == nil || s.Identity == "" || identity != "" && s.Identity != identity || s.availability(req.Model, now, maxAge) != "ready" || s.Weekly.ResetsAt == nil {
			continue
		}
		r := *s.Weekly.ResetsAt
		if !r.After(now) {
			continue
		}
		if len(candidates) > 0 && c.Priority > candidates[0].Priority {
			candidates = candidates[:0]
		}
		if len(candidates) == 0 || c.Priority == candidates[0].Priority {
			candidates = append(candidates, *c)
		}
	}
	return candidates, "no_known_usable_quota"
}

func (e *engine) pick(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	if e.failed.Load() {
		return pluginapi.SchedulerPickResponse{}
	}
	e.mu.Lock()
	var d decision
	if e.cfg.SelectionPolicy == "quota_balanced" {
		d = e.rotation.choose(req, e.states, e.now(), e.cfg.MaxAge)
	} else {
		d = choose(req, e.states, e.now(), e.cfg.MaxAge)
	}
	e.last = d
	e.mu.Unlock()
	e.picks.Add(1)
	if e.cfg.Mode != "active" || d.AuthID == "" {
		return pluginapi.SchedulerPickResponse{}
	}
	e.routed.Add(1)
	return pluginapi.SchedulerPickResponse{AuthID: d.AuthID, Handled: true}
}

func (e *engine) status() any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	accounts := make(map[string]accountState, len(e.states))
	for id, state := range e.states {
		accounts[id] = state
	}
	return struct {
		Version      string                  `json:"version"`
		Policy       string                  `json:"selection_policy"`
		Mode         string                  `json:"mode"`
		PollInterval string                  `json:"poll_interval"`
		MaxAge       string                  `json:"max_age"`
		Accounts     map[string]accountState `json:"accounts"`
		ListError    string                  `json:"list_error,omitempty"`
		LastDecision decision                `json:"last_decision"`
		Picks        uint64                  `json:"picks"`
		Routed       uint64                  `json:"routed"`
		WorkerFailed bool                    `json:"worker_failed"`
	}{pluginVersion, e.cfg.SelectionPolicy, e.cfg.Mode, e.cfg.PollInterval.String(), e.cfg.MaxAge.String(), accounts, e.listErr, e.last, e.picks.Load(), e.routed.Load(), e.failed.Load()}
}
