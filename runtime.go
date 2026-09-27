package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const statusPath = "/v0/management/plugins/" + pluginID + "/status"

type pluginRuntime struct {
	host    authHost
	mu      sync.Mutex
	active  atomic.Pointer[engine]
	cancel  context.CancelFunc
	done    chan struct{}
	fetcher *httpQuotaFetcher
}

func success(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		return failure("response_encode_failed")
	}
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func failure(code string) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{Error: &pluginabi.Error{Code: code, Message: code}})
	return raw
}

func (p *pluginRuntime) handle(method string, raw []byte) []byte {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req); err != nil {
				return failure("invalid_lifecycle_request")
			}
		}
		cfg, err := decodeConfig(req.ConfigYAML)
		if err != nil {
			return failure("invalid_configuration")
		}
		if p.host == nil {
			return failure("host_unavailable")
		}
		p.configure(cfg)
		return success(registration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		p.stop()
		return success(struct{}{})
	case pluginabi.MethodSchedulerPick:
		var req pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return success(pluginapi.SchedulerPickResponse{})
		}
		if e := p.active.Load(); e != nil {
			return success(e.pick(req))
		}
		return success(pluginapi.SchedulerPickResponse{})
	case pluginabi.MethodManagementRegister:
		return success(pluginapi.ManagementRegistrationResponse{Routes: []pluginapi.ManagementRoute{{Method: http.MethodGet, Path: statusPath}}})
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := json.Unmarshal(raw, &req); err != nil || req.Method != http.MethodGet || req.Path != statusPath {
			return success(pluginapi.ManagementResponse{StatusCode: http.StatusNotFound})
		}
		var status any = map[string]string{"state": "stopped"}
		if e := p.active.Load(); e != nil {
			status = e.status()
		}
		body, _ := json.Marshal(status)
		return success(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: body})
	default:
		return failure("unsupported_method")
	}
}

func (p *pluginRuntime) configure(cfg config) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
	p.fetcher = newHTTPFetcher()
	e := newEngine(p.host, p.fetcher, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	done := p.done
	p.active.Store(e)
	go func() {
		defer close(done)
		defer func() {
			if recover() != nil {
				e.failed.Store(true)
			}
		}()
		e.run(ctx)
	}()
}

func (p *pluginRuntime) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
}

func (p *pluginRuntime) stopLocked() {
	p.active.Store(nil)
	if p.cancel != nil {
		p.cancel()
		<-p.done
		p.cancel, p.done = nil, nil
	}
	if p.fetcher != nil {
		p.fetcher.client.CloseIdleConnections()
		p.fetcher = nil
	}
}

func registration() any {
	return struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  map[string]bool    `json:"capabilities"`
	}{
		pluginabi.SchemaVersion,
		pluginapi.Metadata{Name: pluginID, Version: pluginVersion, Author: "Shreyash (webdevcaptain); sashage fork", GitHubRepository: "https://github.com/sashage/quota-reset-router", ConfigFields: []pluginapi.ConfigField{
			{Name: "mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"shadow", "active"}, Description: "Shadow records decisions without changing routing. Active applies the selection policy."},
			{Name: "selection_policy", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"quota_balanced", "weekly_reset_first"}, Description: "Default quota_balanced spreads requests using quota headroom and reset urgency. weekly_reset_first preserves upstream behavior."},
			{Name: "poll_interval", Type: pluginapi.ConfigFieldTypeString, Description: "Quota refresh interval, 1m to 1h. Default 5m."},
			{Name: "max_age", Type: pluginapi.ConfigFieldTypeString, Description: "Maximum quota cache age. Default 10m."},
			{Name: "request_timeout", Type: pluginapi.ConfigFieldTypeString, Description: "Background quota request timeout, 1s to 30s. Default 10s."},
		}},
		map[string]bool{"scheduler": true, "management_api": true},
	}
}
