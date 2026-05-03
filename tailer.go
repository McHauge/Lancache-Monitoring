package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/s00500/env_logger"
)

// parseStats tracks per-minute parse counters plus a small set of unparseable
// line samples for debugging. Samples are logged the first few times they
// occur and never repeated, so a misconfigured log_format produces three
// example lines and then quiets down to a single "skipped N" line per minute.
var parseStats = struct {
	parsed   atomic.Int64
	skipped  atomic.Int64
	muSample sync.Mutex
	samples  int // number of sample lines already logged
}{}

const maxParseSamples = 3

// LogLine is one parsed lancache access log entry.
type LogLine struct {
	Time         time.Time
	RemoteAddr   string
	Method       string
	URI          string
	Status       int
	BytesSent    int64
	CacheStatus  string // HIT, MISS, BYPASS, EXPIRED, STALE, UPDATING, REVALIDATED, "-"
	Host         string
}

// IsHit reports whether the cache served the response from disk.
// REVALIDATED counts as a hit (we re-validated cached content but didn't re-download).
func (l LogLine) IsHit() bool {
	return l.CacheStatus == "HIT" || l.CacheStatus == "REVALIDATED"
}

// Stock lancache-monolithic log format (single line):
// [service] remote_addr / host upstream_cache_status remote_user [time_local] "request" status body_bytes_sent "referer" "user_agent" "upstream_cache_status" "host" "http_range"
//
// Example:
// [steam] 10.0.0.5 / - - - [02/May/2026:14:23:01 +0200] "GET /depot/123/chunk/abc HTTP/1.1" 200 1048576 "-" "Valve/Steam" "HIT" "lancache.steamcontent.com" "-"
//
// We capture: remote_addr, time_local, method, uri, status, bytes_sent,
// cache_status (the quoted one near the end — it carries HIT/MISS reliably),
// and the upstream host (the quoted host field after cache_status).
var logLineRE = regexp.MustCompile(
	`^\[\S+\] (\S+) \S+ \S+ \S+ \S+ \[([^\]]+)\] "(\S+) (\S+)[^"]*" (\d+) (\d+) "[^"]*" "[^"]*" "([^"]*)" "([^"]*)" "[^"]*"`,
)

// timeLocalLayout matches nginx $time_local: 02/Jan/2006:15:04:05 -0700
const timeLocalLayout = "02/Jan/2006:15:04:05 -0700"

// ParseLogLine parses a single nginx access log line in the lancache format.
// Returns (line, true) on success, (zero, false) if the line doesn't match.
func ParseLogLine(s string) (LogLine, bool) {
	m := logLineRE.FindStringSubmatch(s)
	if m == nil {
		return LogLine{}, false
	}
	t, err := time.Parse(timeLocalLayout, m[2])
	if err != nil {
		return LogLine{}, false
	}
	status, _ := strconv.Atoi(m[5])
	bytes, _ := strconv.ParseInt(m[6], 10, 64)

	// Strip an optional ::ffff: prefix from IPv4-mapped IPv6 addresses.
	addr := m[1]
	if ip := net.ParseIP(addr); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			addr = v4.String()
		}
	}

	return LogLine{
		Time:        t,
		RemoteAddr:  addr,
		Method:      m[3],
		URI:         m[4],
		Status:      status,
		BytesSent:   bytes,
		CacheStatus: m[7],
		Host:        m[8],
	}, true
}

// logParseSample logs the first few unparseable lines verbatim so a
// misconfigured log_format is obvious in the container logs. Subsequent
// failures are silent until the next periodic stats tick.
func logParseSample(line string) {
	parseStats.muSample.Lock()
	defer parseStats.muSample.Unlock()
	if parseStats.samples >= maxParseSamples {
		return
	}
	parseStats.samples++
	if len(line) > 300 {
		line = line[:300] + "…"
	}
	log.Warnf("tailer: failed to parse line (sample %d/%d): %s",
		parseStats.samples, maxParseSamples, line)
}

// RunTailerStatsLogger emits a summary of how many lines the tailer parsed
// vs skipped during the previous minute. Skipped lines indicate either a
// malformed entry or — much more commonly — a log_format mismatch.
func RunTailerStatsLogger(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			parsed := parseStats.parsed.Swap(0)
			skipped := parseStats.skipped.Swap(0)
			if parsed == 0 && skipped == 0 {
				continue
			}
			if skipped > 0 {
				log.Warnf("tailer: parsed=%d skipped=%d (last 60s) — skipped > 0 means log_format mismatch", parsed, skipped)
			} else {
				log.Infof("tailer: parsed=%d skipped=0 (last 60s)", parsed)
			}
		}
	}
}

// TailLog follows path, parses each new line, and calls emit for each parsed
// LogLine. It handles log rotation by reopening the file when fsnotify reports
// the original was renamed or removed. It returns when ctx is canceled or on
// an unrecoverable error.
func TailLog(ctx context.Context, path string, emit func(LogLine)) error {
	for {
		if err := tailOne(ctx, path, emit); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Warnf("tailer error on %s: %v — retrying in 2s", path, err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		// tailOne returned nil → file was rotated; loop and reopen.
	}
}

// tailOne opens path, seeks to end, and streams new lines until the file is
// rotated (rename/remove) or ctx is canceled.
func tailOne(ctx context.Context, path string, emit func(LogLine)) error {
	// Wait for the file to exist if the lancache container hasn't started yet.
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		log.Infof("waiting for log file %s", path)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	log.Infof("tailer: opened %s (size=%d bytes, tailing from end)", path, offset)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	if err := watcher.Add(path); err != nil {
		return err
	}

	reader := bufio.NewReader(f)
	rotated := make(chan struct{}, 1)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-watcher.Events:
				if !ok {
					return
				}
				if ev.Op&(fsnotify.Rename|fsnotify.Remove) != 0 {
					select {
					case rotated <- struct{}{}:
					default:
					}
					return
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Warnf("fsnotify error: %v", err)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-rotated:
			return nil
		default:
		}

		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			if parsed, ok := ParseLogLine(line); ok {
				parseStats.parsed.Add(1)
				emit(parsed)
			} else if trimmed := strings.TrimRight(line, "\r\n"); trimmed != "" {
				parseStats.skipped.Add(1)
				logParseSample(trimmed)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-rotated:
					return nil
				case <-time.After(200 * time.Millisecond):
				}
				continue
			}
			return err
		}
	}
}
