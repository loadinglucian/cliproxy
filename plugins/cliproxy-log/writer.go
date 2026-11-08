package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	spoolSuffix       = ".jsonl.gz"
	minBackoff        = time.Second
	maxBackoff        = time.Minute
	sendTimeout       = time.Minute
	replayFilesPerRun = 20
	// stopSpoolTimeout bounds the local spool writes that follow a canceled
	// shutdown insert.
	stopSpoolTimeout = 5 * time.Second
)

// errRejected marks a batch ClickHouse refused as malformed (HTTP 400). Retrying
// cannot succeed, so the batch is dropped and counted instead of spooled forever.
var errRejected = errors.New("clickhouse rejected the batch")

// writer batches rows and inserts them into ClickHouse over HTTP with gzip request
// compression. Failed batches go to gzip spool files that are replayed, oldest
// first, once ClickHouse accepts inserts again.
type writer struct {
	cfg    func() *pluginConfig
	now    func() time.Time
	client *http.Client
	// ctx scopes every insert; stop cancels it when ClickHouse stalls shutdown.
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	rows      [][]byte
	rowsBytes int64

	kick   chan struct{}
	stopCh chan struct{}
	done   chan struct{}

	// Fields below are owned by the writer goroutine (or by stop after it exits).
	healthy     bool
	backoff     time.Duration
	nextAttempt time.Time
	seq         uint64

	spoolMu     sync.Mutex
	spoolDir    string
	spoolTotal  int64
	spoolLoaded bool
}

func newWriter(cfg func() *pluginConfig, now func() time.Time) *writer {
	ctx, cancel := context.WithCancel(context.Background())
	return &writer{
		cfg:     cfg,
		now:     now,
		client:  &http.Client{Timeout: sendTimeout},
		ctx:     ctx,
		cancel:  cancel,
		kick:    make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
		healthy: true,
	}
}

// add queues one encoded row. It never blocks on the network; when the in-memory
// buffer is over its cap the row is dropped and counted.
func (w *writer) add(raw []byte) {
	cfg := w.cfg()
	w.mu.Lock()
	if w.rowsBytes+int64(len(raw)) > cfg.BufferMaxBytes {
		w.mu.Unlock()
		stats.rowsDropped.Add(1)
		return
	}
	w.rows = append(w.rows, raw)
	w.rowsBytes += int64(len(raw))
	full := len(w.rows) >= cfg.FlushRows || w.rowsBytes >= defaultFlushBatchBytes
	w.mu.Unlock()
	if full {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
}

func (w *writer) start() { go w.run() }

// reconfigure makes the next cycle pick up a changed spool directory.
func (w *writer) reconfigure() {
	w.spoolMu.Lock()
	if w.spoolDir != w.cfg().SpoolDir {
		w.spoolLoaded = false
	}
	w.spoolMu.Unlock()
}

// stop flushes buffered rows once more (spooling them if ClickHouse is down) and
// waits at most timeout for the writer goroutine. If ClickHouse has not answered by
// then, it cancels the in-flight insert so that batch and the buffered rows go to
// the spool instead of being lost when the process exits, and waits at most
// stopSpoolTimeout more for those local writes.
func (w *writer) stop(timeout time.Duration) {
	defer w.cancel()
	close(w.stopCh)
	select {
	case <-w.done:
		return
	case <-time.After(timeout):
	}
	w.cancel()
	select {
	case <-w.done:
	case <-time.After(stopSpoolTimeout):
	}
}

func (w *writer) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.cfg().FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.safeCycle(true)
		case <-w.kick:
			w.safeCycle(true)
		case <-w.stopCh:
			// Final flush: one attempt, no replay, so shutdown stays short.
			w.healthy, w.nextAttempt = true, time.Time{}
			w.safeCycle(false)
			return
		}
	}
}

func (w *writer) safeCycle(replay bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			stats.panics.Add(1)
		}
	}()
	w.cycle(replay)
}

// cycle sends the buffered batch (or spools it while ClickHouse is backing off)
// and then replays spool files.
func (w *writer) cycle(replay bool) {
	w.mu.Lock()
	rows := w.rows
	w.rows, w.rowsBytes = nil, 0
	w.mu.Unlock()
	if len(rows) > 0 {
		body, errCompress := gzipRows(rows)
		if errCompress != nil {
			stats.rowsDropped.Add(uint64(len(rows)))
		} else {
			w.deliver(body, len(rows))
		}
	}
	if replay {
		w.replaySpool()
	}
}

func (w *writer) deliver(body []byte, n int) {
	if !w.healthy && w.now().Before(w.nextAttempt) {
		w.spool(body, n)
		return
	}
	errSend := w.send(body)
	switch {
	case errSend == nil:
		w.markHealthy()
		stats.rowsWritten.Add(uint64(n))
	case errors.Is(errSend, errRejected):
		stats.rowsRejected.Add(uint64(n))
		logWarn("cliproxy-log: ClickHouse rejected a batch; dropped", map[string]any{"rows": n, "error": errSend.Error()})
	default:
		w.markUnhealthy(errSend)
		w.spool(body, n)
	}
}

func (w *writer) markHealthy() {
	if !w.healthy {
		logInfo("cliproxy-log: ClickHouse reachable again; replaying spool", nil)
	}
	w.healthy, w.backoff, w.nextAttempt = true, 0, time.Time{}
}

func (w *writer) markUnhealthy(err error) {
	if w.healthy {
		logWarn("cliproxy-log: ClickHouse insert failed; spooling to disk", map[string]any{"error": err.Error()})
	}
	w.healthy = false
	if w.backoff == 0 {
		w.backoff = minBackoff
	} else {
		w.backoff = min(w.backoff*2, maxBackoff)
	}
	w.nextAttempt = w.now().Add(w.backoff)
}

// send posts one gzip JSONEachRow batch. Error messages carry only status and the
// ClickHouse exception code, never row content.
func (w *writer) send(body []byte) error {
	cfg := w.cfg()
	query := fmt.Sprintf("INSERT INTO `%s`.`requests` FORMAT JSONEachRow", cfg.ClickHouseDatabase)
	endpoint := cfg.ClickHouseURL + "/?" + url.Values{
		"query":                  {query},
		"date_time_input_format": {"best_effort"},
	}.Encode()
	req, errReq := http.NewRequestWithContext(w.ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return fmt.Errorf("build insert request: %w", errReq)
	}
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/x-ndjson")
	if cfg.ClickHouseUser != "" {
		req.Header.Set("X-ClickHouse-User", cfg.ClickHouseUser)
	}
	if cfg.ClickHousePasswordFile != "" {
		password, errRead := os.ReadFile(cfg.ClickHousePasswordFile)
		if errRead != nil {
			return fmt.Errorf("read clickhouse-password-file: %w", errRead)
		}
		req.Header.Set("X-ClickHouse-Key", strings.TrimSpace(string(password)))
	}
	resp, errDo := w.client.Do(req)
	if errDo != nil {
		return fmt.Errorf("clickhouse insert: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			logWarn("cliproxy-log: closing ClickHouse response failed", map[string]any{"error": errClose.Error()})
		}
	}()
	// An unread or cut-off reply is not an acknowledgement: the batch is retried
	// (possibly duplicated) rather than counted as written and lost.
	snippet, errRead := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if errRead == nil {
		_, errRead = io.Copy(io.Discard, resp.Body)
	}
	if errRead != nil {
		return fmt.Errorf("clickhouse insert status %d: read response: %w", resp.StatusCode, errRead)
	}
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	errSend := fmt.Errorf("clickhouse insert status %d %s", resp.StatusCode, clickHouseErrorCode(snippet))
	if resp.StatusCode == http.StatusBadRequest {
		return fmt.Errorf("%w: %v", errRejected, errSend)
	}
	return errSend
}

// clickHouseErrorCode extracts "Code: N" from an error body. The rest of the body
// can quote row data, so it is not logged.
func clickHouseErrorCode(body []byte) string {
	text := string(body)
	idx := strings.Index(text, "Code: ")
	if idx < 0 {
		return ""
	}
	end := idx + len("Code: ")
	for end < len(text) && text[end] >= '0' && text[end] <= '9' {
		end++
	}
	return text[idx:end]
}

func gzipRows(rows [][]byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, errWriter := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if errWriter != nil {
		return nil, errWriter
	}
	for _, r := range rows {
		if _, errWrite := zw.Write(r); errWrite != nil {
			return nil, errWrite
		}
		if _, errWrite := zw.Write([]byte{'\n'}); errWrite != nil {
			return nil, errWrite
		}
	}
	if errClose := zw.Close(); errClose != nil {
		return nil, errClose
	}
	return buf.Bytes(), nil
}

// loadSpool scans the spool directory once (and again after it changes) to learn
// its size and remove interrupted temp files.
func (w *writer) loadSpoolLocked() {
	dir := w.cfg().SpoolDir
	if w.spoolLoaded && w.spoolDir == dir {
		return
	}
	w.spoolDir, w.spoolTotal, w.spoolLoaded = dir, 0, true
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		return
	}
	for _, de := range entries {
		name := de.Name()
		path := filepath.Join(dir, name)
		if strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(path)
			continue
		}
		if !strings.HasSuffix(name, spoolSuffix) {
			continue
		}
		if info, errInfo := de.Info(); errInfo == nil {
			w.spoolTotal += info.Size()
		}
	}
}

// spool writes a batch to disk unless that would pass spool-max-bytes, in which
// case the rows are dropped and counted.
func (w *writer) spool(body []byte, n int) {
	w.spoolMu.Lock()
	defer w.spoolMu.Unlock()
	w.loadSpoolLocked()
	cfg := w.cfg()
	if w.spoolTotal+int64(len(body)) > cfg.SpoolMaxBytes {
		stats.rowsDropped.Add(uint64(n))
		return
	}
	if errMkdir := os.MkdirAll(w.spoolDir, 0o700); errMkdir != nil {
		stats.rowsDropped.Add(uint64(n))
		logWarn("cliproxy-log: cannot create spool directory; dropped batch", map[string]any{"rows": n, "error": errMkdir.Error()})
		return
	}
	w.seq++
	name := fmt.Sprintf("%020d-%06d-%d%s", w.now().UnixNano(), w.seq%1000000, n, spoolSuffix)
	final := filepath.Join(w.spoolDir, name)
	tmp := final + ".tmp"
	if errWrite := writeFileSync(tmp, body); errWrite != nil {
		_ = os.Remove(tmp)
		stats.rowsDropped.Add(uint64(n))
		logWarn("cliproxy-log: cannot write spool file; dropped batch", map[string]any{"rows": n, "error": errWrite.Error()})
		return
	}
	if errRename := os.Rename(tmp, final); errRename != nil {
		_ = os.Remove(tmp)
		stats.rowsDropped.Add(uint64(n))
		return
	}
	w.spoolTotal += int64(len(body))
	stats.rowsSpooled.Add(uint64(n))
}

// replaySpool sends spool files oldest first. While ClickHouse is backing off it
// waits; after the backoff the oldest file doubles as the health probe.
func (w *writer) replaySpool() {
	if !w.healthy && w.now().Before(w.nextAttempt) {
		return
	}
	w.spoolMu.Lock()
	w.loadSpoolLocked()
	dir := w.spoolDir
	total := w.spoolTotal
	w.spoolMu.Unlock()
	if total == 0 {
		return
	}
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, de := range entries {
		if strings.HasSuffix(de.Name(), spoolSuffix) {
			names = append(names, de.Name())
		}
	}
	sort.Strings(names)
	for i, name := range names {
		if i >= replayFilesPerRun {
			return
		}
		path := filepath.Join(dir, name)
		body, errFile := os.ReadFile(path)
		if errFile != nil {
			continue
		}
		n := spoolRowCount(name)
		if !gzipIntact(body) {
			// A file cut short (for example by a power loss) can never be inserted;
			// ClickHouse answers it with a 500 that would stall replay behind it.
			stats.rowsDropped.Add(uint64(n))
			logWarn("cliproxy-log: spool file is corrupt; dropped", map[string]any{"rows": n})
			w.removeSpoolFile(path, len(body))
			continue
		}
		errSend := w.send(body)
		switch {
		case errSend == nil:
			w.markHealthy()
			stats.rowsWritten.Add(uint64(n))
		case errors.Is(errSend, errRejected):
			stats.rowsRejected.Add(uint64(n))
			logWarn("cliproxy-log: ClickHouse rejected a spooled batch; dropped", map[string]any{"rows": n, "error": errSend.Error()})
		default:
			w.markUnhealthy(errSend)
			return
		}
		w.removeSpoolFile(path, len(body))
	}
}

func (w *writer) removeSpoolFile(path string, size int) {
	if errRemove := os.Remove(path); errRemove != nil {
		return
	}
	w.spoolMu.Lock()
	w.spoolTotal -= int64(size)
	if w.spoolTotal < 0 {
		w.spoolTotal = 0
	}
	w.spoolMu.Unlock()
}

// writeFileSync writes and fsyncs a new file, so a rename after it cannot leave a
// renamed but empty or partial spool file behind a power loss.
func writeFileSync(path string, data []byte) error {
	f, errOpen := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if errOpen != nil {
		return errOpen
	}
	_, errWrite := f.Write(data)
	if errWrite == nil {
		errWrite = f.Sync()
	}
	if errClose := f.Close(); errWrite == nil {
		errWrite = errClose
	}
	return errWrite
}

// gzipIntact reports whether body is a complete gzip stream.
func gzipIntact(body []byte) bool {
	zr, errReader := gzip.NewReader(bytes.NewReader(body))
	if errReader != nil {
		return false
	}
	_, errCopy := io.Copy(io.Discard, zr)
	return errCopy == nil
}

// spoolRowCount reads the row count encoded in a spool file name.
func spoolRowCount(name string) int {
	base := strings.TrimSuffix(name, spoolSuffix)
	idx := strings.LastIndex(base, "-")
	if idx < 0 {
		return 0
	}
	n, errParse := strconv.Atoi(base[idx+1:])
	if errParse != nil {
		return 0
	}
	return n
}

func (w *writer) spoolBytes() int64 {
	w.spoolMu.Lock()
	defer w.spoolMu.Unlock()
	return w.spoolTotal
}
