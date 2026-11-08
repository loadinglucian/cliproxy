package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// droppedHeaders are removed outright, whatever the allowlist says.
var droppedHeaders = map[string]struct{}{
	"authorization":       {},
	"x-api-key":           {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
}

// sensitiveHeaderWords drop any header whose lowercase name contains one of them.
// "session-token" is covered by "token".
var sensitiveHeaderWords = []string{"token", "secret", "key", "credential"}

// allowedHeaders survive the word filter: they carry no secret but would match it.
var allowedHeaders = map[string]struct{}{
	"anthropic-version": {},
	"anthropic-beta":    {},
	"idempotency-key":   {},
}

// allowedHeaderPrefixes cover upstream rate-limit headers such as
// anthropic-ratelimit-tokens-remaining, which report counts, not credentials.
var allowedHeaderPrefixes = []string{"anthropic-ratelimit-", "x-ratelimit-"}

// redactHeaders returns lowercase header names mapped to comma-joined values with
// credentials removed. It returns an empty, non-nil map for nil input.
func redactHeaders(in http.Header) map[string]string {
	out := make(map[string]string, len(in))
	for name, values := range in {
		lower := strings.ToLower(strings.TrimSpace(name))
		if lower == "" || !headerAllowed(lower) {
			continue
		}
		joined := strings.Join(values, ", ")
		if existing, ok := out[lower]; ok {
			joined = existing + ", " + joined
		}
		out[lower] = joined
	}
	return out
}

func headerAllowed(lower string) bool {
	if _, drop := droppedHeaders[lower]; drop {
		return false
	}
	if _, ok := allowedHeaders[lower]; ok {
		return true
	}
	for _, prefix := range allowedHeaderPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	for _, word := range sensitiveHeaderWords {
		if strings.Contains(lower, word) {
			return false
		}
	}
	return true
}

// clientKeyFromHeaders extracts the client API key the same way the proxy's
// config access provider does: Authorization bearer first, then x-api-key, then
// x-goog-api-key. The caller must hash it immediately and drop the raw value.
func clientKeyFromHeaders(h http.Header) string {
	if h == nil {
		return ""
	}
	if auth := h.Get("Authorization"); auth != "" {
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			auth = strings.TrimSpace(parts[1])
		}
		if auth != "" {
			return auth
		}
	}
	if key := strings.TrimSpace(h.Get("X-Api-Key")); key != "" {
		return key
	}
	return strings.TrimSpace(h.Get("X-Goog-Api-Key"))
}

// hashKey returns the lowercase sha256 hex of a client key, or "" for an empty key.
func hashKey(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// clientKeyID is the short, non-reversible identifier stored in client_key_id.
func clientKeyID(hash string) string {
	if len(hash) < 12 {
		return hash
	}
	return hash[:12]
}
