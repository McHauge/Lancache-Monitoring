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
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/s00500/env_logger"
)

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

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}

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
				emit(parsed)
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
