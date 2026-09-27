package main

import (
	"math"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Rotation state is per provider, bounded by the current candidate set. Changing
// model/eligibility preserves credit for surviving accounts, but never accrues
// catch-up credit for absent accounts. The engine lock serializes every pick.
type weightedRotation map[string]map[string]rotationCredit

type rotationCredit struct {
	identity string
	value    float64
}

func (r *weightedRotation) choose(req pluginapi.SchedulerPickRequest, states map[string]accountState, now time.Time, maxAge time.Duration) decision {
	candidates, reason := eligibleCandidates(req, states, now, maxAge)
	if len(candidates) == 0 {
		return decision{Reason: reason}
	}
	provider := candidates[0].Provider
	if *r == nil {
		*r = make(weightedRotation)
	}
	previous := (*r)[provider]
	credits := make(map[string]rotationCredit, len(candidates))
	weights := make(map[string]float64, len(candidates))
	var total, mean float64
	for _, c := range candidates {
		s := states[c.ID].Snapshot
		w := balanceWeight(*s, req.Model, now)
		if w <= 0 || math.IsNaN(w) || math.IsInf(w, 0) {
			continue
		}
		// Deduplicate defensively: one account must not gain extra voting weight.
		if _, exists := weights[c.ID]; exists {
			continue
		}
		credit := previous[c.ID]
		if credit.identity != s.Identity {
			credit = rotationCredit{identity: s.Identity}
		}
		credits[c.ID], weights[c.ID] = credit, w
		mean += credit.value
		total += w
	}
	if len(credits) == 0 {
		return decision{Reason: "no_known_usable_quota"}
	}
	mean /= float64(len(credits))
	best := ""
	bestCredit := math.Inf(-1)
	for id, credit := range credits {
		weights[id] /= total
		// Recenter after eligibility changes; retained credits stay bounded and
		// proportional shares adapt immediately when fresh quota arrives.
		credit.value = max(-1, min(1, credit.value-mean)) + weights[id]
		credits[id] = credit
		if credit.value > bestCredit || credit.value == bestCredit && (best == "" || id < best) {
			best, bestCredit = id, credit.value
		}
	}
	credit := credits[best]
	credit.value--
	credits[best] = credit
	(*r)[provider] = credits
	return decision{AuthID: best, Reason: "quota_balanced", Shares: weights}
}

// This is a relative request-share heuristic, not an estimate of token cost.
// A 1h floor bounds urgency close to reset. Squared headroom protects low
// balances outside 24h; its exponent eases toward linear inside that window.
// All positive balances remain usable: there is no hard reserve or rate cap.
func balanceWeight(s quotaSnapshot, model string, now time.Time) float64 {
	hours := max(1, s.Weekly.ResetsAt.Sub(now).Hours())
	remaining := 1 - *s.Weekly.Utilization/100
	model = strings.ToLower(model)
	for _, item := range []struct {
		name   string
		window *quotaWindow
	}{{"sonnet", s.Sonnet}, {"opus", s.Opus}} {
		if strings.Contains(model, item.name) && item.window != nil && item.window.Utilization != nil {
			remaining = min(remaining, 1-*item.window.Utilization/100)
		}
	}
	exponent := 1 + min(1, hours/24)
	weight := math.Pow(max(0, remaining), exponent) / math.Pow(hours, 1.5)
	if s.FiveHour != nil && s.FiveHour.Utilization != nil {
		weight *= max(0, 1-*s.FiveHour.Utilization/100)
	}
	return weight
}
