package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func registerRequest(t *testing.T, yaml string) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(lifecycleRequest{ConfigYAML: []byte(yaml), SchemaVersion: pluginabi.SchemaVersion})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return raw
}

func TestRegisterDeclaresObserverCapabilities(t *testing.T) {
	defer stopRuntime()
	dir := t.TempDir()
	out, ok := handleMethod(pluginabi.MethodPluginRegister, registerRequest(t, "enabled: true\npriority: 1\nclickhouse-url: http://127.0.0.1:1\nspool-dir: "+dir+"\n"))
	if !ok {
		t.Fatalf("register failed: %s", out)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			SchemaVersion uint32          `json:"schema_version"`
			Metadata      map[string]any  `json:"metadata"`
			Capabilities  map[string]bool `json:"capabilities"`
		} `json:"result"`
	}
	if errDecode := json.Unmarshal(out, &env); errDecode != nil || !env.OK {
		t.Fatalf("envelope = %s", out)
	}
	if env.Result.SchemaVersion != pluginSchemaVersion || env.Result.Metadata["Name"] != "cliproxy-log" {
		t.Fatalf("registration = %+v", env.Result)
	}
	for _, capability := range []string{"request_interceptor", "request_lifecycle_plugin", "response_interceptor", "response_stream_interceptor", "websocket_response_observer", "usage_plugin"} {
		if !env.Result.Capabilities[capability] {
			t.Errorf("capability %s not declared", capability)
		}
	}
	if currentCollector() == nil {
		t.Fatalf("runtime not started")
	}
	// Reconfigure keeps the same runtime and applies the new machines map.
	hash := strings.Repeat("a", 64)
	if _, ok := handleMethod(pluginabi.MethodPluginReconfigure, registerRequest(t, "spool-dir: "+dir+"\nmachines:\n  "+hash+": bigbook\n")); !ok {
		t.Fatalf("reconfigure failed")
	}
	if state.cfg.Load().Machines[hash] != "bigbook" {
		t.Fatalf("machines not applied")
	}
}

func TestRegisterRejectsInvalidConfig(t *testing.T) {
	cases := []string{
		"clickhouse-url: ftp://host\n",
		"clickhouse-url: http://user:pass@host:8123\n",
		"clickhouse-database: \"x; DROP\"\n",
		"max-body-bytes: 0\n",
		"spool-max-bytes: -1\n",
		"machines:\n  not-a-hash: bigbook\n",
		"machines: [1, 2]\n",
	}
	for _, yaml := range cases {
		out, ok := handleMethod(pluginabi.MethodPluginRegister, registerRequest(t, yaml))
		if ok || !strings.Contains(string(out), `"ok":false`) {
			t.Errorf("config %q accepted: %s", yaml, out)
		}
	}
	if currentCollector() != nil {
		t.Fatalf("invalid config must not start the runtime")
	}
}

func TestObservationMethodsReturnUnmodified(t *testing.T) {
	methods := []string{
		pluginabi.MethodRequestInterceptBefore,
		pluginabi.MethodRequestInterceptAfter,
		pluginabi.MethodResponseInterceptAfter,
		pluginabi.MethodResponseInterceptStreamChunk,
		pluginabi.MethodWebSocketResponseEvent,
		pluginabi.MethodUsageHandle,
		pluginabi.MethodRequestComplete,
	}
	// Without a runtime, and with garbage input, every call still succeeds with an
	// empty result, which the host treats as "no change".
	for _, method := range methods {
		out, ok := handleMethod(method, []byte("garbage"))
		if !ok || string(out) != `{"ok":true}` {
			t.Errorf("%s returned %s", method, out)
		}
	}
	if out, ok := handleMethod("bogus.method", nil); ok || !strings.Contains(string(out), "unknown_method") {
		t.Errorf("unknown method returned %s", out)
	}
}

func TestHandleMethodContainsPanics(t *testing.T) {
	defer func() {
		state.mu.Lock()
		state.collector = nil
		state.mu.Unlock()
	}()
	// A collector whose config accessor panics makes enqueue panic inside handleMethod.
	state.mu.Lock()
	state.collector = newCollector(func() *pluginConfig { panic("boom") }, nil, nil)
	state.mu.Unlock()
	before := stats.panics.Load()
	out, ok := handleMethod(pluginabi.MethodRequestInterceptBefore, []byte(`{}`))
	if !ok || string(out) != `{"ok":true}` || stats.panics.Load() != before+1 {
		t.Fatalf("panic escaped or result wrong: %s", out)
	}
}

// A quiesced copy is replaced but never shut down, so it must stop its writer at
// quiesce; a rollback re-registers it and must start a fresh runtime.
func TestQuiesceStopsRuntimeAndRegisterRestartsIt(t *testing.T) {
	defer stopRuntime()
	dir := t.TempDir()
	request := registerRequest(t, "clickhouse-url: http://127.0.0.1:1\nspool-dir: "+dir+"\n")
	if _, ok := handleMethod(pluginabi.MethodPluginRegister, request); !ok {
		t.Fatal("register failed")
	}
	if out, ok := handleMethod(pluginabi.MethodPluginQuiesce, nil); !ok || string(out) != `{"ok":true}` {
		t.Fatalf("quiesce = %s", out)
	}
	if currentCollector() != nil {
		t.Fatalf("runtime still running after quiesce")
	}
	if _, ok := handleMethod(pluginabi.MethodPluginRegister, request); !ok || currentCollector() == nil {
		t.Fatalf("register after quiesce did not restart the runtime")
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if _, ok := handleMethod(pluginabi.MethodPluginRegister, registerRequest(t, "clickhouse-url: http://127.0.0.1:1\nspool-dir: "+dir+"\n")); !ok {
		t.Fatal("register failed")
	}
	handleMethod(pluginabi.MethodPluginShutdown, nil)
	stopRuntime()
	if currentCollector() != nil {
		t.Fatalf("runtime still running")
	}
}
