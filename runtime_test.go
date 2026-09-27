package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestConfigValidation(t *testing.T) {
	for _, raw := range []string{"", "mode: active", "enabled: true\npriority: 10\nmode: shadow", "poll_interval: 1m\nmax_age: 2m\nrequest_timeout: 1s"} {
		if _, err := decodeConfig([]byte(raw)); err != nil {
			t.Fatalf("valid config %q: %v", raw, err)
		}
	}
	for _, raw := range []string{"mode: wrong", "poll_interval: 1s", "poll_interval: 2h", "max_age: 1m", "max_age: 2h", "request_timeout: 0s", "request_timeout: 31s", "poll_interval: invalid"} {
		if _, err := decodeConfig([]byte(raw)); err == nil {
			t.Fatalf("invalid config accepted: %q", raw)
		}
	}
	cfg, _ := decodeConfig(nil)
	if cfg.Mode != "shadow" {
		t.Fatal("default must not alter routing")
	}
	if cfg.SelectionPolicy != "quota_balanced" {
		t.Fatal("default must balance quota")
	}
	if _, err := decodeConfig([]byte("selection_policy: wrong")); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if _, err := decodeConfig([]byte("selection_policy: weekly_reset_first")); err != nil {
		t.Fatal(err)
	}
}

func decodeEnvelope(t *testing.T, raw []byte, result any) pluginabi.Envelope {
	t.Helper()
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if result != nil && envelope.OK {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			t.Fatal(err)
		}
	}
	return envelope
}

func TestRuntimeRegistrationReconfigureShutdown(t *testing.T) {
	p := &pluginRuntime{host: &fakeHost{}}
	defer p.stop()
	var reg struct {
		SchemaVersion uint32          `json:"schema_version"`
		Capabilities  map[string]bool `json:"capabilities"`
	}
	if env := decodeEnvelope(t, p.handle(pluginabi.MethodPluginRegister, []byte(`{}`)), &reg); !env.OK {
		t.Fatalf("register: %s", env.Error)
	}
	if reg.SchemaVersion != 6 || !reg.Capabilities["scheduler"] || !reg.Capabilities["management_api"] || len(reg.Capabilities) != 2 {
		t.Fatalf("unexpected capabilities: %+v", reg)
	}
	if p.active.Load().cfg.Mode != "shadow" {
		t.Fatal("unsafe startup mode")
	}
	old := p.active.Load()
	if env := decodeEnvelope(t, p.handle(pluginabi.MethodPluginReconfigure, []byte(`{"config_yaml":"`+base64.StdEncoding.EncodeToString([]byte("mode: invalid"))+`"}`)), nil); env.OK {
		t.Fatal("invalid configuration accepted")
	}
	if p.active.Load() != old {
		t.Fatal("invalid reconfigure interrupted working instance")
	}
	request, _ := json.Marshal(map[string][]byte{"config_yaml": []byte("mode: active")})
	if env := decodeEnvelope(t, p.handle(pluginabi.MethodPluginReconfigure, request), nil); !env.OK {
		t.Fatalf("reconfigure: %s", env.Error)
	}
	if p.active.Load().cfg.Mode != "active" || p.active.Load() == old {
		t.Fatal("reconfigure did not replace worker")
	}
	p.handle(pluginabi.MethodPluginQuiesce, nil)
	if p.active.Load() != nil {
		t.Fatal("quiesce did not stop")
	}
	p.stop()
	var pick pluginapi.SchedulerPickResponse
	decodeEnvelope(t, p.handle(pluginabi.MethodSchedulerPick, []byte(`{}`)), &pick)
	if pick.Handled {
		t.Fatal("stopped runtime handled pick")
	}
}

func TestRuntimeManagementIsReadOnlyAndSanitized(t *testing.T) {
	p := &pluginRuntime{}
	e, _, _, _ := testEngine(t)
	p.active.Store(e)
	request, _ := json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: statusPath})
	var response pluginapi.ManagementResponse
	decodeEnvelope(t, p.handle(pluginabi.MethodManagementHandle, request), &response)
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), `"mode":"active"`) {
		t.Fatalf("bad status response: %s", response.Body)
	}
	if strings.Contains(string(response.Body), "SECRET_TOKEN") {
		t.Fatal("secret leaked")
	}
	for _, method := range []string{"POST", "PATCH", "DELETE"} {
		request, _ = json.Marshal(pluginapi.ManagementRequest{Method: method, Path: statusPath})
		decodeEnvelope(t, p.handle(pluginabi.MethodManagementHandle, request), &response)
		if response.StatusCode != 404 {
			t.Fatal("mutating route exposed")
		}
	}
	var pick pluginapi.SchedulerPickResponse
	if env := decodeEnvelope(t, p.handle(pluginabi.MethodSchedulerPick, []byte(`broken`)), &pick); !env.OK || pick.Handled {
		t.Fatal("malformed pick should delegate")
	}
}

func TestConcurrentReconfigureAndPicks(t *testing.T) {
	p := &pluginRuntime{host: &fakeHost{}}
	defer p.stop()
	cfg, _ := decodeConfig(nil)
	p.configure(cfg)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				p.handle(pluginabi.MethodSchedulerPick, []byte(`{"Provider":"claude"}`))
			}
		}()
	}
	for i := 0; i < 10; i++ {
		p.configure(cfg)
	}
	wg.Wait()
	done := make(chan struct{})
	go func() { p.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown deadlocked")
	}
}
