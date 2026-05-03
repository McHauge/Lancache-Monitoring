package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const validRateLimit = `geo $rate_limit {
    default 1;
    10.0.0.1 0;
}

map $rate_limit $limit_key {
    0 "";
    1 $binary_remote_addr;
}

limit_conn_zone $limit_key zone=perip:10m;
limit_conn perip 10;
limit_rate 2500k;
`

func TestRateLimit_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rate-limit.conf")
	if err := os.WriteFile(path, []byte(validRateLimit), 0644); err != nil {
		t.Fatal(err)
	}
	r := &RateLimitFile{Path: path}

	got, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got != validRateLimit {
		t.Errorf("read mismatch")
	}

	updated := validRateLimit + "\n# new comment\n"
	backup, err := r.Write(updated)
	if err != nil {
		t.Fatal(err)
	}
	if backup.Content != validRateLimit {
		t.Errorf("backup content mismatch")
	}

	got2, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got2 != updated {
		t.Errorf("post-write read mismatch")
	}

	// Roll back via the backup and confirm.
	if err := backup.Restore(); err != nil {
		t.Fatal(err)
	}
	got3, _ := r.Read()
	if got3 != validRateLimit {
		t.Errorf("restore failed")
	}
}

func TestRateLimit_RejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rate-limit.conf")
	os.WriteFile(path, []byte(validRateLimit), 0644)
	r := &RateLimitFile{Path: path}

	if _, err := r.Write(""); !errors.Is(err, ErrInvalidRateLimit) {
		t.Errorf("empty: got %v", err)
	}
	if _, err := r.Write("just a comment"); !errors.Is(err, ErrInvalidRateLimit) {
		t.Errorf("garbage: got %v", err)
	}

	// Original file must be untouched after a rejected write.
	got, _ := r.Read()
	if got != validRateLimit {
		t.Errorf("file modified despite rejected write")
	}
}
