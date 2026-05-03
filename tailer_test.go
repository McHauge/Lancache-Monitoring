package main

import (
	"testing"
)

func TestParseLogLine_Hit(t *testing.T) {
	line := `[02/May/2026:14:23:01 +0200] 10.0.0.5 GET "/depot/123/chunk/abc" - 200 1048576 HIT lancache.steamcontent.com 200 0.123 "Valve/Steam"`
	parsed, ok := ParseLogLine(line)
	if !ok {
		t.Fatal("failed to parse")
	}
	if parsed.RemoteAddr != "10.0.0.5" {
		t.Errorf("addr: got %q", parsed.RemoteAddr)
	}
	if parsed.Method != "GET" {
		t.Errorf("method: got %q", parsed.Method)
	}
	if parsed.Status != 200 {
		t.Errorf("status: got %d", parsed.Status)
	}
	if parsed.BytesSent != 1048576 {
		t.Errorf("bytes: got %d", parsed.BytesSent)
	}
	if parsed.CacheStatus != "HIT" {
		t.Errorf("cache: got %q", parsed.CacheStatus)
	}
	if parsed.Host != "lancache.steamcontent.com" {
		t.Errorf("host: got %q", parsed.Host)
	}
	if !parsed.IsHit() {
		t.Error("expected IsHit() true")
	}
}

func TestParseLogLine_AllCacheStatuses(t *testing.T) {
	statuses := []string{"HIT", "MISS", "BYPASS", "EXPIRED", "STALE", "UPDATING", "REVALIDATED", "-"}
	for _, st := range statuses {
		line := `[02/May/2026:14:23:01 +0200] 10.0.0.5 GET "/x" - 200 100 ` + st + ` example.com 200 0.001 "ua"`
		parsed, ok := ParseLogLine(line)
		if !ok {
			t.Errorf("status %q: failed to parse", st)
			continue
		}
		if parsed.CacheStatus != st {
			t.Errorf("status %q: got %q", st, parsed.CacheStatus)
		}
	}
}

func TestParseLogLine_RevalidatedIsHit(t *testing.T) {
	line := `[02/May/2026:14:23:01 +0200] 10.0.0.5 GET "/x" - 200 100 REVALIDATED example.com 200 0.001 "ua"`
	parsed, ok := ParseLogLine(line)
	if !ok {
		t.Fatal("parse failed")
	}
	if !parsed.IsHit() {
		t.Error("REVALIDATED should count as hit")
	}
}

func TestParseLogLine_IPv4MappedIPv6Stripped(t *testing.T) {
	line := `[02/May/2026:14:23:01 +0200] ::ffff:10.0.0.5 GET "/x" - 200 100 HIT example.com 200 0.001 "ua"`
	parsed, ok := ParseLogLine(line)
	if !ok {
		t.Fatal("parse failed")
	}
	if parsed.RemoteAddr != "10.0.0.5" {
		t.Errorf("expected normalized IPv4, got %q", parsed.RemoteAddr)
	}
}

func TestParseLogLine_Garbage(t *testing.T) {
	if _, ok := ParseLogLine("this is not a log line\n"); ok {
		t.Error("garbage line should not parse")
	}
	if _, ok := ParseLogLine(""); ok {
		t.Error("empty line should not parse")
	}
}
