package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	kindJSON   = "json"
	kindSSERaw = "sse_raw"
	kindNone   = "none"
)

// captureErrs collects parse problems for the capture_errors column. Messages are
// fixed descriptions, never payload content, and repeated messages are kept once.
type captureErrs struct {
	list []string
	seen map[string]struct{}
}

const maxCaptureErrors = 20

func (c *captureErrs) add(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if c.seen == nil {
		c.seen = make(map[string]struct{})
	}
	if _, dup := c.seen[msg]; dup || len(c.list) >= maxCaptureErrors {
		return
	}
	c.seen[msg] = struct{}{}
	c.list = append(c.list, msg)
}

// sseEvents extracts event payloads line by line: every "data:" line and every bare
// JSON line is one payload. The host delivers some streams as whole SSE blocks and
// others (OpenAI Responses) as single lines without blank-line separators, so block
// boundaries cannot be relied on; JSON event payloads never span lines in practice.
// "[DONE]" sentinels are skipped.
func sseEvents(raw []byte) [][]byte {
	if len(raw) == 0 {
		return nil
	}
	var events [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		switch {
		case bytes.HasPrefix(line, []byte("data:")):
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
				events = append(events, payload)
			}
		case bytes.HasPrefix(line, []byte("{")):
			events = append(events, line)
		}
	}
	return events
}

type eventType struct {
	Type string `json:"type"`
}

// reassembleStream turns captured stream text into the final response object when
// the format is known (Anthropic Messages or OpenAI Responses) and otherwise returns
// the raw text with kind sse_raw.
func reassembleStream(raw []byte, errs *captureErrs) ([]byte, string) {
	if len(raw) == 0 {
		return nil, kindNone
	}
	events := sseEvents(raw)
	format := ""
	for _, ev := range events {
		var t eventType
		if json.Unmarshal(ev, &t) != nil {
			continue
		}
		if t.Type == "message_start" {
			format = "anthropic"
			break
		}
		if strings.HasPrefix(t.Type, "response.") {
			format = "responses"
			break
		}
	}
	switch format {
	case "anthropic":
		if body, ok := reassembleAnthropic(events, errs); ok {
			return body, kindJSON
		}
	case "responses":
		if body, ok := reassembleResponses(events, errs); ok {
			return body, kindJSON
		}
	default:
		errs.add("unrecognized stream format; stored raw stream text")
	}
	return raw, kindSSERaw
}

type anthropicEvent struct {
	Type         string         `json:"type"`
	Index        *int           `json:"index"`
	Message      map[string]any `json:"message"`
	ContentBlock map[string]any `json:"content_block"`
	Delta        map[string]any `json:"delta"`
	Usage        map[string]any `json:"usage"`
}

// reassembleAnthropic rebuilds the final Messages API object from an SSE stream:
// message_start provides the envelope, content blocks accumulate text, thinking,
// signature, citation, and tool_use input JSON deltas, and message_delta merges the
// stop reason and final usage. A stream holding only an error event yields that event.
func reassembleAnthropic(events [][]byte, errs *captureErrs) ([]byte, bool) {
	var msg map[string]any
	var errorEvent []byte
	blocks := make(map[int]map[string]any)
	partial := make(map[int]*strings.Builder)
	for _, raw := range events {
		var ev anthropicEvent
		if errDecode := decodeUseNumber(raw, &ev); errDecode != nil {
			errs.add("undecodable stream event")
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message == nil {
				errs.add("message_start without message")
				continue
			}
			msg = ev.Message
		case "content_block_start":
			if ev.Index == nil || ev.ContentBlock == nil {
				errs.add("content_block_start without index or block")
				continue
			}
			blocks[*ev.Index] = ev.ContentBlock
		case "content_block_delta":
			if ev.Index == nil || ev.Delta == nil {
				errs.add("content_block_delta without index or delta")
				continue
			}
			block := blocks[*ev.Index]
			if block == nil {
				errs.add("content_block_delta for unknown block")
				continue
			}
			applyAnthropicDelta(block, *ev.Index, ev.Delta, partial, errs)
		case "content_block_stop":
			if ev.Index == nil {
				errs.add("content_block_stop without index")
				continue
			}
			finishToolInput(blocks[*ev.Index], *ev.Index, partial, errs)
		case "message_delta":
			if msg == nil {
				errs.add("message_delta before message_start")
				continue
			}
			for key, value := range ev.Delta {
				msg[key] = value
			}
			if len(ev.Usage) > 0 {
				usage, _ := msg["usage"].(map[string]any)
				if usage == nil {
					usage = make(map[string]any, len(ev.Usage))
				}
				for key, value := range ev.Usage {
					usage[key] = value
				}
				msg["usage"] = usage
			}
		case "message_stop", "ping":
		case "error":
			errorEvent = raw
			errs.add("stream contained an error event")
		default:
			errs.add("unknown stream event type %q", truncateLabel(ev.Type))
		}
	}
	if len(partial) > 0 {
		errs.add("content block not closed before stream end")
		for index := range partial {
			finishToolInput(blocks[index], index, partial, errs)
		}
	}
	if msg == nil {
		if errorEvent != nil {
			return errorEvent, true
		}
		errs.add("stream had no message_start event")
		return nil, false
	}
	indexes := make([]int, 0, len(blocks))
	for index := range blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	content := make([]any, 0, len(indexes))
	for _, index := range indexes {
		content = append(content, blocks[index])
	}
	msg["content"] = content
	body, errMarshal := marshalNoEscape(msg)
	if errMarshal != nil {
		errs.add("could not encode reassembled message")
		return nil, false
	}
	return body, true
}

func applyAnthropicDelta(block map[string]any, index int, delta map[string]any, partial map[int]*strings.Builder, errs *captureErrs) {
	deltaType, _ := delta["type"].(string)
	switch deltaType {
	case "text_delta":
		block["text"] = stringField(block, "text") + stringField(delta, "text")
	case "thinking_delta":
		block["thinking"] = stringField(block, "thinking") + stringField(delta, "thinking")
	case "signature_delta":
		block["signature"] = stringField(block, "signature") + stringField(delta, "signature")
	case "input_json_delta":
		b := partial[index]
		if b == nil {
			b = &strings.Builder{}
			partial[index] = b
		}
		b.WriteString(stringField(delta, "partial_json"))
	case "citations_delta":
		citations, _ := block["citations"].([]any)
		block["citations"] = append(citations, delta["citation"])
	default:
		errs.add("unknown content delta type %q", truncateLabel(deltaType))
	}
}

// finishToolInput replaces a tool_use block's placeholder input with the parsed
// accumulated partial JSON. Unparsable input is kept as a string.
func finishToolInput(block map[string]any, index int, partial map[int]*strings.Builder, errs *captureErrs) {
	b, ok := partial[index]
	if !ok {
		return
	}
	delete(partial, index)
	if block == nil {
		return
	}
	text := b.String()
	if strings.TrimSpace(text) == "" {
		block["input"] = map[string]any{}
		return
	}
	var input any
	if errDecode := decodeUseNumber([]byte(text), &input); errDecode != nil {
		errs.add("tool input JSON did not parse; kept as text")
		block["input"] = text
		return
	}
	block["input"] = input
}

type responsesEvent struct {
	Type     string          `json:"type"`
	Response json.RawMessage `json:"response"`
}

// reassembleResponses returns the response object of the last terminal event
// (response.completed, response.failed, or response.incomplete).
func reassembleResponses(events [][]byte, errs *captureErrs) ([]byte, bool) {
	var final []byte
	for _, raw := range events {
		var ev responsesEvent
		if json.Unmarshal(raw, &ev) != nil {
			errs.add("undecodable stream event")
			continue
		}
		switch ev.Type {
		case "response.completed", "response.failed", "response.incomplete":
			if len(ev.Response) > 0 && !bytes.Equal(ev.Response, []byte("null")) {
				final = ev.Response
			}
		case "error":
			errs.add("stream contained an error event")
		}
	}
	if final == nil {
		errs.add("stream had no terminal response event; stored raw stream text")
		return nil, false
	}
	return final, true
}

// responseFacts holds what the row needs from a final response body.
type responseFacts struct {
	model  string
	tokens tokenCounts
	found  bool
}

type tokenCounts struct {
	input, output, cacheRead, cacheWrite, reasoning uint64
}

type bodyUsage struct {
	Model string `json:"model"`
	Usage *struct {
		InputTokens              uint64 `json:"input_tokens"`
		OutputTokens             uint64 `json:"output_tokens"`
		CacheReadInputTokens     uint64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens uint64 `json:"cache_creation_input_tokens"`
		PromptTokens             uint64 `json:"prompt_tokens"`
		CompletionTokens         uint64 `json:"completion_tokens"`
		InputTokensDetails       *struct {
			CachedTokens uint64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputTokensDetails *struct {
			ReasoningTokens uint64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
		PromptTokensDetails *struct {
			CachedTokens uint64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens uint64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// extractResponseFacts reads the model and token usage from an Anthropic Messages,
// OpenAI Responses, or Chat Completions body. It is a fallback for rows without a
// usage record, so parse failures are silent.
func extractResponseFacts(body []byte) responseFacts {
	var parsed bodyUsage
	if len(body) == 0 || json.Unmarshal(body, &parsed) != nil {
		return responseFacts{}
	}
	facts := responseFacts{model: parsed.Model}
	if u := parsed.Usage; u != nil {
		facts.found = true
		facts.tokens.input = u.InputTokens + u.PromptTokens
		facts.tokens.output = u.OutputTokens + u.CompletionTokens
		facts.tokens.cacheRead = u.CacheReadInputTokens
		facts.tokens.cacheWrite = u.CacheCreationInputTokens
		if u.InputTokensDetails != nil {
			facts.tokens.cacheRead += u.InputTokensDetails.CachedTokens
		}
		if u.PromptTokensDetails != nil {
			facts.tokens.cacheRead += u.PromptTokensDetails.CachedTokens
		}
		if u.OutputTokensDetails != nil {
			facts.tokens.reasoning = u.OutputTokensDetails.ReasoningTokens
		}
		if u.CompletionTokensDetails != nil {
			facts.tokens.reasoning += u.CompletionTokensDetails.ReasoningTokens
		}
	}
	return facts
}

func decodeUseNumber(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v)
}

// marshalNoEscape encodes without HTML escaping so stored text keeps literal <, >, &
// and stays searchable.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if errEncode := enc.Encode(v); errEncode != nil {
		return nil, errEncode
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func truncateLabel(s string) string {
	if len(s) > 64 {
		return s[:64]
	}
	return s
}
