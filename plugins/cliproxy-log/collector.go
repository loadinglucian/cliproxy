package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	outcomeAbandoned = "abandoned"
	sweepInterval    = time.Second
	eventQueueLength = 65536
	maxErrorBytes    = 1 << 20
)

// event is one copied RPC payload waiting for the collector goroutine.
type event struct {
	method string
	raw    []byte
	at     time.Time
}

// entry accumulates everything observed for one RequestID until it is written.
type entry struct {
	id, traceID string
	firstSeen   time.Time
	lastSeen    time.Time
	skip        bool

	endpoint, transport, sourceFormat, targetFormat string
	requestedModel, servedModel                     string
	stream                                          bool
	keyHash                                         string
	reasoningEffort, serviceTier                    string

	reqHeaders, respHeaders map[string]string
	clientRequest           []byte
	upstreamRequest         []byte
	response                []byte
	streamText              []byte
	wsText                  []byte
	responseStatus          int
	truncated               bool

	wsProvider, wsAuthID, wsLabel string

	usage       *pluginapi.UsageRecord
	completion  *pluginapi.RequestCompletion
	completedAt time.Time
}

// collector correlates host callbacks by RequestID on a single goroutine, so entry
// state needs no locks. Interceptors only enqueue; all decoding happens here.
type collector struct {
	cfg  func() *pluginConfig
	emit func([]byte)
	now  func() time.Time

	events      chan event
	queuedBytes atomic.Int64
	stopCh      chan struct{}
	done        chan struct{}

	entries map[string]*entry
	byTrace map[string][]*entry
}

func newCollector(cfg func() *pluginConfig, emit func([]byte), now func() time.Time) *collector {
	return &collector{
		cfg:     cfg,
		emit:    emit,
		now:     now,
		events:  make(chan event, eventQueueLength),
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
		entries: make(map[string]*entry),
		byTrace: make(map[string][]*entry),
	}
}

// enqueue hands a payload to the collector without ever blocking. Payloads past the
// queue byte budget or channel capacity are dropped and counted.
func (c *collector) enqueue(method string, raw []byte) {
	size := int64(len(raw))
	if c.queuedBytes.Add(size) > c.cfg().QueueMaxBytes {
		c.queuedBytes.Add(-size)
		stats.eventsDropped.Add(1)
		return
	}
	select {
	case c.events <- event{method: method, raw: raw, at: c.now()}:
	default:
		c.queuedBytes.Add(-size)
		stats.eventsDropped.Add(1)
	}
}

func (c *collector) start() { go c.run() }

// stop processes queued events, writes every remaining entry (incomplete ones as
// abandoned), and returns once the goroutine has exited.
func (c *collector) stop() {
	close(c.stopCh)
	<-c.done
}

func (c *collector) run() {
	defer close(c.done)
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case ev := <-c.events:
			c.queuedBytes.Add(-int64(len(ev.raw)))
			c.process(ev)
		case <-ticker.C:
			c.sweep(false)
		case <-c.stopCh:
			for {
				select {
				case ev := <-c.events:
					c.queuedBytes.Add(-int64(len(ev.raw)))
					c.process(ev)
					continue
				default:
				}
				break
			}
			c.sweep(true)
			return
		}
	}
}

// process applies one event. A panic is contained to that event.
func (c *collector) process(ev event) {
	defer func() {
		if recovered := recover(); recovered != nil {
			stats.panics.Add(1)
		}
	}()
	switch ev.method {
	case pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter:
		var req pluginapi.RequestInterceptRequest
		if json.Unmarshal(ev.raw, &req) != nil {
			return
		}
		c.onRequest(req, ev.method == pluginabi.MethodRequestInterceptBefore, ev.at)
	case pluginabi.MethodResponseInterceptAfter:
		var req pluginapi.ResponseInterceptRequest
		if json.Unmarshal(ev.raw, &req) != nil {
			return
		}
		c.onResponse(req, ev.at)
	case pluginabi.MethodResponseInterceptStreamChunk:
		var req pluginapi.StreamChunkInterceptRequest
		if json.Unmarshal(ev.raw, &req) != nil {
			return
		}
		c.onStreamChunk(req, ev.at)
	case pluginabi.MethodWebSocketResponseEvent:
		var req pluginapi.WebSocketResponseEvent
		if json.Unmarshal(ev.raw, &req) != nil {
			return
		}
		c.onWebSocketEvent(req, ev.at)
	case pluginabi.MethodUsageHandle:
		var rec pluginapi.UsageRecord
		if json.Unmarshal(ev.raw, &rec) != nil {
			return
		}
		c.onUsage(rec)
	case pluginabi.MethodRequestComplete:
		var completion pluginapi.RequestCompletion
		if json.Unmarshal(ev.raw, &completion) != nil {
			return
		}
		c.onComplete(completion, ev.at)
	}
}

func (c *collector) onRequest(req pluginapi.RequestInterceptRequest, beforeAuth bool, at time.Time) {
	if req.RequestID == "" {
		return
	}
	e := c.entries[req.RequestID]
	if e == nil {
		if len(c.entries) >= c.cfg().MaxInflight {
			stats.eventsDropped.Add(1)
			return
		}
		e = &entry{id: req.RequestID, traceID: req.TraceID, firstSeen: at, transport: "http"}
		e.endpoint = metadataString(req.Metadata, "request_path")
		// Token counting goes through the same execution path but is not a model call.
		e.skip = strings.Contains(e.endpoint, "count_tokens")
		c.entries[e.id] = e
		if e.traceID != "" {
			c.byTrace[e.traceID] = append(c.byTrace[e.traceID], e)
		}
	}
	e.lastSeen = at
	if e.skip {
		return
	}
	maxBody := c.cfg().MaxBodyBytes
	if e.sourceFormat == "" {
		e.sourceFormat = req.SourceFormat
	}
	if e.requestedModel == "" {
		e.requestedModel = req.RequestedModel
	}
	e.stream = e.stream || req.Stream
	if v := metadataString(req.Metadata, "reasoning_effort"); v != "" {
		e.reasoningEffort = v
	}
	if v := metadataString(req.Metadata, "service_tier"); v != "" {
		e.serviceTier = v
	}
	if e.reqHeaders == nil && req.Headers != nil {
		e.reqHeaders = redactHeaders(req.Headers)
		e.keyHash = hashKey(clientKeyFromHeaders(req.Headers))
		if strings.EqualFold(strings.TrimSpace(req.Headers.Get("Upgrade")), "websocket") {
			e.transport = "websocket"
		}
	}
	if beforeAuth {
		if e.clientRequest == nil && len(req.Body) > 0 {
			e.clientRequest = e.capped(req.Body, maxBody)
		}
		return
	}
	// After auth runs once per upstream attempt; the last attempt is the one sent.
	e.targetFormat = req.ToFormat
	e.servedModel = req.Model
	if len(req.Body) > 0 {
		e.upstreamRequest = e.capped(req.Body, maxBody)
	}
}

func (c *collector) onResponse(req pluginapi.ResponseInterceptRequest, at time.Time) {
	e := c.live(req.RequestID, at)
	if e == nil {
		return
	}
	e.response = e.capped(req.Body, c.cfg().MaxBodyBytes)
	e.respHeaders = redactHeaders(req.ResponseHeaders)
	e.responseStatus = req.StatusCode
}

func (c *collector) onStreamChunk(req pluginapi.StreamChunkInterceptRequest, at time.Time) {
	e := c.live(req.RequestID, at)
	if e == nil {
		return
	}
	if req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex {
		e.respHeaders = redactHeaders(req.ResponseHeaders)
		return
	}
	chunk := req.Body
	if len(chunk) > 0 && chunk[len(chunk)-1] != '\n' {
		// Chunks are whole lines or whole events; keep their boundary visible.
		chunk = append(chunk, '\n')
	}
	e.streamText = e.appendCapped(e.streamText, chunk, c.cfg().MaxBodyBytes)
}

func (c *collector) onWebSocketEvent(ev pluginapi.WebSocketResponseEvent, at time.Time) {
	e := c.live(ev.RequestID, at)
	if e == nil {
		return
	}
	if ev.Provider != "" {
		e.wsProvider = ev.Provider
	}
	if ev.AuthID != "" {
		e.wsAuthID = ev.AuthID
	}
	if ev.AuthLabel != "" {
		e.wsLabel = ev.AuthLabel
	}
	if len(ev.Payload) == 0 {
		return
	}
	maxBody := c.cfg().MaxBodyBytes
	// Frame each upstream event as SSE so the stream reassembler handles both paths.
	framed := make([]byte, 0, len(ev.Payload)+8)
	framed = append(framed, "data: "...)
	framed = append(framed, ev.Payload...)
	framed = append(framed, "\n\n"...)
	e.wsText = e.appendCapped(e.wsText, framed, maxBody)
}

// onUsage attaches a usage record. The host gives usage records their own per-attempt
// ID, so they correlate by TraceID (the inbound HTTP request) instead. On a WebSocket
// connection every turn shares one TraceID; turns run one at a time, so the oldest
// turn still waiting for usage is the match.
func (c *collector) onUsage(rec pluginapi.UsageRecord) {
	if rec.TraceID == "" {
		return
	}
	candidates := c.byTrace[rec.TraceID]
	if len(candidates) == 0 {
		return
	}
	var target *entry
	for _, e := range candidates {
		if !hasGoodUsage(e) && usageModelMatches(e, rec) {
			target = e
			break
		}
	}
	if target == nil {
		for _, e := range candidates {
			if !hasGoodUsage(e) {
				target = e
				break
			}
		}
	}
	if target == nil || target.skip {
		return
	}
	if target.usage == nil || target.usage.Failed || !rec.Failed {
		copied := rec
		copied.APIKey = ""
		target.usage = &copied
	}
	if rec.APIKey != "" {
		// The authenticated principal is authoritative over the header guess.
		target.keyHash = hashKey(rec.APIKey)
	}
}

func hasGoodUsage(e *entry) bool { return e.usage != nil && !e.usage.Failed }

func usageModelMatches(e *entry, rec pluginapi.UsageRecord) bool {
	return rec.Model != "" && (rec.Model == e.servedModel || rec.Model == e.requestedModel || rec.Alias == e.requestedModel)
}

func (c *collector) onComplete(completion pluginapi.RequestCompletion, at time.Time) {
	e := c.entries[completion.RequestID]
	if e == nil {
		return
	}
	if e.skip {
		c.remove(e)
		return
	}
	completion.Metadata = nil
	e.completion = &completion
	e.completedAt = at
	e.lastSeen = at
	if usageSettled(e) {
		c.finalize(e, c.now())
	}
}

// live returns the entry for a request that is being logged, refreshing its TTL.
// Events for unknown IDs (for example model-list responses) are ignored.
func (c *collector) live(requestID string, at time.Time) *entry {
	e := c.entries[requestID]
	if e == nil || e.skip {
		return nil
	}
	e.lastSeen = at
	return e
}

// sweep writes completed entries whose usage arrived or whose usage grace expired,
// and abandons entries idle past the TTL. With all set, it writes everything now.
func (c *collector) sweep(all bool) {
	cfg := c.cfg()
	now := c.now()
	for _, e := range c.entries {
		switch {
		case e.skip:
			if all || now.Sub(e.lastSeen) >= cfg.InflightTTL {
				c.remove(e)
			}
		case e.completion != nil:
			if all || usageSettled(e) || now.Sub(e.completedAt) >= cfg.UsageGrace {
				c.finalize(e, now)
			}
		case all || now.Sub(e.lastSeen) >= cfg.InflightTTL:
			stats.abandoned.Add(1)
			c.finalize(e, now)
		}
	}
}

// usageSettled reports whether waiting longer cannot improve the usage data: a
// succeeded request has a successful record, or a failed one has any record.
func usageSettled(e *entry) bool {
	if e.usage == nil {
		return false
	}
	succeeded := e.completion.Outcome == pluginapi.RequestCompletionSucceeded
	return !succeeded || !e.usage.Failed
}

func (c *collector) remove(e *entry) {
	delete(c.entries, e.id)
	if e.traceID == "" {
		return
	}
	list := c.byTrace[e.traceID]
	for i, candidate := range list {
		if candidate == e {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(c.byTrace, e.traceID)
	} else {
		c.byTrace[e.traceID] = list
	}
}

func (c *collector) finalize(e *entry, now time.Time) {
	c.remove(e)
	r := buildRow(e, c.cfg(), now)
	raw, errMarshal := marshalNoEscape(r)
	if errMarshal != nil {
		stats.rowsDropped.Add(1)
		return
	}
	c.emit(raw)
}

// capped copies src, cutting it at max bytes and flagging the entry as truncated.
func (e *entry) capped(src []byte, max int) []byte {
	if len(src) > max {
		e.truncated = true
		src = src[:max]
	}
	return append([]byte(nil), src...)
}

// appendCapped appends src to dst without letting dst exceed max bytes.
func (e *entry) appendCapped(dst, src []byte, max int) []byte {
	room := max - len(dst)
	if room <= 0 {
		if len(src) > 0 {
			e.truncated = true
		}
		return dst
	}
	if len(src) > room {
		e.truncated = true
		src = src[:room]
	}
	return append(dst, src...)
}

func metadataString(meta map[string]any, key string) string {
	v, _ := meta[key].(string)
	return strings.TrimSpace(v)
}
