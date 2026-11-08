package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, errRead := os.ReadFile("testdata/" + name)
	if errRead != nil {
		t.Fatal(errRead)
	}
	return raw
}

type anthropicMessage struct {
	ID         string `json:"id"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type      string         `json:"type"`
		Text      string         `json:"text"`
		Thinking  string         `json:"thinking"`
		Signature string         `json:"signature"`
		Name      string         `json:"name"`
		Input     map[string]any `json:"input"`
	} `json:"content"`
	Usage map[string]float64 `json:"usage"`
}

func TestReassembleAnthropicStream(t *testing.T) {
	var errs captureErrs
	body, kind := reassembleStream(readFixture(t, "anthropic_stream.sse"), &errs)
	if kind != kindJSON {
		t.Fatalf("kind = %q, errors %v", kind, errs.list)
	}
	if len(errs.list) != 0 {
		t.Fatalf("unexpected capture errors: %v", errs.list)
	}
	var msg anthropicMessage
	if errDecode := json.Unmarshal(body, &msg); errDecode != nil {
		t.Fatalf("not JSON: %v", errDecode)
	}
	if msg.ID != "msg_01XFDUDYJgAACzvnptvVoYEL" || msg.Model != "claude-test-model" || msg.StopReason != "tool_use" {
		t.Fatalf("envelope wrong: %+v", msg)
	}
	if len(msg.Content) != 3 {
		t.Fatalf("content blocks = %d", len(msg.Content))
	}
	if msg.Content[0].Thinking != "The user wants the weather. I should call get_weather." || msg.Content[0].Signature == "" {
		t.Fatalf("thinking block wrong: %+v", msg.Content[0])
	}
	if msg.Content[1].Text != "Let me check <the> weather for you & San Francisco." {
		t.Fatalf("text block wrong: %q", msg.Content[1].Text)
	}
	if msg.Content[2].Name != "get_weather" || msg.Content[2].Input["location"] != "San Francisco, CA" || msg.Content[2].Input["unit"] != "fahrenheit" {
		t.Fatalf("tool_use block wrong: %+v", msg.Content[2])
	}
	if msg.Usage["output_tokens"] != 89 || msg.Usage["input_tokens"] != 472 || msg.Usage["cache_read_input_tokens"] != 2048 {
		t.Fatalf("usage not merged: %v", msg.Usage)
	}
	if bytes.Contains(body, []byte("\\"+"u003c")) {
		t.Fatalf("reassembled text is HTML-escaped: %s", body)
	}
}

func TestReassembleAnthropicKeepsStartTextBeforeDeltas(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m","content":[]}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Hel"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo, "}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`,
		`data: {"type":"content_block_stop","index":0}`,
	}, "\n\n")
	var errs captureErrs
	body, kind := reassembleStream([]byte(stream), &errs)
	var msg anthropicMessage
	if kind != kindJSON || json.Unmarshal(body, &msg) != nil || len(msg.Content) != 1 {
		t.Fatalf("kind %q body %s errors %v", kind, body, errs.list)
	}
	if msg.Content[0].Text != "Hello, world" {
		t.Fatalf("text = %q", msg.Content[0].Text)
	}
}

func TestReassembleAnthropicTruncatedStream(t *testing.T) {
	raw := readFixture(t, "anthropic_stream.sse")
	cut := bytes.Index(raw, []byte("ncisco, CA"))
	var errs captureErrs
	body, kind := reassembleStream(raw[:cut], &errs)
	if kind != kindJSON {
		t.Fatalf("kind = %q", kind)
	}
	if len(errs.list) == 0 {
		t.Fatalf("expected capture errors for a cut stream")
	}
	var msg struct {
		Content []map[string]any `json:"content"`
	}
	if errDecode := json.Unmarshal(body, &msg); errDecode != nil {
		t.Fatalf("not JSON: %v", errDecode)
	}
	if len(msg.Content) != 3 || msg.Content[1]["text"] == "" {
		t.Fatalf("partial content lost: %s", body)
	}
	// The cut tool input cannot parse, so it is kept as the partial text.
	if msg.Content[2]["input"] != `{"location": "San Fra` {
		t.Fatalf("partial tool input = %#v", msg.Content[2]["input"])
	}
}

func TestReassembleAnthropicErrorOnlyStream(t *testing.T) {
	raw := []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	var errs captureErrs
	body, kind := reassembleStream(raw, &errs)
	if kind != kindSSERaw || !bytes.Equal(body, raw) {
		// An error-only stream has no message_start, so the format is not detected.
		t.Fatalf("kind = %q body = %s", kind, body)
	}
}

type responsesObject struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Model  string `json:"model"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
}

func TestReassembleResponsesStream(t *testing.T) {
	var errs captureErrs
	body, kind := reassembleStream(readFixture(t, "responses_stream.sse"), &errs)
	if kind != kindJSON || len(errs.list) != 0 {
		t.Fatalf("kind = %q errors %v", kind, errs.list)
	}
	var obj responsesObject
	if errDecode := json.Unmarshal(body, &obj); errDecode != nil {
		t.Fatalf("not JSON: %v", errDecode)
	}
	if obj.ID != "resp_68b1c0ffee" || obj.Status != "completed" || len(obj.Output) != 2 || obj.Output[1].Content[0].Text != "Hello from Codex." {
		t.Fatalf("wrong final object: %s", body)
	}
	facts := extractResponseFacts(body)
	if facts.model != "gpt-test-model" || facts.tokens.input != 1200 || facts.tokens.output != 40 || facts.tokens.cacheRead != 1024 || facts.tokens.reasoning != 16 {
		t.Fatalf("facts = %+v", facts)
	}
}

// The host delivers OpenAI Responses chunks as single SSE lines with no blank
// separators; the collector joins them with newlines only.
func TestReassembleResponsesLineChunks(t *testing.T) {
	var joined []byte
	for _, line := range strings.Split(string(readFixture(t, "responses_stream.sse")), "\n") {
		if line == "" {
			continue
		}
		joined = append(joined, line...)
		joined = append(joined, '\n')
	}
	var errs captureErrs
	body, kind := reassembleStream(joined, &errs)
	if kind != kindJSON || !bytes.Contains(body, []byte("Hello from Codex.")) {
		t.Fatalf("kind = %q body = %s errs %v", kind, body, errs.list)
	}
}

func TestReassembleResponsesWithoutTerminalEvent(t *testing.T) {
	raw := readFixture(t, "responses_stream.sse")
	raw = raw[:bytes.Index(raw, []byte("event: response.completed"))]
	var errs captureErrs
	body, kind := reassembleStream(raw, &errs)
	if kind != kindSSERaw || !bytes.Equal(body, raw) || len(errs.list) == 0 {
		t.Fatalf("kind = %q errs %v", kind, errs.list)
	}
}

func TestReassembleUnknownFormatKeepsRawText(t *testing.T) {
	raw := []byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	var errs captureErrs
	body, kind := reassembleStream(raw, &errs)
	if kind != kindSSERaw || !bytes.Equal(body, raw) {
		t.Fatalf("kind = %q", kind)
	}
	if len(errs.list) != 1 {
		t.Fatalf("errs = %v", errs.list)
	}
}

func TestReassembleEmptyStream(t *testing.T) {
	var errs captureErrs
	if body, kind := reassembleStream(nil, &errs); body != nil || kind != kindNone {
		t.Fatalf("kind = %q", kind)
	}
}

func TestCaptureErrsDedupeAndCap(t *testing.T) {
	var errs captureErrs
	for i := 0; i < 100; i++ {
		errs.add("same")
		errs.add("n%d", i)
	}
	if len(errs.list) != maxCaptureErrors || errs.list[0] != "same" {
		t.Fatalf("errs = %v", errs.list)
	}
}
