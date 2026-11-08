package main

import (
	"net/http"
	"testing"
)

func TestRedactHeaders(t *testing.T) {
	in := http.Header{
		"Authorization":                        {"Bearer secret"},
		"X-Api-Key":                            {"secret"},
		"Proxy-Authorization":                  {"Basic secret"},
		"Cookie":                               {"a=b"},
		"Set-Cookie":                           {"c=d"},
		"X-Goog-Api-Key":                       {"secret"},
		"X-Session-Token":                      {"secret"},
		"X-Client-Secret":                      {"secret"},
		"X-Amz-Credential":                     {"secret"},
		"X-Refresh-Token":                      {"secret"},
		"Anthropic-Version":                    {"2023-06-01"},
		"Anthropic-Beta":                       {"a", "b"},
		"Idempotency-Key":                      {"retry-1"},
		"Anthropic-Ratelimit-Tokens-Remaining": {"1000"},
		"X-Ratelimit-Remaining-Tokens":         {"5"},
		"User-Agent":                           {"claude-cli/2.0"},
		"X-Claude-Code-Session-Id":             {"sess-1"},
	}
	got := redactHeaders(in)
	for _, dropped := range []string{"authorization", "x-api-key", "proxy-authorization", "cookie", "set-cookie", "x-goog-api-key", "x-session-token", "x-client-secret", "x-amz-credential", "x-refresh-token"} {
		if _, ok := got[dropped]; ok {
			t.Errorf("%s was kept", dropped)
		}
	}
	want := map[string]string{
		"anthropic-version":                    "2023-06-01",
		"anthropic-beta":                       "a, b",
		"idempotency-key":                      "retry-1",
		"anthropic-ratelimit-tokens-remaining": "1000",
		"x-ratelimit-remaining-tokens":         "5",
		"user-agent":                           "claude-cli/2.0",
		"x-claude-code-session-id":             "sess-1",
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %q, want %q", name, got[name], value)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d headers: %v", len(got), got)
	}
	if redactHeaders(nil) == nil {
		t.Errorf("nil input must give an empty map")
	}
}

func TestClientKeyFromHeaders(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
		want string
	}{
		{"bearer", http.Header{"Authorization": {"Bearer k1"}}, "k1"},
		{"bearer case", http.Header{"Authorization": {"bearer k1"}}, "k1"},
		{"raw authorization", http.Header{"Authorization": {"k1"}}, "k1"},
		{"bearer wins", http.Header{"Authorization": {"Bearer k1"}, "X-Api-Key": {"k2"}}, "k1"},
		{"x-api-key", http.Header{"X-Api-Key": {"k2"}}, "k2"},
		{"goog", http.Header{"X-Goog-Api-Key": {"k3"}}, "k3"},
		{"none", http.Header{"Content-Type": {"application/json"}}, ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		if got := clientKeyFromHeaders(tc.h); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestHashKey(t *testing.T) {
	// sha256("abc")
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := hashKey("abc"); got != want {
		t.Fatalf("hashKey = %s", got)
	}
	if clientKeyID(want) != "ba7816bf8f01" {
		t.Fatalf("clientKeyID = %s", clientKeyID(want))
	}
	if hashKey("") != "" || clientKeyID("") != "" {
		t.Fatalf("empty key must stay empty")
	}
}
