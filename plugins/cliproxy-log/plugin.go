package main

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// pluginSchemaVersion is the oldest host RPC contract that gives this plugin everything
// it needs: lifecycle completion (2), stream chunks without per-chunk request bodies (3),
// WebSocket response events (4), and stream chunks without history (5). Declaring the
// minimum keeps the plugin loadable on any host at or above it.
const pluginSchemaVersion uint32 = 5

// pluginVersion is the plugin release; release builds may override it with
// -ldflags "-X main.pluginVersion=...".
var pluginVersion = "0.1.0"

// proxyVersion overrides the detected proxy version when set with
// -ldflags "-X main.proxyVersion=...". When empty, the version of the proxy module
// this plugin was built from is used, which matches the proxy because both ship
// from the same release.
var proxyVersion = ""

var okEmptyEnvelope = []byte(`{"ok":true}`)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

type capabilities struct {
	RequestInterceptor        bool `json:"request_interceptor"`
	RequestLifecyclePlugin    bool `json:"request_lifecycle_plugin"`
	ResponseInterceptor       bool `json:"response_interceptor"`
	StreamChunkInterceptor    bool `json:"response_stream_interceptor"`
	WebSocketResponseObserver bool `json:"websocket_response_observer"`
	UsagePlugin               bool `json:"usage_plugin"`
}

// runtimeState owns the background collector and writer. It starts on the first
// successful plugin.register and stops on shutdown.
type runtimeState struct {
	mu        sync.Mutex
	cfg       atomic.Pointer[pluginConfig]
	collector *collector
	writer    *writer
	stopStats chan struct{}
	statsDone chan struct{}
}

var state runtimeState

// handleMethod dispatches one RPC call. Observation methods only enqueue a copy of the
// payload and return an empty result, which the host treats as "no modification".
// It never panics: every failure becomes an envelope.
func handleMethod(method string, request []byte) (out []byte, ok bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			stats.panics.Add(1)
			out, ok = okEmptyEnvelope, true
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return errorEnvelope("invalid_config", errConfigure.Error()), false
		}
		raw, errMarshal := okEnvelope(pluginRegistration())
		if errMarshal != nil {
			return errorEnvelope("plugin_error", errMarshal.Error()), false
		}
		return raw, true
	case pluginabi.MethodPluginQuiesce:
		// The host quiesces this copy before a replacement library takes over and
		// then keeps it loaded without shutting it down. Stopping here leaves one
		// writer replaying the spool; a rollback calls plugin.register, which starts
		// the runtime again.
		stopRuntime()
		return okEmptyEnvelope, true
	case pluginabi.MethodPluginShutdown:
		stopRuntime()
		return okEmptyEnvelope, true
	case pluginabi.MethodRequestInterceptBefore,
		pluginabi.MethodRequestInterceptAfter,
		pluginabi.MethodResponseInterceptAfter,
		pluginabi.MethodResponseInterceptStreamChunk,
		pluginabi.MethodWebSocketResponseEvent,
		pluginabi.MethodUsageHandle,
		pluginabi.MethodRequestComplete:
		if c := currentCollector(); c != nil {
			c.enqueue(method, request)
		}
		return okEmptyEnvelope, true
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), false
	}
}

func currentCollector() *collector {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.collector
}

// configure parses the per-plugin YAML and starts or updates the runtime.
func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
		}
	}
	if req.SchemaVersion != 0 && req.SchemaVersion < pluginSchemaVersion {
		return fmt.Errorf("host schema version %d is older than required %d", req.SchemaVersion, pluginSchemaVersion)
	}
	cfg, errParse := parseConfig(req.ConfigYAML)
	if errParse != nil {
		return errParse
	}
	startRuntime(cfg)
	return nil
}

func startRuntime(cfg *pluginConfig) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.cfg.Store(cfg)
	if state.collector != nil {
		state.writer.reconfigure()
		return
	}
	state.writer = newWriter(state.cfg.Load, time.Now)
	state.collector = newCollector(state.cfg.Load, state.writer.add, time.Now)
	state.writer.start()
	state.collector.start()
	state.stopStats = make(chan struct{})
	state.statsDone = make(chan struct{})
	go logStatsLoop(state.stopStats, state.statsDone, state.writer)
	logInfo("cliproxy-log started", map[string]any{"plugin_version": pluginVersion, "proxy_version": resolvedProxyVersion()})
}

// stopRuntime finalizes in-flight requests and flushes or spools pending rows.
// It is bounded in time so host shutdown is never held for long.
func stopRuntime() {
	state.mu.Lock()
	c, w, stopStatsCh, statsDone := state.collector, state.writer, state.stopStats, state.statsDone
	state.collector, state.writer, state.stopStats, state.statsDone = nil, nil, nil, nil
	state.mu.Unlock()
	if c == nil {
		return
	}
	c.stop()
	w.stop(10 * time.Second)
	close(stopStatsCh)
	<-statsDone
	logStats(w, true)
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginSchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "cliproxy-log",
			Version:          pluginVersion,
			Author:           "loadinglucian",
			GitHubRepository: "https://github.com/loadinglucian/cliproxy",
			ConfigFields:     configFields(),
		},
		Capabilities: capabilities{
			RequestInterceptor:        true,
			RequestLifecyclePlugin:    true,
			ResponseInterceptor:       true,
			StreamChunkInterceptor:    true,
			WebSocketResponseObserver: true,
			UsagePlugin:               true,
		},
	}
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, errMarshal := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if errMarshal != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error"}}`)
	}
	return raw
}

var (
	proxyVersionOnce  sync.Once
	proxyVersionValue string
)

// resolvedProxyVersion returns the ldflags override or the proxy module version
// recorded in this binary's build info, without a leading "v".
func resolvedProxyVersion() string {
	proxyVersionOnce.Do(func() {
		version := strings.TrimSpace(proxyVersion)
		if version == "" {
			if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
				version = info.Main.Version
			}
		}
		proxyVersionValue = strings.TrimPrefix(version, "v")
	})
	return proxyVersionValue
}

// counters are process-wide, monotonically increasing, and never contain content.
type counters struct {
	rowsWritten   atomic.Uint64
	rowsSpooled   atomic.Uint64
	rowsDropped   atomic.Uint64
	rowsRejected  atomic.Uint64
	eventsDropped atomic.Uint64
	captureErrors atomic.Uint64
	abandoned     atomic.Uint64
	panics        atomic.Uint64
}

var stats counters

func (c *counters) snapshot() map[string]any {
	return map[string]any{
		"rows_written":   c.rowsWritten.Load(),
		"rows_spooled":   c.rowsSpooled.Load(),
		"rows_dropped":   c.rowsDropped.Load(),
		"rows_rejected":  c.rowsRejected.Load(),
		"events_dropped": c.eventsDropped.Load(),
		"capture_errors": c.captureErrors.Load(),
		"abandoned":      c.abandoned.Load(),
		"panics":         c.panics.Load(),
	}
}

const statsInterval = time.Minute

func logStatsLoop(stop <-chan struct{}, done chan<- struct{}, w *writer) {
	defer close(done)
	defer func() {
		if recovered := recover(); recovered != nil {
			stats.panics.Add(1)
		}
	}()
	ticker := time.NewTicker(statsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			logStats(w, false)
		}
	}
}

var lastLoggedStats atomic.Pointer[string]

// logStats writes the counters through the host log when they changed since the last
// line (or always when forced). Losses are logged at warn level.
func logStats(w *writer, force bool) {
	fields := stats.snapshot()
	key := fmt.Sprint(fields)
	if previous := lastLoggedStats.Load(); !force && previous != nil && *previous == key {
		return
	}
	lastLoggedStats.Store(&key)
	if w != nil {
		fields["spool_bytes"] = w.spoolBytes()
	}
	level := "info"
	if stats.rowsDropped.Load() > 0 || stats.eventsDropped.Load() > 0 || stats.panics.Load() > 0 {
		level = "warn"
	}
	logAt(level, "cliproxy-log stats", fields)
}

// hostLogger sends a host.log payload; it is set at plugin init and left nil in tests.
var hostLogger func([]byte)

type hostLogRequest struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

func logInfo(message string, fields map[string]any) { logAt("info", message, fields) }

func logWarn(message string, fields map[string]any) { logAt("warn", message, fields) }

func logAt(level, message string, fields map[string]any) {
	defer func() { _ = recover() }()
	if hostLogger == nil {
		return
	}
	raw, errMarshal := json.Marshal(hostLogRequest{Level: level, Message: message, Fields: fields})
	if errMarshal != nil {
		return
	}
	hostLogger(raw)
}
