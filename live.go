package main

import (
	"context"
	"sort"
	"sync"
	"time"
)

// activitySample holds rolling counters for either an IP or a host.
type activitySample struct {
	LastSeen   time.Time
	Bytes      int64
	Requests   int64
	Hits       int64
	Misses     int64
	TopHost    string  // for IP samples: most-seen host in the window
	hostCounts map[string]int64
}

// LiveTracker keeps a rolling window of recent activity, indexed by client IP
// and by upstream host. Stale entries are swept on a timer.
type LiveTracker struct {
	mu       sync.RWMutex
	window   time.Duration
	sweepInt time.Duration
	byIP     map[string]*activitySample
	byHost   map[string]*activitySample
}

// NewLiveTracker returns a tracker that keeps activity for `window` and sweeps
// stale entries every `sweep`.
func NewLiveTracker(window, sweep time.Duration) *LiveTracker {
	return &LiveTracker{
		window:   window,
		sweepInt: sweep,
		byIP:     make(map[string]*activitySample),
		byHost:   make(map[string]*activitySample),
	}
}

// Track records one log line into the rolling window.
func (lt *LiveTracker) Track(line LogLine) {
	if line.BytesSent <= 0 && line.Status == 0 {
		return
	}
	lt.mu.Lock()
	defer lt.mu.Unlock()

	ip := lt.byIP[line.RemoteAddr]
	if ip == nil {
		ip = &activitySample{hostCounts: make(map[string]int64)}
		lt.byIP[line.RemoteAddr] = ip
	}
	ip.LastSeen = line.Time
	ip.Bytes += line.BytesSent
	ip.Requests++
	if line.IsHit() {
		ip.Hits++
	} else {
		ip.Misses++
	}
	ip.hostCounts[line.Host]++
	if ip.hostCounts[line.Host] > ip.hostCounts[ip.TopHost] {
		ip.TopHost = line.Host
	}

	host := lt.byHost[line.Host]
	if host == nil {
		host = &activitySample{}
		lt.byHost[line.Host] = host
	}
	host.LastSeen = line.Time
	host.Bytes += line.BytesSent
	host.Requests++
	if line.IsHit() {
		host.Hits++
	} else {
		host.Misses++
	}
}

// Run sweeps stale entries until ctx is canceled.
func (lt *LiveTracker) Run(ctx context.Context) {
	t := time.NewTicker(lt.sweepInt)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			lt.sweep(now)
		}
	}
}

// Reset drops every tracked IP and host. Used after a "clear data" action so
// the dashboard tables snap to empty immediately without waiting for the
// rolling-window sweep.
func (lt *LiveTracker) Reset() {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	lt.byIP = make(map[string]*activitySample)
	lt.byHost = make(map[string]*activitySample)
}

func (lt *LiveTracker) sweep(now time.Time) {
	cutoff := now.Add(-lt.window)
	lt.mu.Lock()
	defer lt.mu.Unlock()
	for k, v := range lt.byIP {
		if v.LastSeen.Before(cutoff) {
			delete(lt.byIP, k)
		}
	}
	for k, v := range lt.byHost {
		if v.LastSeen.Before(cutoff) {
			delete(lt.byHost, k)
		}
	}
}

// IPSnapshot is one entry in the active-downloads view.
type IPSnapshot struct {
	IP       string
	Bytes    int64
	Requests int64
	HitRatio float64
	TopHost  string
	LastSeen time.Time
}

// HostSnapshot is one entry in the per-CDN summary.
type HostSnapshot struct {
	Host     string
	Bytes    int64
	Requests int64
	HitRatio float64
	LastSeen time.Time
}

// SnapshotIPs returns the current active IPs sorted by bytes desc.
func (lt *LiveTracker) SnapshotIPs() []IPSnapshot {
	lt.mu.RLock()
	defer lt.mu.RUnlock()
	out := make([]IPSnapshot, 0, len(lt.byIP))
	for ip, s := range lt.byIP {
		out = append(out, IPSnapshot{
			IP:       ip,
			Bytes:    s.Bytes,
			Requests: s.Requests,
			HitRatio: ratio(s.Hits, s.Requests),
			TopHost:  s.TopHost,
			LastSeen: s.LastSeen,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}

// SnapshotHosts returns the current active hosts sorted by bytes desc.
func (lt *LiveTracker) SnapshotHosts() []HostSnapshot {
	lt.mu.RLock()
	defer lt.mu.RUnlock()
	out := make([]HostSnapshot, 0, len(lt.byHost))
	for h, s := range lt.byHost {
		out = append(out, HostSnapshot{
			Host:     h,
			Bytes:    s.Bytes,
			Requests: s.Requests,
			HitRatio: ratio(s.Hits, s.Requests),
			LastSeen: s.LastSeen,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}

func ratio(hits, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}
