package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	testKey      = "client-key-bigbook"
	testOtherKey = "client-key-other"
)

// harness drives a collector synchronously with a controllable clock.
type harness struct {
	t     *testing.T
	cfg   *pluginConfig
	clock time.Time
	c     *collector
	rows  [][]byte
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, cfg: defaultConfig(), clock: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	h.cfg.Machines[hashKey(testKey)] = "bigbook"
	h.c = newCollector(func() *pluginConfig { return h.cfg }, func(raw []byte) { h.rows = append(h.rows, raw) }, func() time.Time { return h.clock })
	return h
}

func (h *harness) feed(method string, v any) {
	h.t.Helper()
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		h.t.Fatal(errMarshal)
	}
	h.c.process(event{method: method, raw: raw, at: h.clock})
}

func (h *harness) advance(d time.Duration) { h.clock = h.clock.Add(d) }

func (h *harness) row(i int) row {
	h.t.Helper()
	if i >= len(h.rows) {
		h.t.Fatalf("only %d rows written", len(h.rows))
	}
	var r row
	if errDecode := json.Unmarshal(h.rows[i], &r); errDecode != nil {
		h.t.Fatal(errDecode)
	}
	return r
}

func (h *harness) request(id, trace, path string, headers http.Header, body string) {
	h.feed(pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{
		RequestID: id, TraceID: trace, SourceFormat: "claude", Model: "claude-alias", RequestedModel: "claude-alias",
		Headers: headers, Body: []byte(body),
		Metadata: map[string]any{"request_path": path, "reasoning_effort": "high"},
	})
	h.feed(pluginabi.MethodRequestInterceptAfter, pluginapi.RequestInterceptRequest{
		RequestID: id, TraceID: trace, SourceFormat: "claude", ToFormat: "claude", Model: "claude-real", RequestedModel: "claude-alias",
		Headers: headers, Body: []byte(strings.Replace(body, "claude-alias", "claude-real", 1)),
	})
}

func (h *harness) complete(id string, outcome pluginapi.RequestCompletionOutcome, status int) {
	h.feed(pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{
		RequestID: id, Outcome: outcome, StatusCode: status,
		StartedAt: h.clock.Add(-1500 * time.Millisecond), CompletedAt: h.clock,
	})
}

func usageRecord(trace, model string, failed bool) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		RequestID: "usage-attempt", TraceID: trace, Provider: "claude", Model: model, APIKey: testKey,
		AuthID: "claude-user.json", AuthType: "oauth", Source: "user@example.com", SessionID: "sess-1",
		ServiceTier: "priority", Failed: failed,
		Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheCreationTokens: 40, ReasoningTokens: 5},
	}
}

func TestCollectorNonStreamRow(t *testing.T) {
	h := newHarness(t)
	headers := http.Header{"X-Api-Key": {testKey}, "Anthropic-Version": {"2023-06-01"}}
	h.request("r1", "t1", "/v1/messages", headers, `{"model":"claude-alias","messages":[]}`)
	h.feed(pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		RequestID: "r1", StatusCode: 200, Body: readFixture(t, "anthropic_message.json"),
		ResponseHeaders: http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"x=y"}},
	})
	h.feed(pluginabi.MethodUsageHandle, usageRecord("t1", "claude-real", false))
	if len(h.rows) != 0 {
		t.Fatalf("row written before completion")
	}
	h.complete("r1", pluginapi.RequestCompletionSucceeded, 200)
	if len(h.rows) != 1 {
		t.Fatalf("rows = %d", len(h.rows))
	}
	r := h.row(0)
	if r.RequestID != "r1" || r.TraceID != "t1" || r.Endpoint != "/v1/messages" || r.Transport != "http" {
		t.Fatalf("identity wrong: %+v", r)
	}
	if r.Machine != "bigbook" || r.ClientKeyID != hashKey(testKey)[:12] {
		t.Fatalf("attribution wrong: %s %s", r.Machine, r.ClientKeyID)
	}
	if r.RequestedModel != "claude-alias" || r.ServedModel != "claude-real" || r.ResponseModel != "claude-test-model" {
		t.Fatalf("models wrong: %+v", r)
	}
	if r.Provider != "claude" || r.Account != "user@example.com" || r.AuthID != "claude-user.json" || r.SessionID != "sess-1" {
		t.Fatalf("usage fields wrong: %+v", r)
	}
	if r.InputTokens != 10 || r.OutputTokens != 20 || r.CacheReadTokens != 30 || r.CacheWriteTokens != 40 || r.ReasoningTokens != 5 {
		t.Fatalf("tokens wrong: %+v", r)
	}
	if r.ReasoningEffort != "high" || r.ServiceTier != "priority" {
		t.Fatalf("effort/tier wrong: %s %s", r.ReasoningEffort, r.ServiceTier)
	}
	if r.StatusCode != 200 || r.Outcome != "succeeded" || r.DurationMS != 1500 || r.Stream {
		t.Fatalf("status wrong: %+v", r)
	}
	if r.StartedAt != "2026-11-08 11:59:58.500" || r.CompletedAt != "2026-11-08 12:00:00.000" {
		t.Fatalf("times wrong: %s %s", r.StartedAt, r.CompletedAt)
	}
	if r.ResponseKind != kindJSON || !strings.Contains(r.Response, "non-streaming reply") {
		t.Fatalf("response wrong: %s %s", r.ResponseKind, r.Response)
	}
	if !strings.Contains(r.ClientRequest, "claude-alias") || !strings.Contains(r.UpstreamRequest, "claude-real") {
		t.Fatalf("bodies wrong: %s / %s", r.ClientRequest, r.UpstreamRequest)
	}
	if _, ok := r.RequestHeaders["x-api-key"]; ok || r.RequestHeaders["anthropic-version"] != "2023-06-01" {
		t.Fatalf("request headers wrong: %v", r.RequestHeaders)
	}
	if _, ok := r.ResponseHeaders["set-cookie"]; ok {
		t.Fatalf("response headers kept set-cookie")
	}
	if bytes.Contains(h.rows[0], []byte(testKey)) {
		t.Fatalf("raw client key stored in row")
	}
	if len(h.c.entries) != 0 || len(h.c.byTrace) != 0 {
		t.Fatalf("entry not removed")
	}
}

func TestCollectorStreamRowWithLateUsage(t *testing.T) {
	h := newHarness(t)
	h.request("r1", "t1", "/v1/messages", http.Header{"Authorization": {"Bearer " + testOtherKey}}, `{"stream":true}`)
	h.feed(pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{
		RequestID: "r1", ChunkIndex: pluginapi.StreamChunkHeaderInitIndex,
		ResponseHeaders: http.Header{"Content-Type": {"text/event-stream"}},
	})
	for i, block := range bytes.SplitAfter(readFixture(t, "anthropic_stream.sse"), []byte("\n\n")) {
		h.feed(pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{RequestID: "r1", ChunkIndex: i, Body: block})
	}
	h.complete("r1", pluginapi.RequestCompletionSucceeded, 200)
	if len(h.rows) != 0 {
		t.Fatalf("row written before usage arrived or grace expired")
	}
	h.advance(time.Second)
	h.feed(pluginabi.MethodUsageHandle, usageRecord("t1", "claude-real", false))
	h.c.sweep(false)
	if len(h.rows) != 1 {
		t.Fatalf("rows = %d", len(h.rows))
	}
	r := h.row(0)
	if r.ResponseKind != kindJSON || !strings.Contains(r.Response, `"location":"San Francisco, CA"`) {
		t.Fatalf("stream not reassembled: %s", r.Response)
	}
	if r.ResponseHeaders["content-type"] != "text/event-stream" {
		t.Fatalf("stream headers missing: %v", r.ResponseHeaders)
	}
	// The usage record carries the authenticated key, which overrides the header guess.
	if r.Machine != "bigbook" {
		t.Fatalf("machine = %s", r.Machine)
	}
	if len(r.CaptureErrors) != 0 {
		t.Fatalf("capture errors: %v", r.CaptureErrors)
	}
}

func TestCollectorWritesAfterUsageGrace(t *testing.T) {
	h := newHarness(t)
	h.request("r1", "t1", "/v1/messages", http.Header{"Authorization": {"Bearer " + testOtherKey}}, `{}`)
	h.feed(pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{RequestID: "r1", StatusCode: 200, Body: readFixture(t, "anthropic_message.json")})
	h.complete("r1", pluginapi.RequestCompletionSucceeded, 200)
	h.advance(h.cfg.UsageGrace - time.Millisecond)
	h.c.sweep(false)
	if len(h.rows) != 0 {
		t.Fatalf("written before grace expired")
	}
	h.advance(time.Millisecond)
	h.c.sweep(false)
	r := h.row(0)
	// Without a usage record, tokens come from the response body.
	if r.InputTokens != 25 || r.OutputTokens != 11 || r.Machine != "unknown" || r.ClientKeyID != hashKey(testOtherKey)[:12] {
		t.Fatalf("fallback fields wrong: %+v", r)
	}
}

func TestCollectorWebSocketTurnsShareTrace(t *testing.T) {
	h := newHarness(t)
	ws := http.Header{"Upgrade": {"websocket"}, "Authorization": {"Bearer " + testKey}}
	h.request("turn1", "conn", "/v1/responses", ws, `{"input":"one"}`)
	h.feed(pluginabi.MethodWebSocketResponseEvent, pluginapi.WebSocketResponseEvent{
		RequestID: "turn1", Provider: "codex", AuthID: "codex.json", AuthLabel: "work",
		Payload: []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5","usage":{"input_tokens":1,"output_tokens":2}}}`),
	})
	h.complete("turn1", pluginapi.RequestCompletionSucceeded, 200)
	h.request("turn2", "conn", "/v1/responses", ws, `{"input":"two"}`)

	first := usageRecord("conn", "claude-real", false)
	first.Detail.OutputTokens = 111
	h.feed(pluginabi.MethodUsageHandle, first)
	h.complete("turn2", pluginapi.RequestCompletionSucceeded, 200)
	second := usageRecord("conn", "claude-real", false)
	second.Detail.OutputTokens = 222
	h.feed(pluginabi.MethodUsageHandle, second)
	h.c.sweep(false)

	if len(h.rows) != 2 {
		t.Fatalf("rows = %d", len(h.rows))
	}
	got := map[string]row{}
	for i := range h.rows {
		r := h.row(i)
		got[r.RequestID] = r
	}
	if got["turn1"].OutputTokens != 111 || got["turn2"].OutputTokens != 222 {
		t.Fatalf("usage matched to the wrong turn: %d %d", got["turn1"].OutputTokens, got["turn2"].OutputTokens)
	}
	t1 := got["turn1"]
	if t1.Transport != "websocket" || t1.ResponseKind != kindJSON || !strings.Contains(t1.Response, "resp_1") {
		t.Fatalf("websocket turn wrong: %+v", t1)
	}
	if t1.Account != "user@example.com" || t1.AuthID != "claude-user.json" {
		t.Fatalf("account wrong: %s %s", t1.Account, t1.AuthID)
	}
}

// The image tool publishes a side-model usage record after the primary one, with
// the same trace and alias. Arriving after the next turn started, it must not take
// that turn's place.
func TestCollectorSideModelUsageStaysOffLaterTurn(t *testing.T) {
	h := newHarness(t)
	ws := http.Header{"Upgrade": {"websocket"}, "Authorization": {"Bearer " + testKey}}
	h.request("turn1", "conn", "/v1/responses", ws, `{"input":"one"}`)
	turn1Start := h.clock
	primary := usageRecord("conn", "claude-real", false)
	primary.RequestedAt = turn1Start
	primary.Detail.OutputTokens = 111
	h.feed(pluginabi.MethodUsageHandle, primary)
	h.complete("turn1", pluginapi.RequestCompletionSucceeded, 200)

	h.advance(time.Second)
	h.request("turn2", "conn", "/v1/responses", ws, `{"input":"two"}`)
	side := usageRecord("conn", "image-side-model", false)
	side.Alias = "claude-alias"
	side.RequestedAt = turn1Start
	side.Detail.OutputTokens = 7
	h.feed(pluginabi.MethodUsageHandle, side)
	second := usageRecord("conn", "claude-real", false)
	second.RequestedAt = h.clock
	second.Detail.OutputTokens = 999
	h.feed(pluginabi.MethodUsageHandle, second)
	h.complete("turn2", pluginapi.RequestCompletionSucceeded, 200)

	if len(h.rows) != 2 {
		t.Fatalf("rows = %d", len(h.rows))
	}
	got := map[string]row{}
	for i := range h.rows {
		r := h.row(i)
		got[r.RequestID] = r
	}
	if got["turn1"].OutputTokens != 111 || got["turn2"].OutputTokens != 999 || got["turn2"].ServedModel != "claude-real" {
		t.Fatalf("side-model usage misattributed: turn1 %d, turn2 %d %s", got["turn1"].OutputTokens, got["turn2"].OutputTokens, got["turn2"].ServedModel)
	}
}

// A turn whose usage never arrives must not take the next turn's record, and a
// side-model record for the next turn must not fall back onto it either.
func TestCollectorUsageGoesToNewestTurnStartedBeforeIt(t *testing.T) {
	h := newHarness(t)
	ws := http.Header{"Upgrade": {"websocket"}, "Authorization": {"Bearer " + testKey}}
	h.request("turn1", "conn", "/v1/responses", ws, `{"input":"one"}`)
	h.complete("turn1", pluginapi.RequestCompletionSucceeded, 200)

	h.advance(time.Second)
	h.request("turn2", "conn", "/v1/responses", ws, `{"input":"two"}`)
	turn2Start := h.clock
	primary := usageRecord("conn", "claude-real", false)
	primary.RequestedAt = turn2Start
	primary.Detail.OutputTokens = 222
	h.feed(pluginabi.MethodUsageHandle, primary)
	side := usageRecord("conn", "image-side-model", false)
	side.Alias = "claude-alias"
	side.RequestedAt = turn2Start
	side.Detail.OutputTokens = 7
	h.feed(pluginabi.MethodUsageHandle, side)
	h.complete("turn2", pluginapi.RequestCompletionSucceeded, 200)
	h.advance(h.cfg.UsageGrace)
	h.c.sweep(false)

	got := map[string]row{}
	for i := range h.rows {
		r := h.row(i)
		got[r.RequestID] = r
	}
	if len(got) != 2 || got["turn1"].OutputTokens != 0 || got["turn2"].OutputTokens != 222 {
		t.Fatalf("usage misattributed: turn1 %d, turn2 %d", got["turn1"].OutputTokens, got["turn2"].OutputTokens)
	}
}

// Without an account email the host reports the raw client key as the usage
// source; it must not reach the row.
func TestCollectorNeverStoresKeyFallbackAsAccount(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"client key", testKey},
		{"client key shaped like an email", "client@key"},
		{"credential key", "sk-upstream-credential"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.request("r1", "t1", "/v1/messages", http.Header{"X-Api-Key": {testKey}}, `{}`)
			rec := usageRecord("t1", "claude-real", false)
			rec.Source = tc.source
			if tc.source == "client@key" {
				rec.APIKey = tc.source
			}
			h.feed(pluginabi.MethodUsageHandle, rec)
			h.complete("r1", pluginapi.RequestCompletionSucceeded, 200)
			if r := h.row(0); r.Account != "" {
				t.Fatalf("account = %q", r.Account)
			}
			if bytes.Contains(h.rows[0], []byte(tc.source)) {
				t.Fatalf("key fallback stored in row: %s", h.rows[0])
			}
		})
	}
}

// A panic while a sweep finalizes a row drops that row and is counted; it must not
// escape the collector goroutine.
func TestCollectorSweepContainsFinalizePanic(t *testing.T) {
	h := newHarness(t)
	h.c.emit = func([]byte) { panic("emit failed") }
	h.request("r1", "t1", "/v1/messages", http.Header{}, `{}`)
	h.complete("r1", pluginapi.RequestCompletionSucceeded, 200)
	h.request("r2", "t2", "/v1/messages", http.Header{}, `{}`)
	panicsBefore, droppedBefore := stats.panics.Load(), stats.rowsDropped.Load()
	h.c.sweep(true)
	if stats.panics.Load() != panicsBefore+2 || stats.rowsDropped.Load() != droppedBefore+2 || len(h.c.entries) != 0 {
		t.Fatalf("panics +%d dropped +%d entries %d", stats.panics.Load()-panicsBefore, stats.rowsDropped.Load()-droppedBefore, len(h.c.entries))
	}
}

func TestCollectorWebSocketLabelWhenNoUsage(t *testing.T) {
	h := newHarness(t)
	h.request("turn1", "conn", "/v1/responses", http.Header{"Upgrade": {"websocket"}}, `{}`)
	h.feed(pluginabi.MethodWebSocketResponseEvent, pluginapi.WebSocketResponseEvent{RequestID: "turn1", Provider: "codex", AuthID: "codex.json", AuthLabel: "work"})
	h.complete("turn1", pluginapi.RequestCompletionFailed, 502)
	h.advance(h.cfg.UsageGrace)
	h.c.sweep(false)
	r := h.row(0)
	if r.Provider != "codex" || r.AuthID != "codex.json" || r.Account != "work" || r.Outcome != "failed" || r.ResponseKind != kindNone {
		t.Fatalf("row = %+v", r)
	}
}

func TestCollectorSkipsCountTokensAndUnknownIDs(t *testing.T) {
	h := newHarness(t)
	h.request("count", "t1", "/v1/messages/count_tokens", http.Header{}, `{}`)
	h.feed(pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{RequestID: "count", Body: []byte(`{"input_tokens":3}`)})
	h.feed(pluginabi.MethodUsageHandle, usageRecord("t1", "claude-real", false))
	h.complete("count", pluginapi.RequestCompletionSucceeded, 200)
	// A model-list response has no request interception before it.
	h.feed(pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{RequestID: "models", Body: []byte(`{"data":[]}`)})
	h.feed(pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: "models", Outcome: pluginapi.RequestCompletionSucceeded})
	h.advance(time.Hour)
	h.c.sweep(false)
	if len(h.rows) != 0 || len(h.c.entries) != 0 || len(h.c.byTrace) != 0 {
		t.Fatalf("rows = %d entries = %d", len(h.rows), len(h.c.entries))
	}
}

func TestCollectorAbandonsIdleEntries(t *testing.T) {
	h := newHarness(t)
	before := stats.abandoned.Load()
	h.request("r1", "t1", "/v1/messages", http.Header{}, `{}`)
	h.feed(pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{RequestID: "r1", Body: []byte("event: ping\n")})
	h.advance(h.cfg.InflightTTL - time.Second)
	h.c.sweep(false)
	if len(h.rows) != 0 {
		t.Fatalf("abandoned too early")
	}
	h.advance(time.Second)
	h.c.sweep(false)
	r := h.row(0)
	if r.Outcome != outcomeAbandoned || stats.abandoned.Load() != before+1 {
		t.Fatalf("outcome = %s", r.Outcome)
	}
	if r.ResponseKind != kindSSERaw {
		t.Fatalf("kind = %s", r.ResponseKind)
	}
}

func TestCollectorTruncatesBodies(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxBodyBytes = 16
	h.request("r1", "t1", "/v1/messages", http.Header{}, strings.Repeat("a", 100))
	for i := 0; i < 5; i++ {
		h.feed(pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{RequestID: "r1", ChunkIndex: i, Body: []byte("data: 0123456789")})
	}
	h.complete("r1", pluginapi.RequestCompletionFailed, 500)
	h.advance(h.cfg.UsageGrace)
	h.c.sweep(false)
	r := h.row(0)
	if !r.Truncated || len(r.ClientRequest) != 16 || len(r.UpstreamRequest) != 16 || len(r.Response) != 16 {
		t.Fatalf("truncation wrong: %v %d %d %d", r.Truncated, len(r.ClientRequest), len(r.UpstreamRequest), len(r.Response))
	}
}

func TestCollectorKeepsSuccessfulUsageOverRetryFailure(t *testing.T) {
	h := newHarness(t)
	h.request("r1", "t1", "/v1/messages", http.Header{}, `{}`)
	failed := usageRecord("t1", "claude-real", true)
	failed.Detail = pluginapi.UsageDetail{}
	h.feed(pluginabi.MethodUsageHandle, failed)
	h.complete("r1", pluginapi.RequestCompletionSucceeded, 200)
	if len(h.rows) != 0 {
		t.Fatalf("a failed attempt's usage must not settle a succeeded request")
	}
	h.feed(pluginabi.MethodUsageHandle, usageRecord("t1", "claude-real", false))
	h.feed(pluginabi.MethodUsageHandle, failed)
	h.c.sweep(false)
	if r := h.row(0); r.OutputTokens != 20 {
		t.Fatalf("tokens = %d", r.OutputTokens)
	}
}

func TestCollectorInflightCap(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxInflight = 1
	before := stats.eventsDropped.Load()
	h.request("r1", "t1", "/v1/messages", http.Header{}, `{}`)
	h.request("r2", "t2", "/v1/messages", http.Header{}, `{}`)
	if len(h.c.entries) != 1 || stats.eventsDropped.Load() <= before {
		t.Fatalf("entries = %d", len(h.c.entries))
	}
}

func TestCollectorEnqueueDropsPastBudget(t *testing.T) {
	cfg := defaultConfig()
	cfg.QueueMaxBytes = 10
	c := newCollector(func() *pluginConfig { return cfg }, func([]byte) {}, time.Now)
	before := stats.eventsDropped.Load()
	c.enqueue(pluginabi.MethodRequestComplete, []byte("12345"))
	c.enqueue(pluginabi.MethodRequestComplete, []byte("123456"))
	if len(c.events) != 1 || stats.eventsDropped.Load() != before+1 || c.queuedBytes.Load() != 5 {
		t.Fatalf("queued = %d dropped = %d bytes = %d", len(c.events), stats.eventsDropped.Load()-before, c.queuedBytes.Load())
	}
}

func TestCollectorStopFlushesEverything(t *testing.T) {
	cfg := defaultConfig()
	var rows [][]byte
	c := newCollector(func() *pluginConfig { return cfg }, func(raw []byte) { rows = append(rows, raw) }, time.Now)
	c.start()
	raw, _ := json.Marshal(pluginapi.RequestInterceptRequest{RequestID: "r1", Metadata: map[string]any{"request_path": "/v1/messages"}})
	c.enqueue(pluginabi.MethodRequestInterceptBefore, raw)
	c.stop()
	if len(rows) != 1 || !bytes.Contains(rows[0], []byte(`"outcome":"abandoned"`)) {
		t.Fatalf("rows = %s", rows)
	}
}

func TestCollectorSurvivesPanicsAndGarbage(t *testing.T) {
	cfg := defaultConfig()
	c := newCollector(func() *pluginConfig { return cfg }, func([]byte) { panic("emit failed") }, time.Now)
	before := stats.panics.Load()
	c.process(event{method: pluginabi.MethodRequestInterceptBefore, raw: []byte("not json")})
	raw, _ := json.Marshal(pluginapi.RequestInterceptRequest{RequestID: "r1", TraceID: "t1"})
	c.process(event{method: pluginabi.MethodRequestInterceptBefore, raw: raw})
	usage, _ := json.Marshal(pluginapi.UsageRecord{TraceID: "t1", Failed: true})
	c.process(event{method: pluginabi.MethodUsageHandle, raw: usage})
	done, _ := json.Marshal(pluginapi.RequestCompletion{RequestID: "r1", Outcome: pluginapi.RequestCompletionFailed})
	c.process(event{method: pluginabi.MethodRequestComplete, raw: done})
	if stats.panics.Load() != before+1 {
		t.Fatalf("panic not contained")
	}
}
