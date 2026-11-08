//go:build e2e

// Package e2e runs the real proxy with the cliproxy-log plugin against a fake
// Claude/Codex upstream and a real ClickHouse, then checks the rows it wrote.
//
// Required: CLIPROXY_LOG_E2E_CLICKHOUSE_URL (for example http://127.0.0.1:8123,
// default user without password). Optional: CLIPROXY_LOG_E2E_SERVER_BIN and
// CLIPROXY_LOG_E2E_PLUGIN to reuse prebuilt artifacts; otherwise both are built.
package e2e

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	keyMapped   = "e2e-client-key-bigbook-0001"
	keyUnmapped = "e2e-client-key-unmapped-0002"
	upstreamKey = "sk-e2e-upstream-secret"
)

func TestPluginWritesRowsToClickHouse(t *testing.T) {
	chURL := strings.TrimRight(os.Getenv("CLIPROXY_LOG_E2E_CLICKHOUSE_URL"), "/")
	if chURL == "" {
		t.Skip("CLIPROXY_LOG_E2E_CLICKHOUSE_URL is not set")
	}
	root := repoRoot(t)
	work := t.TempDir()
	applySchema(t, chURL, root)

	serverBin := os.Getenv("CLIPROXY_LOG_E2E_SERVER_BIN")
	if serverBin == "" {
		serverBin = filepath.Join(work, "cli-proxy-api")
		goBuild(t, root, "-o", serverBin, "./cmd/server")
	}
	pluginDir := filepath.Join(work, "plugins")
	if errMkdir := os.MkdirAll(pluginDir, 0o755); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	pluginPath := filepath.Join(pluginDir, "cliproxy-log"+pluginExt())
	if prebuilt := os.Getenv("CLIPROXY_LOG_E2E_PLUGIN"); prebuilt != "" {
		copyFile(t, prebuilt, pluginPath)
	} else {
		goBuild(t, root, "-buildmode=c-shared", "-o", pluginPath, "./plugins/cliproxy-log")
	}

	upstream := newFakeUpstream(t, root)
	defer upstream.Close()

	port := freePort(t)
	marker := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	configPath := filepath.Join(work, "config.yaml")
	writeConfig(t, configPath, port, work, upstream.URL, chURL)

	logPath := filepath.Join(work, "proxy.log")
	logFile, errLog := os.Create(logPath)
	if errLog != nil {
		t.Fatal(errLog)
	}
	cmd := exec.Command(serverBin, "-config", configPath, "-local-model")
	cmd.Dir = work
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if errStart := cmd.Start(); errStart != nil {
		t.Fatal(errStart)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = logFile.Close()
		if t.Failed() {
			raw, _ := os.ReadFile(logPath)
			t.Logf("proxy log:\n%s", tail(string(raw), 8000))
		}
	}()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitReady(t, base)

	// 1. Claude, non-streaming, x-api-key from the mapped machine.
	status, body := post(t, base+"/v1/messages", map[string]string{"x-api-key": keyMapped, "anthropic-version": "2023-06-01"},
		fmt.Sprintf(`{"model":"claude-test-model","max_tokens":64,"messages":[{"role":"user","content":"%s claude-nonstream"}]}`, marker))
	expectOK(t, "claude non-stream", status, body)

	// 2. Claude, streaming, bearer key that is not in the machines map.
	status, body = post(t, base+"/v1/messages", map[string]string{"Authorization": "Bearer " + keyUnmapped, "anthropic-version": "2023-06-01"},
		fmt.Sprintf(`{"model":"claude-test-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"%s claude-stream"}]}`, marker))
	expectOK(t, "claude stream", status, body)

	// 3. count_tokens runs through the interceptors but must not be logged.
	status, body = post(t, base+"/v1/messages/count_tokens", map[string]string{"x-api-key": keyMapped, "anthropic-version": "2023-06-01"},
		fmt.Sprintf(`{"model":"claude-test-model","messages":[{"role":"user","content":"%s count-tokens"}]}`, marker))
	expectOK(t, "count_tokens", status, body)

	// 4. Codex Responses over HTTP streaming.
	status, body = post(t, base+"/v1/responses", map[string]string{"Authorization": "Bearer " + keyMapped},
		fmt.Sprintf(`{"model":"gpt-test-model","stream":true,"input":"%s codex-http"}`, marker))
	expectOK(t, "codex http", status, body)

	// 5. Codex Responses over a client WebSocket.
	runWebSocketTurn(t, base, marker)

	// 6. Model list goes through the response interceptor but is not a model call.
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+keyMapped)
	if resp, errDo := http.DefaultClient.Do(req); errDo == nil {
		_ = resp.Body.Close()
	}

	rows := waitRows(t, chURL, marker, 4)
	byCase := map[string]map[string]any{}
	for _, r := range rows {
		cr, _ := r["client_request"].(string)
		for _, name := range []string{"claude-nonstream", "claude-stream", "count-tokens", "codex-http", "codex-ws"} {
			if strings.Contains(cr, marker+" "+name) {
				byCase[name] = r
			}
		}
		raw, _ := json.Marshal(r)
		for _, secret := range []string{keyMapped, keyUnmapped, upstreamKey} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatalf("row %v contains a raw key", r["request_id"])
			}
		}
	}
	if _, found := byCase["count-tokens"]; found {
		t.Fatalf("count_tokens request was logged")
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}

	mappedID := hashHex(keyMapped)[:12]
	unmappedID := hashHex(keyUnmapped)[:12]

	ns := mustCase(t, byCase, "claude-nonstream")
	expectEq(t, ns, "endpoint", "/v1/messages")
	expectEq(t, ns, "transport", "http")
	expectEq(t, ns, "source_format", "claude")
	expectEq(t, ns, "stream", false)
	expectEq(t, ns, "status_code", float64(200))
	expectEq(t, ns, "outcome", "succeeded")
	expectEq(t, ns, "machine", "bigbook")
	expectEq(t, ns, "client_key_id", mappedID)
	expectEq(t, ns, "requested_model", "claude-test-model")
	expectEq(t, ns, "response_model", "claude-test-model")
	expectEq(t, ns, "response_kind", "json")
	expectEq(t, ns, "provider", "claude")
	expectEq(t, ns, "input_tokens", float64(25))
	expectEq(t, ns, "output_tokens", float64(11))
	expectContains(t, ns, "response", "non-streaming reply")
	expectNonEmpty(t, ns, "upstream_request")
	expectNonEmpty(t, ns, "trace_id")
	headers, _ := ns["request_headers"].(map[string]any)
	if headers["anthropic-version"] != "2023-06-01" {
		t.Fatalf("request_headers missing anthropic-version: %v", headers)
	}
	for _, name := range []string{"x-api-key", "authorization"} {
		if _, present := headers[name]; present {
			t.Fatalf("request_headers kept %s", name)
		}
	}

	st := mustCase(t, byCase, "claude-stream")
	expectEq(t, st, "stream", true)
	expectEq(t, st, "machine", "unknown")
	expectEq(t, st, "client_key_id", unmappedID)
	expectEq(t, st, "response_kind", "json")
	expectEq(t, st, "outcome", "succeeded")
	expectEq(t, st, "output_tokens", float64(89))
	var message struct {
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type     string         `json:"type"`
			Text     string         `json:"text"`
			Thinking string         `json:"thinking"`
			Input    map[string]any `json:"input"`
		} `json:"content"`
	}
	if errDecode := json.Unmarshal([]byte(st["response"].(string)), &message); errDecode != nil {
		t.Fatalf("stream response is not JSON: %v", errDecode)
	}
	if len(message.Content) != 3 || message.StopReason != "tool_use" ||
		message.Content[0].Thinking != "The user wants the weather. I should call get_weather." ||
		message.Content[1].Text != "Let me check <the> weather for you & San Francisco." ||
		message.Content[2].Input["location"] != "San Francisco, CA" {
		t.Fatalf("stream response not reassembled: %s", st["response"])
	}

	ch := mustCase(t, byCase, "codex-http")
	expectEq(t, ch, "endpoint", "/v1/responses")
	expectEq(t, ch, "transport", "http")
	expectEq(t, ch, "source_format", "openai-response")
	expectEq(t, ch, "stream", true)
	expectEq(t, ch, "machine", "bigbook")
	expectEq(t, ch, "response_kind", "json")
	expectEq(t, ch, "response_model", "gpt-test-model")
	expectEq(t, ch, "provider", "codex")
	expectContains(t, ch, "response", "Hello from Codex.")
	if ch["output_tokens"].(float64) == 0 {
		t.Fatalf("codex http row has no output tokens")
	}

	ws := mustCase(t, byCase, "codex-ws")
	expectEq(t, ws, "transport", "websocket")
	expectEq(t, ws, "endpoint", "/v1/responses")
	expectEq(t, ws, "machine", "bigbook")
	expectEq(t, ws, "response_kind", "json")
	expectEq(t, ws, "outcome", "succeeded")
	expectContains(t, ws, "response", "Hello from Codex.")
	if ws["output_tokens"].(float64) == 0 {
		t.Fatalf("codex websocket row has no output tokens")
	}
	for name, r := range byCase {
		if errs, _ := r["capture_errors"].([]any); len(errs) > 0 {
			t.Fatalf("%s has capture errors: %v", name, errs)
		}
		expectNonEmpty(t, r, "plugin_version")
	}
}

func runWebSocketTurn(t *testing.T, base, marker string) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/v1/responses"
	header := http.Header{"Authorization": {"Bearer " + keyMapped}}
	conn, resp, errDial := websocket.DefaultDialer.Dial(wsURL, header)
	if errDial != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("websocket dial: %v (status %d)", errDial, status)
	}
	// Keep the connection open until the test ends, as a real client does between
	// turns. Closing right after response.completed races the host's completion
	// of the turn, which then reports outcome "canceled".
	t.Cleanup(func() { _ = conn.Close() })
	msg := fmt.Sprintf(`{"type":"response.create","model":"gpt-test-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"%s codex-ws"}]}]}`, marker)
	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(msg)); errWrite != nil {
		t.Fatalf("websocket write: %v", errWrite)
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		_, data, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Fatalf("websocket read: %v", errRead)
		}
		var ev struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(data, &ev)
		if ev.Type == "response.completed" {
			return
		}
		if ev.Type == "error" || ev.Type == "response.failed" {
			t.Fatalf("websocket turn failed: %s", data)
		}
	}
}

func newFakeUpstream(t *testing.T, root string) *httptest.Server {
	t.Helper()
	testdata := filepath.Join(root, "plugins", "cliproxy-log", "testdata")
	anthropicStream := readFile(t, filepath.Join(testdata, "anthropic_stream.sse"))
	anthropicMessage := readFile(t, filepath.Join(testdata, "anthropic_message.json"))
	responsesStream := readFile(t, filepath.Join(testdata, "responses_stream.sse"))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/messages/count_tokens"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":12}`))
		case strings.HasSuffix(r.URL.Path, "/v1/messages"):
			var req struct {
				Stream bool `json:"stream"`
			}
			_ = json.Unmarshal(body, &req)
			w.Header().Set("Anthropic-Ratelimit-Tokens-Remaining", "1000")
			w.Header().Set("Set-Cookie", "upstream-session=secret")
			if req.Stream {
				writeSSE(w, anthropicStream)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(anthropicMessage)
		case strings.HasSuffix(r.URL.Path, "/responses"):
			writeSSE(w, responsesStream)
		default:
			http.NotFound(w, r)
		}
	}))
}

// writeSSE sends a fixture event by event so the proxy sees a real stream.
func writeSSE(w http.ResponseWriter, fixture []byte) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	for _, block := range bytes.SplitAfter(fixture, []byte("\n\n")) {
		if len(bytes.TrimSpace(block)) == 0 {
			continue
		}
		_, _ = w.Write(block)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func writeConfig(t *testing.T, path string, port int, work, upstreamURL, chURL string) {
	t.Helper()
	cfg := fmt.Sprintf(`config-version: 8
server:
  host: "127.0.0.1"
  port: %d
access:
  api-keys: [%q, %q]
oauth:
  auth-dir: %q
api-keys:
  claude:
    - name: fake-claude
      base-url: %q
      models:
        - name: "claude-test-model"
      keys:
        - api-key: %q
  codex:
    - name: fake-codex
      base-url: %q
      models:
        - name: "gpt-test-model"
      keys:
        - api-key: %q
plugins:
  enabled: true
  dir: %q
  configs:
    cliproxy-log:
      enabled: true
      priority: 1
      clickhouse-url: %q
      clickhouse-user: "default"
      spool-dir: %q
      machines:
        %s: "bigbook"
`, port, keyMapped, keyUnmapped, filepath.Join(work, "auths"),
		upstreamURL, upstreamKey, upstreamURL, upstreamKey,
		filepath.Join(work, "plugins"), chURL, filepath.Join(work, "spool"), hashHex(keyMapped))
	if errWrite := os.WriteFile(path, []byte(cfg), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
}

func applySchema(t *testing.T, chURL, root string) {
	t.Helper()
	path := filepath.Join(root, "deploy", "clickhouse", "schema", "001_requests.sql")
	t.Logf("applying schema %s", path)
	for _, stmt := range splitSQL(string(readFile(t, path))) {
		if _, errQuery := chQuery(chURL, stmt); errQuery != nil {
			t.Fatalf("schema statement failed: %v\n%s", errQuery, stmt)
		}
	}
}

// splitSQL drops "--" comment lines and splits on semicolons that end a line.
func splitSQL(sql string) []string {
	var kept []string
	for _, line := range strings.Split(sql, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	var out []string
	for _, stmt := range strings.Split(strings.Join(kept, "\n"), ";\n") {
		stmt = strings.TrimSuffix(strings.TrimSpace(stmt), ";")
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

func chQuery(chURL, query string) ([]byte, error) {
	resp, errPost := http.Post(chURL+"/?user=default", "text/plain", strings.NewReader(query))
	if errPost != nil {
		return nil, errPost
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

func waitRows(t *testing.T, chURL, marker string, want int) []map[string]any {
	t.Helper()
	query := fmt.Sprintf("SELECT * FROM cliproxy_log.requests WHERE position(client_request, '%s') > 0 FORMAT JSONEachRow SETTINGS output_format_json_quote_64bit_integers=0", marker)
	deadline := time.Now().Add(60 * time.Second)
	var rows []map[string]any
	for time.Now().Before(deadline) {
		body, errQuery := chQuery(chURL, query)
		if errQuery == nil {
			rows = rows[:0]
			scanner := bufio.NewScanner(bytes.NewReader(body))
			scanner.Buffer(make([]byte, 1<<20), 64<<20)
			for scanner.Scan() {
				var r map[string]any
				if json.Unmarshal(scanner.Bytes(), &r) == nil {
					rows = append(rows, r)
				}
			}
			if len(rows) >= want {
				// Give a late row (for example a wrongly logged count_tokens) a chance to land.
				time.Sleep(3 * time.Second)
				body, _ = chQuery(chURL, query)
				rows = rows[:0]
				scanner = bufio.NewScanner(bytes.NewReader(body))
				scanner.Buffer(make([]byte, 1<<20), 64<<20)
				for scanner.Scan() {
					var r map[string]any
					if json.Unmarshal(scanner.Bytes(), &r) == nil {
						rows = append(rows, r)
					}
				}
				return rows
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out waiting for %d rows, have %d", want, len(rows))
	return nil
}

func waitReady(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+keyMapped)
		if resp, errDo := http.DefaultClient.Do(req); errDo == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("proxy did not become ready")
}

func post(t *testing.T, target string, headers map[string]string, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, errDo := http.DefaultClient.Do(req)
	if errDo != nil {
		t.Fatalf("POST %s: %v", target, errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func expectOK(t *testing.T, name string, status int, body string) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("%s: status %d: %s", name, status, tail(body, 2000))
	}
}

func mustCase(t *testing.T, rows map[string]map[string]any, name string) map[string]any {
	t.Helper()
	r, ok := rows[name]
	if !ok {
		t.Fatalf("no row for %s", name)
	}
	return r
}

func expectEq(t *testing.T, r map[string]any, column string, want any) {
	t.Helper()
	if got := r[column]; got != want {
		t.Fatalf("%s: %s = %#v, want %#v", r["endpoint"], column, got, want)
	}
}

func expectContains(t *testing.T, r map[string]any, column, want string) {
	t.Helper()
	got, _ := r[column].(string)
	if !strings.Contains(got, want) {
		t.Fatalf("%s does not contain %q: %s", column, want, tail(got, 2000))
	}
}

func expectNonEmpty(t *testing.T, r map[string]any, column string) {
	t.Helper()
	if got, _ := r[column].(string); got == "" {
		t.Fatalf("%s is empty", column)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, errStat := os.Stat(filepath.Join(dir, "go.mod")); errStat == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func goBuild(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("go", append([]string{"build"}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, errBuild := cmd.CombinedOutput(); errBuild != nil {
		t.Fatalf("go build %v: %v\n%s", args, errBuild, out)
	}
}

func pluginExt() string {
	switch runtime.GOOS {
	case "darwin":
		return ".dylib"
	case "windows":
		return ".dll"
	default:
		return ".so"
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	if errWrite := os.WriteFile(dst, readFile(t, src), 0o755); errWrite != nil {
		t.Fatal(errWrite)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	return raw
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
