package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClickHouse records inserts and answers with a configurable status.
type fakeClickHouse struct {
	*httptest.Server
	status  atomic.Int32
	mu      sync.Mutex
	rows    []string
	queries []string
	users   []string
	keys    []string
}

func newFakeClickHouse(t *testing.T) *fakeClickHouse {
	f := &fakeClickHouse{}
	f.status.Store(http.StatusOK)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := int(f.status.Load())
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("Code: 27. DB::Exception: Cannot parse input: secret row content"))
			return
		}
		if r.Header.Get("Content-Encoding") != "gzip" {
			t.Errorf("insert not gzip-compressed")
		}
		zr, errReader := gzip.NewReader(r.Body)
		if errReader != nil {
			t.Errorf("gzip: %v", errReader)
			return
		}
		raw, _ := io.ReadAll(zr)
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.Query().Get("query"))
		f.users = append(f.users, r.Header.Get("X-ClickHouse-User"))
		f.keys = append(f.keys, r.Header.Get("X-ClickHouse-Key"))
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			f.rows = append(f.rows, line)
		}
		f.mu.Unlock()
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeClickHouse) rowCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

type writerHarness struct {
	cfg   *pluginConfig
	clock time.Time
	w     *writer
}

func newWriterHarness(t *testing.T, url string) *writerHarness {
	h := &writerHarness{cfg: defaultConfig(), clock: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	h.cfg.ClickHouseURL = url
	h.cfg.SpoolDir = filepath.Join(t.TempDir(), "spool")
	h.w = newWriter(func() *pluginConfig { return h.cfg }, func() time.Time { return h.clock })
	return h
}

func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "*"+spoolSuffix))
	return matches
}

func TestWriterSendsGzipBatchWithCredentials(t *testing.T) {
	ch := newFakeClickHouse(t)
	h := newWriterHarness(t, ch.URL)
	passwordFile := filepath.Join(t.TempDir(), "password")
	if errWrite := os.WriteFile(passwordFile, []byte("s3cret\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	h.cfg.ClickHousePasswordFile = passwordFile
	before := stats.rowsWritten.Load()
	h.w.add([]byte(`{"request_id":"a"}`))
	h.w.add([]byte(`{"request_id":"b"}`))
	h.w.cycle(true)
	if ch.rowCount() != 2 || stats.rowsWritten.Load() != before+2 {
		t.Fatalf("rows = %d", ch.rowCount())
	}
	if ch.queries[0] != "INSERT INTO `cliproxy_log`.`requests` FORMAT JSONEachRow" || ch.users[0] != "cliproxy_writer" || ch.keys[0] != "s3cret" {
		t.Fatalf("request wrong: %q %q %q", ch.queries[0], ch.users[0], ch.keys[0])
	}
}

func TestWriterSpoolsAndReplays(t *testing.T) {
	ch := newFakeClickHouse(t)
	ch.status.Store(http.StatusServiceUnavailable)
	h := newWriterHarness(t, ch.URL)
	spooledBefore, writtenBefore := stats.rowsSpooled.Load(), stats.rowsWritten.Load()

	h.w.add([]byte(`{"request_id":"a"}`))
	h.w.cycle(true)
	if files := spoolFiles(t, h.cfg.SpoolDir); len(files) != 1 || stats.rowsSpooled.Load() != spooledBefore+1 {
		t.Fatalf("spool files = %v", files)
	}
	if h.w.healthy || h.w.spoolBytes() == 0 {
		t.Fatalf("writer should be backing off with a non-empty spool")
	}

	// During backoff, new batches go straight to disk without a network attempt.
	ch.status.Store(http.StatusOK)
	h.w.add([]byte(`{"request_id":"b"}`))
	h.w.cycle(true)
	if ch.rowCount() != 0 || len(spoolFiles(t, h.cfg.SpoolDir)) != 2 {
		t.Fatalf("sent during backoff")
	}

	h.clock = h.clock.Add(minBackoff)
	h.w.cycle(true)
	if ch.rowCount() != 2 || len(spoolFiles(t, h.cfg.SpoolDir)) != 0 || h.w.spoolBytes() != 0 {
		t.Fatalf("replay incomplete: rows %d files %v", ch.rowCount(), spoolFiles(t, h.cfg.SpoolDir))
	}
	if stats.rowsWritten.Load() != writtenBefore+2 || !h.w.healthy {
		t.Fatalf("written = %d", stats.rowsWritten.Load()-writtenBefore)
	}
}

func TestWriterBackoffGrows(t *testing.T) {
	ch := newFakeClickHouse(t)
	ch.status.Store(http.StatusInternalServerError)
	h := newWriterHarness(t, ch.URL)
	h.w.add([]byte(`{}`))
	h.w.cycle(true)
	first := h.w.backoff
	h.clock = h.w.nextAttempt
	h.w.cycle(true)
	if h.w.backoff != 2*first {
		t.Fatalf("backoff %v then %v", first, h.w.backoff)
	}
}

func TestWriterDropsPastSpoolCap(t *testing.T) {
	ch := newFakeClickHouse(t)
	ch.status.Store(http.StatusServiceUnavailable)
	h := newWriterHarness(t, ch.URL)
	h.cfg.SpoolMaxBytes = 10
	before := stats.rowsDropped.Load()
	h.w.add([]byte(`{"request_id":"a"}`))
	h.w.add([]byte(`{"request_id":"b"}`))
	h.w.cycle(true)
	if len(spoolFiles(t, h.cfg.SpoolDir)) != 0 || stats.rowsDropped.Load() != before+2 {
		t.Fatalf("dropped = %d", stats.rowsDropped.Load()-before)
	}
}

func TestWriterDropsRejectedBatch(t *testing.T) {
	ch := newFakeClickHouse(t)
	ch.status.Store(http.StatusBadRequest)
	h := newWriterHarness(t, ch.URL)
	before := stats.rowsRejected.Load()
	var logged []string
	hostLogger = func(raw []byte) { logged = append(logged, string(raw)) }
	defer func() { hostLogger = nil }()
	h.w.add([]byte(`{"request_id":"a"}`))
	h.w.cycle(true)
	if stats.rowsRejected.Load() != before+1 || len(spoolFiles(t, h.cfg.SpoolDir)) != 0 || !h.w.healthy {
		t.Fatalf("rejected batch handling wrong")
	}
	joined := strings.Join(logged, "\n")
	if !strings.Contains(joined, "Code: 27") || strings.Contains(joined, "secret row content") {
		t.Fatalf("log must carry the error code only: %s", joined)
	}
}

func TestWriterBufferCap(t *testing.T) {
	h := newWriterHarness(t, "http://127.0.0.1:1")
	h.cfg.BufferMaxBytes = 8
	before := stats.rowsDropped.Load()
	h.w.add([]byte("12345"))
	h.w.add([]byte("12345"))
	if len(h.w.rows) != 1 || stats.rowsDropped.Load() != before+1 {
		t.Fatalf("rows = %d", len(h.w.rows))
	}
}

func TestWriterReplaysSpoolLeftByPreviousRun(t *testing.T) {
	ch := newFakeClickHouse(t)
	h := newWriterHarness(t, ch.URL)
	if errMkdir := os.MkdirAll(h.cfg.SpoolDir, 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	body, _ := gzipRows([][]byte{[]byte(`{"request_id":"old1"}`), []byte(`{"request_id":"old2"}`)})
	if errWrite := os.WriteFile(filepath.Join(h.cfg.SpoolDir, "00000000000000000001-000001-2"+spoolSuffix), body, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errWrite := os.WriteFile(filepath.Join(h.cfg.SpoolDir, "interrupted"+spoolSuffix+".tmp"), []byte("x"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	before := stats.rowsWritten.Load()
	h.w.cycle(true)
	if ch.rowCount() != 2 || stats.rowsWritten.Load() != before+2 {
		t.Fatalf("rows = %d", ch.rowCount())
	}
	left, _ := os.ReadDir(h.cfg.SpoolDir)
	if len(left) != 0 {
		t.Fatalf("spool not emptied: %v", left)
	}
}

func TestWriterStopFlushes(t *testing.T) {
	ch := newFakeClickHouse(t)
	h := newWriterHarness(t, ch.URL)
	h.w.start()
	h.w.add([]byte(`{"request_id":"a"}`))
	h.w.stop(5 * time.Second)
	if ch.rowCount() != 1 {
		t.Fatalf("rows = %d", ch.rowCount())
	}
}

// A shutdown that finds ClickHouse stalled must leave both the in-flight batch and
// the rows buffered behind it in the spool, not lose them when the process exits.
func TestWriterStopSpoolsWhenClickHouseStalls(t *testing.T) {
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	ch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case received <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer ch.Close()
	defer close(release)
	h := newWriterHarness(t, ch.URL)
	h.cfg.FlushRows = 1
	spooledBefore := stats.rowsSpooled.Load()
	h.w.start()
	h.w.add([]byte(`{"request_id":"in-flight"}`))
	<-received
	h.w.add([]byte(`{"request_id":"buffered"}`))
	h.w.stop(10 * time.Millisecond)
	select {
	case <-h.w.done:
	default:
		t.Fatal("writer goroutine still running after stop")
	}
	if got := stats.rowsSpooled.Load() - spooledBefore; got != 2 {
		t.Fatalf("spooled rows = %d, want 2; files %v", got, spoolFiles(t, h.cfg.SpoolDir))
	}
}

// An HTTP 200 whose reply is cut off is not an acknowledgement: the batch is
// spooled for retry instead of being counted as written.
func TestWriterSpoolsWhenAcknowledgementIsCut(t *testing.T) {
	ch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, buf, errHijack := w.(http.Hijacker).Hijack()
		if errHijack != nil {
			t.Errorf("hijack: %v", errHijack)
			return
		}
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nOk.")
		_ = buf.Flush()
		_ = conn.Close()
	}))
	defer ch.Close()
	h := newWriterHarness(t, ch.URL)
	writtenBefore, spooledBefore := stats.rowsWritten.Load(), stats.rowsSpooled.Load()
	h.w.add([]byte(`{"request_id":"a"}`))
	h.w.cycle(true)
	if stats.rowsWritten.Load() != writtenBefore || stats.rowsSpooled.Load() != spooledBefore+1 || h.w.healthy {
		t.Fatalf("cut acknowledgement counted as written")
	}
}

func TestSpoolRowCount(t *testing.T) {
	if spoolRowCount("00000000000000000001-000001-42"+spoolSuffix) != 42 || spoolRowCount("junk") != 0 {
		t.Fatal("spool row count parse wrong")
	}
	if !bytes.Equal([]byte(clickHouseErrorCode([]byte("Code: 60. DB::Exception: Table missing"))), []byte("Code: 60")) {
		t.Fatal("error code parse wrong")
	}
}
