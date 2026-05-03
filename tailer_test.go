package main

import (
	"testing"
)

func TestParseLogLine_Hit(t *testing.T) {
	line := `[steam] 10.0.0.5 / - - - [02/May/2026:14:23:01 +0200] "GET /depot/123/chunk/abc HTTP/1.1" 200 1048576 "-" "Valve/Steam" "HIT" "lancache.steamcontent.com" "-"`
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
	if parsed.URI != "/depot/123/chunk/abc" {
		t.Errorf("uri: got %q", parsed.URI)
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
		line := `[steam] 10.0.0.5 / - - - [02/May/2026:14:23:01 +0200] "GET /x HTTP/1.1" 200 100 "-" "ua" "` + st + `" "example.com" "-"`
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
	line := `[steam] 10.0.0.5 / - - - [02/May/2026:14:23:01 +0200] "GET /x HTTP/1.1" 200 100 "-" "ua" "REVALIDATED" "example.com" "-"`
	parsed, ok := ParseLogLine(line)
	if !ok {
		t.Fatal("parse failed")
	}
	if !parsed.IsHit() {
		t.Error("REVALIDATED should count as hit")
	}
}

func TestParseLogLine_IPv4MappedIPv6Stripped(t *testing.T) {
	line := `[steam] ::ffff:10.0.0.5 / - - - [02/May/2026:14:23:01 +0200] "GET /x HTTP/1.1" 200 100 "-" "ua" "HIT" "example.com" "-"`
	parsed, ok := ParseLogLine(line)
	if !ok {
		t.Fatal("parse failed")
	}
	if parsed.RemoteAddr != "10.0.0.5" {
		t.Errorf("expected normalized IPv4, got %q", parsed.RemoteAddr)
	}
}

func TestParseLogLine_RealMissExample(t *testing.T) {
	// Verbatim line copied from a real lancache-monolithic install (status 503, MISS-equivalent).
	line := `[steam] 192.168.42.250 / - - - [03/May/2026:21:03:59 +0200] "GET /depot/378861/chunk/8afbce295b713479d1e97d5a3f917df183cc0170 HTTP/1.1" 503 206 "-" "Valve/Steam HTTP Client 1.0" "-" "cache5-ams1.steamcontent.com" "-"`
	parsed, ok := ParseLogLine(line)
	if !ok {
		t.Fatal("real lancache line failed to parse")
	}
	if parsed.RemoteAddr != "192.168.42.250" {
		t.Errorf("addr: got %q", parsed.RemoteAddr)
	}
	if parsed.Status != 503 {
		t.Errorf("status: got %d", parsed.Status)
	}
	if parsed.BytesSent != 206 {
		t.Errorf("bytes: got %d", parsed.BytesSent)
	}
	if parsed.CacheStatus != "-" {
		t.Errorf("cache: got %q", parsed.CacheStatus)
	}
	if parsed.Host != "cache5-ams1.steamcontent.com" {
		t.Errorf("host: got %q", parsed.Host)
	}
	if parsed.IsHit() {
		t.Error("expected IsHit() false")
	}
}

func TestParseLogLine_RealHitExample(t *testing.T) {
	line := `[steam] 192.168.42.250 / - - - [03/May/2026:21:03:59 +0200] "GET /depot/378861/chunk/a97cda87fdca68968510e890cebb6ee376ead112 HTTP/1.1" 200 1062944 "-" "Valve/Steam HTTP Client 1.0" "HIT" "cache5-ams1.steamcontent.com" "-"`
	parsed, ok := ParseLogLine(line)
	if !ok {
		t.Fatal("real lancache HIT line failed to parse")
	}
	if parsed.CacheStatus != "HIT" {
		t.Errorf("cache: got %q", parsed.CacheStatus)
	}
	if !parsed.IsHit() {
		t.Error("expected IsHit() true")
	}
	if parsed.BytesSent != 1062944 {
		t.Errorf("bytes: got %d", parsed.BytesSent)
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
