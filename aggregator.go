package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/s00500/env_logger"
	_ "modernc.org/sqlite"
)

// minuteBucket accumulates counters for a single minute before flushing to SQLite.
type minuteBucket struct {
	BytesHit     int64
	BytesMiss    int64
	RequestsHit  int64
	RequestsMiss int64
	Hosts        map[string]*hostBucket
	IPs          map[string]*ipBucket
}

type hostBucket struct {
	BytesHit  int64
	BytesMiss int64
}

type ipBucket struct {
	BytesHit     int64
	BytesMiss    int64
	RequestsHit  int64
	RequestsMiss int64
}

// Aggregator owns the SQLite handle and the in-memory minute bucket.
type Aggregator struct {
	mu             sync.Mutex
	db             *sql.DB
	currentMinute  int64 // unix minute the bucket belongs to
	bucket         *minuteBucket
	retentionDays  int
}

// NewAggregator opens (and creates) the SQLite database at path.
func NewAggregator(path string, retentionDays int) (*Aggregator, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite is happiest single-threaded for writes
	if err := initSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Aggregator{
		db:            db,
		retentionDays: retentionDays,
		bucket:        newBucket(),
	}, nil
}

func newBucket() *minuteBucket {
	return &minuteBucket{
		Hosts: make(map[string]*hostBucket),
		IPs:   make(map[string]*ipBucket),
	}
}

func initSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS minute_stats (
			ts            INTEGER PRIMARY KEY,
			bytes_hit     INTEGER NOT NULL DEFAULT 0,
			bytes_miss    INTEGER NOT NULL DEFAULT 0,
			requests_hit  INTEGER NOT NULL DEFAULT 0,
			requests_miss INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS minute_host_stats (
			ts         INTEGER NOT NULL,
			host       TEXT NOT NULL,
			bytes_hit  INTEGER NOT NULL DEFAULT 0,
			bytes_miss INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (ts, host)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_minute_host_ts ON minute_host_stats(ts)`,
		`CREATE TABLE IF NOT EXISTS minute_ip_stats (
			ts            INTEGER NOT NULL,
			ip            TEXT NOT NULL,
			bytes_hit     INTEGER NOT NULL DEFAULT 0,
			bytes_miss    INTEGER NOT NULL DEFAULT 0,
			requests_hit  INTEGER NOT NULL DEFAULT 0,
			requests_miss INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (ts, ip)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_minute_ip_ts ON minute_ip_stats(ts)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}
	}
	return nil
}

// Close flushes any pending bucket and closes the DB.
func (a *Aggregator) Close() error {
	a.mu.Lock()
	if a.currentMinute != 0 {
		_ = a.flushLocked(a.currentMinute, a.bucket)
		a.bucket = newBucket()
		a.currentMinute = 0
	}
	a.mu.Unlock()
	return a.db.Close()
}

// Ingest records one log line into the current minute bucket. If the line is
// in a different minute than the current bucket, the previous one is flushed.
func (a *Aggregator) Ingest(line LogLine) {
	minute := line.Time.Unix() / 60
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.currentMinute == 0 {
		a.currentMinute = minute
	}
	if minute != a.currentMinute {
		// Out-of-order or rolled over → flush whichever bucket we have.
		if err := a.flushLocked(a.currentMinute, a.bucket); err != nil {
			log.Warnf("flushing minute %d: %v", a.currentMinute, err)
		}
		a.bucket = newBucket()
		a.currentMinute = minute
	}

	if line.IsHit() {
		a.bucket.BytesHit += line.BytesSent
		a.bucket.RequestsHit++
	} else {
		a.bucket.BytesMiss += line.BytesSent
		a.bucket.RequestsMiss++
	}
	hb := a.bucket.Hosts[line.Host]
	if hb == nil {
		hb = &hostBucket{}
		a.bucket.Hosts[line.Host] = hb
	}
	if line.IsHit() {
		hb.BytesHit += line.BytesSent
	} else {
		hb.BytesMiss += line.BytesSent
	}

	if line.RemoteAddr != "" {
		ib := a.bucket.IPs[line.RemoteAddr]
		if ib == nil {
			ib = &ipBucket{}
			a.bucket.IPs[line.RemoteAddr] = ib
		}
		if line.IsHit() {
			ib.BytesHit += line.BytesSent
			ib.RequestsHit++
		} else {
			ib.BytesMiss += line.BytesSent
			ib.RequestsMiss++
		}
	}
}

// Run flushes the current bucket on each minute boundary and runs nightly retention.
func (a *Aggregator) Run(ctx context.Context) {
	flushTicker := time.NewTicker(60 * time.Second)
	defer flushTicker.Stop()
	retentionTicker := time.NewTicker(24 * time.Hour)
	defer retentionTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-flushTicker.C:
			a.mu.Lock()
			if a.currentMinute != 0 {
				if err := a.flushLocked(a.currentMinute, a.bucket); err != nil {
					log.Warnf("periodic flush: %v", err)
				}
				a.bucket = newBucket()
				a.currentMinute = time.Now().Unix() / 60
			}
			a.mu.Unlock()
		case <-retentionTicker.C:
			if err := a.applyRetention(); err != nil {
				log.Warnf("retention: %v", err)
			}
		}
	}
}

func (a *Aggregator) flushLocked(minute int64, b *minuteBucket) error {
	if b.RequestsHit+b.RequestsMiss == 0 {
		return nil
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT INTO minute_stats (ts, bytes_hit, bytes_miss, requests_hit, requests_miss)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(ts) DO UPDATE SET
			bytes_hit = bytes_hit + excluded.bytes_hit,
			bytes_miss = bytes_miss + excluded.bytes_miss,
			requests_hit = requests_hit + excluded.requests_hit,
			requests_miss = requests_miss + excluded.requests_miss
	`, minute, b.BytesHit, b.BytesMiss, b.RequestsHit, b.RequestsMiss)
	if err != nil {
		return err
	}
	for h, hb := range b.Hosts {
		_, err = tx.Exec(`
			INSERT INTO minute_host_stats (ts, host, bytes_hit, bytes_miss)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(ts, host) DO UPDATE SET
				bytes_hit = bytes_hit + excluded.bytes_hit,
				bytes_miss = bytes_miss + excluded.bytes_miss
		`, minute, h, hb.BytesHit, hb.BytesMiss)
		if err != nil {
			return err
		}
	}
	for ip, ib := range b.IPs {
		_, err = tx.Exec(`
			INSERT INTO minute_ip_stats (ts, ip, bytes_hit, bytes_miss, requests_hit, requests_miss)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(ts, ip) DO UPDATE SET
				bytes_hit = bytes_hit + excluded.bytes_hit,
				bytes_miss = bytes_miss + excluded.bytes_miss,
				requests_hit = requests_hit + excluded.requests_hit,
				requests_miss = requests_miss + excluded.requests_miss
		`, minute, ip, ib.BytesHit, ib.BytesMiss, ib.RequestsHit, ib.RequestsMiss)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *Aggregator) applyRetention() error {
	cutoff := time.Now().Add(-time.Duration(a.retentionDays) * 24 * time.Hour).Unix() / 60
	if _, err := a.db.Exec(`DELETE FROM minute_stats WHERE ts < ?`, cutoff); err != nil {
		return err
	}
	if _, err := a.db.Exec(`DELETE FROM minute_host_stats WHERE ts < ?`, cutoff); err != nil {
		return err
	}
	if _, err := a.db.Exec(`DELETE FROM minute_ip_stats WHERE ts < ?`, cutoff); err != nil {
		return err
	}
	return nil
}

// MinuteRow is a flattened row of per-minute totals.
type MinuteRow struct {
	TS           int64
	BytesHit     int64
	BytesMiss    int64
	RequestsHit  int64
	RequestsMiss int64
}

// LastMinutes returns minute rows from now-n minutes through now.
func (a *Aggregator) LastMinutes(n int) ([]MinuteRow, error) {
	from := time.Now().Unix()/60 - int64(n)
	return a.LastMinutesFrom(from)
}

// LastMinutesFrom returns per-minute rows with ts >= fromMinute. Pass 0 for
// "everything currently in the table" (subject to retention).
func (a *Aggregator) LastMinutesFrom(fromMinute int64) ([]MinuteRow, error) {
	rows, err := a.db.Query(`
		SELECT ts, bytes_hit, bytes_miss, requests_hit, requests_miss
		FROM minute_stats
		WHERE ts >= ?
		ORDER BY ts ASC
	`, fromMinute)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MinuteRow
	for rows.Next() {
		var r MinuteRow
		if err := rows.Scan(&r.TS, &r.BytesHit, &r.BytesMiss, &r.RequestsHit, &r.RequestsMiss); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HourlyFrom returns one row per hour bucket with ts >= fromMinute, summing
// the per-minute counters. The returned MinuteRow.TS is the first minute of
// each hour bucket (a unix-minute), so the existing chart label code (which
// multiplies TS by 60) keeps working unchanged.
func (a *Aggregator) HourlyFrom(fromMinute int64) ([]MinuteRow, error) {
	rows, err := a.db.Query(`
		SELECT (ts/60)*60 AS h,
		       COALESCE(SUM(bytes_hit), 0),
		       COALESCE(SUM(bytes_miss), 0),
		       COALESCE(SUM(requests_hit), 0),
		       COALESCE(SUM(requests_miss), 0)
		FROM minute_stats
		WHERE ts >= ?
		GROUP BY h
		ORDER BY h ASC
	`, fromMinute)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MinuteRow
	for rows.Next() {
		var r MinuteRow
		if err := rows.Scan(&r.TS, &r.BytesHit, &r.BytesMiss, &r.RequestsHit, &r.RequestsMiss); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClearAll wipes every row from minute_stats, minute_host_stats, and
// minute_ip_stats and resets the in-flight bucket so the current minute
// starts fresh on the next ingest.
func (a *Aggregator) ClearAll() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM minute_host_stats`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM minute_ip_stats`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM minute_stats`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.bucket = newBucket()
	a.currentMinute = 0
	return nil
}

// IPTotal is one row of per-IP traffic for a time range.
type IPTotal struct {
	IP           string
	BytesHit     int64
	BytesMiss    int64
	RequestsHit  int64
	RequestsMiss int64
	LastTS       int64 // newest minute (unix-minutes) where this IP had activity
}

func (i IPTotal) Total() int64         { return i.BytesHit + i.BytesMiss }
func (i IPTotal) RequestsTotal() int64 { return i.RequestsHit + i.RequestsMiss }
func (i IPTotal) HitRatio() float64 {
	tot := i.RequestsTotal()
	if tot == 0 {
		return 0
	}
	return float64(i.RequestsHit) / float64(tot)
}

// TopIPsSince returns the top N IPs by total bytes since fromMinute.
func (a *Aggregator) TopIPsSince(fromMinute int64, n int) ([]IPTotal, error) {
	rows, err := a.db.Query(`
		SELECT ip,
		       COALESCE(SUM(bytes_hit), 0),
		       COALESCE(SUM(bytes_miss), 0),
		       COALESCE(SUM(requests_hit), 0),
		       COALESCE(SUM(requests_miss), 0),
		       COALESCE(MAX(ts), 0)
		FROM minute_ip_stats
		WHERE ts >= ?
		GROUP BY ip
		ORDER BY SUM(bytes_hit + bytes_miss) DESC
		LIMIT ?
	`, fromMinute, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IPTotal
	for rows.Next() {
		var i IPTotal
		if err := rows.Scan(&i.IP, &i.BytesHit, &i.BytesMiss, &i.RequestsHit, &i.RequestsMiss, &i.LastTS); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// Totals is the cumulative summary over a time range.
type Totals struct {
	BytesHit     int64
	BytesMiss    int64
	RequestsHit  int64
	RequestsMiss int64
}

func (t Totals) BytesTotal() int64    { return t.BytesHit + t.BytesMiss }
func (t Totals) RequestsTotal() int64 { return t.RequestsHit + t.RequestsMiss }
func (t Totals) HitRatio() float64 {
	tot := t.RequestsTotal()
	if tot == 0 {
		return 0
	}
	return float64(t.RequestsHit) / float64(tot)
}
func (t Totals) ByteHitRatio() float64 {
	tot := t.BytesTotal()
	if tot == 0 {
		return 0
	}
	return float64(t.BytesHit) / float64(tot)
}

// SinceMinute returns cumulative totals for ts >= fromMinute.
func (a *Aggregator) SinceMinute(fromMinute int64) (Totals, error) {
	var t Totals
	err := a.db.QueryRow(`
		SELECT
			COALESCE(SUM(bytes_hit), 0),
			COALESCE(SUM(bytes_miss), 0),
			COALESCE(SUM(requests_hit), 0),
			COALESCE(SUM(requests_miss), 0)
		FROM minute_stats
		WHERE ts >= ?
	`, fromMinute).Scan(&t.BytesHit, &t.BytesMiss, &t.RequestsHit, &t.RequestsMiss)
	return t, err
}

// HostTotal is one row of per-host bytes for a time range.
type HostTotal struct {
	Host      string
	BytesHit  int64
	BytesMiss int64
}

func (h HostTotal) Total() int64 { return h.BytesHit + h.BytesMiss }

// TopHostsSince returns the top N hosts by total bytes since fromMinute.
func (a *Aggregator) TopHostsSince(fromMinute int64, n int) ([]HostTotal, error) {
	rows, err := a.db.Query(`
		SELECT host, SUM(bytes_hit), SUM(bytes_miss)
		FROM minute_host_stats
		WHERE ts >= ?
		GROUP BY host
		ORDER BY SUM(bytes_hit + bytes_miss) DESC
		LIMIT ?
	`, fromMinute, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostTotal
	for rows.Next() {
		var h HostTotal
		if err := rows.Scan(&h.Host, &h.BytesHit, &h.BytesMiss); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
