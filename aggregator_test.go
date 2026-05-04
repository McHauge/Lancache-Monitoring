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

func TestAggregator_HourlyFrom(t *testing.T) {
	a := newTestAgg(t)

	// Two ingests in hour 12, two in hour 13. The aggregator only flushes a
	// minute bucket when the next ingest crosses a minute boundary, so we
	// trail with one extra ingest in hour 14 to flush hour 13's last bucket.
	t0 := time.Date(2026, 5, 3, 12, 0, 30, 0, time.UTC)
	t1 := time.Date(2026, 5, 3, 12, 30, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 3, 13, 5, 0, 0, time.UTC)
	t3 := time.Date(2026, 5, 3, 13, 45, 0, 0, time.UTC)
	tFlush := time.Date(2026, 5, 3, 14, 0, 0, 0, time.UTC)

	a.Ingest(LogLine{Time: t0, BytesSent: 100, CacheStatus: "HIT", Host: "steam"})
	a.Ingest(LogLine{Time: t1, BytesSent: 200, CacheStatus: "MISS", Host: "steam"})
	a.Ingest(LogLine{Time: t2, BytesSent: 50, CacheStatus: "HIT", Host: "steam"})
	a.Ingest(LogLine{Time: t3, BytesSent: 25, CacheStatus: "MISS", Host: "steam"})
	a.Ingest(LogLine{Time: tFlush, BytesSent: 0, CacheStatus: "MISS", Host: "steam"})

	rows, err := a.HourlyFrom(0)
	if err != nil {
		t.Fatal(err)
	}
	// Two flushed buckets: hour 12 and hour 13. Hour 14's ingest is in-flight
	// (only flushed when a later ingest or Close runs).
	if len(rows) != 2 {
		t.Fatalf("expected 2 hourly buckets, got %d: %+v", len(rows), rows)
	}
	if rows[0].BytesHit != 100 || rows[0].BytesMiss != 200 {
		t.Errorf("hour 12: got hit=%d miss=%d, want 100/200", rows[0].BytesHit, rows[0].BytesMiss)
	}
	if rows[1].BytesHit != 50 || rows[1].BytesMiss != 25 {
		t.Errorf("hour 13: got hit=%d miss=%d, want 50/25", rows[1].BytesHit, rows[1].BytesMiss)
	}
	// rows[1].TS is in unix-minutes and should mark the start of the hour
	// (i.e. minute index = hours-since-epoch * 60).
	wantTS13 := time.Date(2026, 5, 3, 13, 0, 0, 0, time.UTC).Unix() / 60
	if rows[1].TS != wantTS13 {
		t.Errorf("hour 13 TS: got %d want %d", rows[1].TS, wantTS13)
	}
}

func TestAggregator_ClearAll(t *testing.T) {
	a := newTestAgg(t)

	t0 := time.Date(2026, 5, 3, 12, 0, 30, 0, time.UTC)
	a.Ingest(LogLine{Time: t0, BytesSent: 100, CacheStatus: "HIT", Host: "steam", RemoteAddr: "10.0.0.5"})
	// Force flush.
	a.Ingest(LogLine{Time: t0.Add(2 * time.Minute), BytesSent: 1, CacheStatus: "MISS", Host: "epic", RemoteAddr: "10.0.0.6"})

	if err := a.ClearAll(); err != nil {
		t.Fatal(err)
	}

	totals, err := a.SinceMinute(0)
	if err != nil {
		t.Fatal(err)
	}
	if totals.BytesTotal() != 0 || totals.RequestsTotal() != 0 {
		t.Errorf("expected zero totals after clear, got %+v", totals)
	}

	hosts, err := a.TopHostsSince(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 0 {
		t.Errorf("expected no hosts after clear, got %+v", hosts)
	}

	ips, err := a.TopIPsSince(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 0 {
		t.Errorf("expected no IPs after clear, got %+v", ips)
	}

	// In-flight bucket must be reset so the next ingest starts a fresh minute.
	if a.currentMinute != 0 {
		t.Errorf("currentMinute not reset: got %d", a.currentMinute)
	}
}

func TestAggregator_TopIPs(t *testing.T) {
	a := newTestAgg(t)
	t0 := time.Date(2026, 5, 3, 12, 0, 30, 0, time.UTC)

	for i := 0; i < 4; i++ {
		a.Ingest(LogLine{Time: t0, BytesSent: 1000, CacheStatus: "HIT", Host: "steam", RemoteAddr: "10.0.0.5"})
	}
	a.Ingest(LogLine{Time: t0, BytesSent: 200, CacheStatus: "MISS", Host: "blizzard", RemoteAddr: "10.0.0.6"})
	// Flush by ingesting a line in a later minute.
	a.Ingest(LogLine{Time: t0.Add(2 * time.Minute), BytesSent: 1, CacheStatus: "MISS", Host: "epic", RemoteAddr: "10.0.0.7"})

	ips, err := a.TopIPsSince(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) < 2 || ips[0].IP != "10.0.0.5" {
		t.Fatalf("expected 10.0.0.5 first, got %+v", ips)
	}
	if ips[0].Total() != 4000 {
		t.Errorf("10.0.0.5 total: got %d want 4000", ips[0].Total())
	}
	if ips[0].RequestsTotal() != 4 {
		t.Errorf("10.0.0.5 requests: got %d want 4", ips[0].RequestsTotal())
	}
	if ips[0].LastTS == 0 {
		t.Error("10.0.0.5 LastTS should be set")
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
