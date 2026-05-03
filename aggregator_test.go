package main

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestAgg(t *testing.T) *Aggregator {
	t.Helper()
	dir := t.TempDir()
	a, err := NewAggregator(filepath.Join(dir, "test.db"), 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestAggregator_IngestAndQuery(t *testing.T) {
	a := newTestAgg(t)

	now := time.Date(2026, 5, 3, 12, 0, 30, 0, time.UTC)
	a.Ingest(LogLine{Time: now, BytesSent: 100, CacheStatus: "HIT", Host: "steam"})
	a.Ingest(LogLine{Time: now, BytesSent: 200, CacheStatus: "MISS", Host: "steam"})
	a.Ingest(LogLine{Time: now, BytesSent: 50, CacheStatus: "HIT", Host: "blizzard"})

	// Force flush by ingesting a line in a later minute.
	later := now.Add(2 * time.Minute)
	a.Ingest(LogLine{Time: later, BytesSent: 1, CacheStatus: "MISS", Host: "epic"})

	// The first minute should have flushed; query it.
	fromMinute := now.Unix()/60 - 1
	totals, err := a.SinceMinute(fromMinute)
	if err != nil {
		t.Fatal(err)
	}
	if totals.BytesHit != 150 {
		t.Errorf("bytes_hit: got %d want 150", totals.BytesHit)
	}
	if totals.BytesMiss != 200 {
		t.Errorf("bytes_miss: got %d want 200", totals.BytesMiss)
	}
	if totals.RequestsHit != 2 {
		t.Errorf("requests_hit: got %d want 2", totals.RequestsHit)
	}
	if totals.RequestsTotal() != 3 {
		t.Errorf("total requests: got %d want 3", totals.RequestsTotal())
	}
}

func TestAggregator_TopHosts(t *testing.T) {
	a := newTestAgg(t)
	t0 := time.Date(2026, 5, 3, 12, 0, 30, 0, time.UTC)

	for i := 0; i < 5; i++ {
		a.Ingest(LogLine{Time: t0, BytesSent: 1000, CacheStatus: "HIT", Host: "steam"})
	}
	a.Ingest(LogLine{Time: t0, BytesSent: 100, CacheStatus: "MISS", Host: "blizzard"})
	// flush
	a.Ingest(LogLine{Time: t0.Add(2 * time.Minute), BytesSent: 1, CacheStatus: "MISS", Host: "epic"})

	from := t0.Unix()/60 - 1
	hosts, err := a.TopHostsSince(from, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) < 2 || hosts[0].Host != "steam" {
		t.Fatalf("expected steam first, got %+v", hosts)
	}
	if hosts[0].Total() != 5000 {
		t.Errorf("steam total: got %d", hosts[0].Total())
	}
}
