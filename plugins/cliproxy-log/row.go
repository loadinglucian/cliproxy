package main

import (
	"encoding/json"
	"math"
	"time"
)

const clickHouseTimeLayout = "2006-01-02 15:04:05.000"

// row mirrors the cliproxy_log.requests contract column for column; it is encoded
// as one JSONEachRow line.
type row struct {
	RequestID        string            `json:"request_id"`
	TraceID          string            `json:"trace_id"`
	StartedAt        string            `json:"started_at"`
	CompletedAt      string            `json:"completed_at"`
	DurationMS       uint32            `json:"duration_ms"`
	Machine          string            `json:"machine"`
	ClientKeyID      string            `json:"client_key_id"`
	Endpoint         string            `json:"endpoint"`
	Transport        string            `json:"transport"`
	SourceFormat     string            `json:"source_format"`
	TargetFormat     string            `json:"target_format"`
	Provider         string            `json:"provider"`
	Account          string            `json:"account"`
	AuthID           string            `json:"auth_id"`
	RequestedModel   string            `json:"requested_model"`
	ServedModel      string            `json:"served_model"`
	ResponseModel    string            `json:"response_model"`
	SessionID        string            `json:"session_id"`
	Stream           bool              `json:"stream"`
	StatusCode       uint16            `json:"status_code"`
	Outcome          string            `json:"outcome"`
	Error            string            `json:"error"`
	InputTokens      uint64            `json:"input_tokens"`
	OutputTokens     uint64            `json:"output_tokens"`
	CacheReadTokens  uint64            `json:"cache_read_tokens"`
	CacheWriteTokens uint64            `json:"cache_write_tokens"`
	ReasoningTokens  uint64            `json:"reasoning_tokens"`
	ReasoningEffort  string            `json:"reasoning_effort"`
	ServiceTier      string            `json:"service_tier"`
	RequestHeaders   map[string]string `json:"request_headers"`
	ResponseHeaders  map[string]string `json:"response_headers"`
	ClientRequest    string            `json:"client_request"`
	UpstreamRequest  string            `json:"upstream_request"`
	Response         string            `json:"response"`
	ResponseKind     string            `json:"response_kind"`
	Truncated        bool              `json:"truncated"`
	CaptureErrors    []string          `json:"capture_errors"`
	PluginVersion    string            `json:"plugin_version"`
	ProxyVersion     string            `json:"proxy_version"`
}

// buildRow turns a finished entry into a row. now is used as the completion time
// for abandoned entries that never received request.complete.
func buildRow(e *entry, cfg *pluginConfig, now time.Time) row {
	var errs captureErrs
	r := row{
		RequestID:       e.id,
		TraceID:         e.traceID,
		Endpoint:        e.endpoint,
		Transport:       e.transport,
		SourceFormat:    e.sourceFormat,
		TargetFormat:    e.targetFormat,
		RequestedModel:  e.requestedModel,
		ServedModel:     e.servedModel,
		Stream:          e.stream,
		ReasoningEffort: e.reasoningEffort,
		ServiceTier:     e.serviceTier,
		RequestHeaders:  nonNilMap(e.reqHeaders),
		ResponseHeaders: nonNilMap(e.respHeaders),
		ClientRequest:   string(e.clientRequest),
		UpstreamRequest: string(e.upstreamRequest),
		Provider:        e.wsProvider,
		AuthID:          e.wsAuthID,
		Account:         e.wsLabel,
		Machine:         "unknown",
		ClientKeyID:     clientKeyID(e.keyHash),
		PluginVersion:   pluginVersion,
		ProxyVersion:    resolvedProxyVersion(),
	}
	if name, ok := cfg.Machines[e.keyHash]; ok && e.keyHash != "" {
		r.Machine = name
	}

	started, completed := e.firstSeen, e.lastSeen
	if completed.IsZero() {
		completed = now
	}
	if c := e.completion; c != nil {
		r.Outcome = string(c.Outcome)
		r.StatusCode = clampUint16(c.StatusCode)
		r.Error = truncateString(c.Error, maxErrorBytes)
		r.Stream = r.Stream || c.Stream
		if !c.StartedAt.IsZero() {
			started = c.StartedAt
		}
		if !c.CompletedAt.IsZero() {
			completed = c.CompletedAt
		}
		if r.ServedModel == "" {
			r.ServedModel = c.Model
		}
		if r.RequestedModel == "" {
			r.RequestedModel = c.RequestedModel
		}
	} else {
		r.Outcome = outcomeAbandoned
		r.StatusCode = clampUint16(e.responseStatus)
	}
	if completed.Before(started) {
		completed = started
	}
	r.StartedAt = started.UTC().Format(clickHouseTimeLayout)
	r.CompletedAt = completed.UTC().Format(clickHouseTimeLayout)
	r.DurationMS = clampUint32(completed.Sub(started).Milliseconds())

	var body []byte
	switch {
	case e.response != nil:
		body, r.ResponseKind = e.response, kindJSON
		if !json.Valid(body) && !e.truncated {
			errs.add("non-stream response body is not JSON")
			r.ResponseKind = kindSSERaw
		}
	case len(e.streamText) > 0:
		body, r.ResponseKind = reassembleStream(e.streamText, &errs)
	case len(e.wsText) > 0:
		body, r.ResponseKind = reassembleStream(e.wsText, &errs)
	default:
		r.ResponseKind = kindNone
	}
	r.Response = string(body)
	facts := extractResponseFacts(body)
	r.ResponseModel = facts.model

	if u := e.usage; u != nil {
		if u.Provider != "" {
			r.Provider = u.Provider
		}
		if u.AuthID != "" {
			r.AuthID = u.AuthID
		}
		// Source is the account email for OAuth credentials; for API-key credentials
		// it can be the upstream key itself, so it is only used for OAuth.
		if u.AuthType == "oauth" && u.Source != "" {
			r.Account = u.Source
		}
		if u.Model != "" {
			r.ServedModel = u.Model
		}
		if r.ResponseModel == "" {
			r.ResponseModel = u.ResponseModel
		}
		r.SessionID = u.SessionID
		if u.ReasoningEffort != "" {
			r.ReasoningEffort = u.ReasoningEffort
		}
		if u.ServiceTier != "" {
			r.ServiceTier = u.ServiceTier
		}
		r.InputTokens = nonNegative(u.Detail.InputTokens)
		r.OutputTokens = nonNegative(u.Detail.OutputTokens)
		r.ReasoningTokens = nonNegative(u.Detail.ReasoningTokens)
		r.CacheWriteTokens = nonNegative(u.Detail.CacheCreationTokens)
		r.CacheReadTokens = nonNegative(u.Detail.CacheReadTokens)
		if r.CacheReadTokens == 0 {
			r.CacheReadTokens = nonNegative(u.Detail.CachedTokens)
		}
	} else if facts.found {
		r.InputTokens = facts.tokens.input
		r.OutputTokens = facts.tokens.output
		r.CacheReadTokens = facts.tokens.cacheRead
		r.CacheWriteTokens = facts.tokens.cacheWrite
		r.ReasoningTokens = facts.tokens.reasoning
	}

	r.Truncated = e.truncated
	r.CaptureErrors = errs.list
	if r.CaptureErrors == nil {
		r.CaptureErrors = []string{}
	} else {
		stats.captureErrors.Add(1)
	}
	return r
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func nonNegative(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

func clampUint16(v int) uint16 {
	if v < 0 {
		return 0
	}
	if v > math.MaxUint16 {
		return math.MaxUint16
	}
	return uint16(v)
}

func clampUint32(v int64) uint32 {
	if v < 0 {
		return 0
	}
	if v > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}

func truncateString(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}
