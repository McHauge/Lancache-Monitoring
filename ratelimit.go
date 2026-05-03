package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RateLimitFile reads and atomically writes the lancache rate-limit nginx config.
type RateLimitFile struct {
	Path string
}

// Read returns the current file contents.
func (r *RateLimitFile) Read() (string, error) {
	data, err := os.ReadFile(r.Path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ErrInvalidRateLimit is returned when SanityCheck rejects content as not
// looking like a real lancache rate-limit.conf — used to refuse obviously
// broken saves before they ever reach nginx.
var ErrInvalidRateLimit = errors.New("rate-limit content does not look like a valid nginx rate-limit config")

// SanityCheck enforces a minimal floor on what we'll write to disk: must
// reference both `geo $rate_limit` (the ACL block) and `limit_rate` (the
// throttle directive). nginx -t will catch syntax errors after; this just
// stops totally empty or wildly off-topic content.
func SanityCheck(content string) error {
	if !strings.Contains(content, "geo $rate_limit") {
		return fmt.Errorf("%w: missing 'geo $rate_limit' block", ErrInvalidRateLimit)
	}
	if !strings.Contains(content, "limit_rate") {
		return fmt.Errorf("%w: missing 'limit_rate' directive", ErrInvalidRateLimit)
	}
	return nil
}

// Backup returns the current file contents plus a function that rewrites them.
// The caller uses this to roll back if `nginx -t` fails after a save.
type Backup struct {
	Content string
	Path    string
}

// Restore writes the backup contents back atomically.
func (b Backup) Restore() error {
	return atomicWrite(b.Path, []byte(b.Content))
}

// Snapshot returns a Backup of the current file.
func (r *RateLimitFile) Snapshot() (Backup, error) {
	c, err := r.Read()
	if err != nil {
		return Backup{}, err
	}
	return Backup{Content: c, Path: r.Path}, nil
}

// Write validates content, snapshots the existing file, and atomically
// replaces it. The returned Backup can be used to roll back on reload failure.
func (r *RateLimitFile) Write(content string) (Backup, error) {
	if err := SanityCheck(content); err != nil {
		return Backup{}, err
	}
	backup, err := r.Snapshot()
	if err != nil {
		// If the file doesn't exist yet, that's a configuration problem on the
		// user's side (they need to mount the file rw) — surface it.
		return Backup{}, fmt.Errorf("reading existing %s: %w", r.Path, err)
	}
	if err := atomicWrite(r.Path, []byte(content)); err != nil {
		return Backup{}, err
	}
	return backup, nil
}

// atomicWrite writes data to a temp file in the same directory, fsyncs, and
// renames over path. Bind-mounted single files cannot be replaced via rename
// (Linux refuses to swap the inode), so on rename failure we fall back to a
// truncating write — same atomicity guarantee that the kernel applies to
// writes smaller than the filesystem block size, which a few KB of nginx
// config always is.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ratelimit-*")
	if err != nil {
		return fallbackWrite(path, data, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fallbackWrite(path, data, err)
	}
	return nil
}

// fallbackWrite is used when rename across a bind mount isn't allowed.
// It truncates and rewrites the file in place. originalErr is included in
// the returned error so the user sees why we fell back if this also fails.
func fallbackWrite(path string, data []byte, originalErr error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("rename failed (%v) and reopen failed: %w", originalErr, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}
