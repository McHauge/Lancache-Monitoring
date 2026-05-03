package main

import (
	"errors"
	"strings"
	"testing"
)

func TestParseDoc_NoManagedBlock(t *testing.T) {
	d, err := ParseDoc(validRateLimit)
	if err != nil {
		t.Fatal(err)
	}
	if d.HasManaged {
		t.Errorf("HasManaged=true on un-migrated file")
	}
	// Existing exempt entry: 10.0.0.1 0; in geo $rate_limit
	if len(d.Overrides) != 1 || d.Overrides[0].IP != "10.0.0.1" || !d.Overrides[0].Exempt {
		t.Errorf("expected one exempt override for 10.0.0.1, got %+v", d.Overrides)
	}
	if d.Global != "2500k" {
		t.Errorf("Global=%q, want 2500k", d.Global)
	}
}

func TestMigrate_AddsManagedBlock(t *testing.T) {
	d, err := ParseDoc(validRateLimit)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	if !d.HasManaged {
		t.Errorf("HasManaged=false after Migrate")
	}
	if d.Global != "2500k" {
		t.Errorf("Global=%q after Migrate, want 2500k", d.Global)
	}
	out, err := d.Emit()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, managedBeginMarker) || !strings.Contains(out, managedEndMarker) {
		t.Errorf("emitted output missing markers: %s", out)
	}
	if !strings.Contains(out, "limit_rate $lcm_effective_rate;") {
		t.Errorf("limit_rate not rewritten: %s", out)
	}
	if strings.Contains(out, "limit_rate 2500k;") {
		t.Errorf("original limit_rate value not replaced")
	}
	if !strings.Contains(out, `""           2500k;`) {
		t.Errorf("map block missing global fallback line: %s", out)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	d, err := ParseDoc(validRateLimit)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	first, err := d.Emit()
	if err != nil {
		t.Fatal(err)
	}
	d2, err := ParseDoc(first)
	if err != nil {
		t.Fatal(err)
	}
	if !d2.HasManaged {
		t.Fatal("re-parsed migrated doc lost HasManaged")
	}
	if err := d2.Migrate(); err != nil {
		t.Fatal(err)
	}
	second, err := d2.Emit()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("Migrate not idempotent.\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestMigrate_NoLimitRate(t *testing.T) {
	bad := `geo $rate_limit {
    default 1;
}
limit_conn perip 5;
`
	d, err := ParseDoc(bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(); !errors.Is(err, ErrNoLimitRate) {
		t.Errorf("Migrate without limit_rate: got %v, want ErrNoLimitRate", err)
	}
}

func TestSetOverride_AddsRate(t *testing.T) {
	d, _ := ParseDoc(validRateLimit)
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := d.SetOverride("10.0.0.5", "5000k", false); err != nil {
		t.Fatal(err)
	}
	out, err := d.Emit()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "10.0.0.5") || !strings.Contains(out, "5000k") {
		t.Errorf("override not in emitted output: %s", out)
	}

	// Round-trip: re-parsing the emitted output should yield the same override.
	d2, err := ParseDoc(out)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range d2.Overrides {
		if o.IP == "10.0.0.5" && o.Rate == "5000k" {
			found = true
		}
	}
	if !found {
		t.Errorf("round-trip lost override: %+v", d2.Overrides)
	}
}

func TestSetOverride_ExemptOnly(t *testing.T) {
	d, _ := ParseDoc(validRateLimit)
	d.Migrate()
	// Add an exempt-only entry (no custom rate).
	if err := d.SetOverride("10.0.0.99", "", true); err != nil {
		t.Fatal(err)
	}
	out, _ := d.Emit()
	if !strings.Contains(out, "10.0.0.99") || !strings.Contains(out, "0;") {
		t.Errorf("exempt entry missing in geo $rate_limit block: %s", out)
	}

	d2, _ := ParseDoc(out)
	var got *Override
	for i := range d2.Overrides {
		if d2.Overrides[i].IP == "10.0.0.99" {
			got = &d2.Overrides[i]
		}
	}
	if got == nil || !got.Exempt || got.Rate != "" {
		t.Errorf("exempt-only round-trip failed: %+v", got)
	}
}

func TestClearOverride_RemovesEntry(t *testing.T) {
	d, _ := ParseDoc(validRateLimit)
	d.Migrate()
	d.SetOverride("10.0.0.5", "5000k", false)
	d.SetOverride("10.0.0.6", "0", false)
	d.ClearOverride("10.0.0.5")

	out, _ := d.Emit()
	if strings.Contains(out, "10.0.0.5") {
		t.Errorf("cleared IP still present: %s", out)
	}
	if !strings.Contains(out, "10.0.0.6") {
		t.Errorf("untouched IP gone: %s", out)
	}
}

func TestSetOverride_RejectsBadIP(t *testing.T) {
	d, _ := ParseDoc(validRateLimit)
	d.Migrate()
	if err := d.SetOverride("not-an-ip", "5000k", false); !errors.Is(err, ErrInvalidIP) {
		t.Errorf("got %v, want ErrInvalidIP", err)
	}
}

func TestSetOverride_RejectsBadRate(t *testing.T) {
	d, _ := ParseDoc(validRateLimit)
	d.Migrate()
	if err := d.SetOverride("10.0.0.5", "fast", false); !errors.Is(err, ErrInvalidRate) {
		t.Errorf("got %v, want ErrInvalidRate", err)
	}
	// Acceptable values
	for _, ok := range []string{"0", "100k", "5m", "5000K", "1M"} {
		if err := d.SetOverride("10.0.0.5", ok, false); err != nil {
			t.Errorf("rate %q rejected: %v", ok, err)
		}
	}
}

func TestSetOverride_AcceptsCIDR(t *testing.T) {
	d, _ := ParseDoc(validRateLimit)
	d.Migrate()
	if err := d.SetOverride("10.0.0.0/24", "1000k", false); err != nil {
		t.Errorf("CIDR rejected: %v", err)
	}
}

func TestEmit_PreservesUserContentByteForByte(t *testing.T) {
	// User-specific content outside the managed block must survive a
	// no-op Set+Clear round trip with no whitespace damage.
	original := validRateLimit + "\n# user comment after limit_rate\n"
	d, _ := ParseDoc(original)
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	migrated, _ := d.Emit()
	if !strings.Contains(migrated, "# user comment after limit_rate") {
		t.Errorf("user comment lost during migration")
	}

	d2, _ := ParseDoc(migrated)
	d2.SetOverride("10.0.0.5", "5000k", false)
	d2.ClearOverride("10.0.0.5")
	out, _ := d2.Emit()
	if !strings.Contains(out, "# user comment after limit_rate") {
		t.Errorf("user comment lost during set/clear cycle")
	}
}

func TestParseDoc_ConnLimit(t *testing.T) {
	d, err := ParseDoc(validRateLimit)
	if err != nil {
		t.Fatal(err)
	}
	// validRateLimit has `limit_conn perip 10;`
	if d.ConnLimit != 10 || !d.ConnLimitParsed {
		t.Errorf("ConnLimit=%d Parsed=%v, want 10/true", d.ConnLimit, d.ConnLimitParsed)
	}
}

func TestParseDoc_ConnLimitFallback(t *testing.T) {
	noConn := `geo $rate_limit {
    default 1;
}
limit_rate 2500k;
`
	d, _ := ParseDoc(noConn)
	if d.ConnLimit != defaultConnLimit {
		t.Errorf("ConnLimit=%d, want %d", d.ConnLimit, defaultConnLimit)
	}
	if d.ConnLimitParsed {
		t.Errorf("ConnLimitParsed=true on missing directive")
	}
}

func TestParseDoc_ConnLimitCustom(t *testing.T) {
	custom := strings.Replace(validRateLimit, "limit_conn perip 10;", "limit_conn perip 25;", 1)
	d, _ := ParseDoc(custom)
	if d.ConnLimit != 25 || !d.ConnLimitParsed {
		t.Errorf("ConnLimit=%d Parsed=%v, want 25/true", d.ConnLimit, d.ConnLimitParsed)
	}
}

func TestEmit_GlobalSyncedOnReParse(t *testing.T) {
	// If the user manually edits the limit_rate fallback in the map block
	// (not recommended, but possible), re-parsing should pick it up.
	d, _ := ParseDoc(validRateLimit)
	d.Migrate()
	migrated, _ := d.Emit()

	swapped := strings.Replace(migrated, `""           2500k;`, `""           5000k;`, 1)
	d2, _ := ParseDoc(swapped)
	if d2.Global != "5000k" {
		t.Errorf("Global=%q, want 5000k", d2.Global)
	}
}
