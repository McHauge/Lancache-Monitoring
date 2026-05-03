package main

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Marker lines that bracket the lcm-managed region in rate-limit.conf.
// Anything between these is owned by the monitor; everything outside
// stays user-editable through the raw textarea.
const (
	managedBeginMarker = "# === LCM-MANAGED: per-IP overrides — edit in the UI, not by hand ==="
	managedEndMarker   = "# === LCM-MANAGED END ==="
)

// effectiveRateVar is the variable the monitor wires into limit_rate when
// overrides are enabled.
const effectiveRateVar = "$lcm_effective_rate"

// rateValuePattern matches an nginx rate value: a positive integer with an
// optional k or m suffix, or a literal "0" meaning unlimited.
var rateValuePattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*[kKmM]?)$`)

// limitRateLine matches the directive in the user's section, e.g. `limit_rate 2500k;`.
// The first group is leading whitespace; the second is the value.
var limitRateLine = regexp.MustCompile(`(?m)^([ \t]*)limit_rate[ \t]+([^;\s]+)[ \t]*;`)

// limitConnPerIP matches `limit_conn perip <N>;` so the UI can convert
// "total bandwidth" to per-connection rate (since limit_rate is applied
// per connection in nginx, but users tend to think in client-total terms).
var limitConnPerIP = regexp.MustCompile(`(?m)^[ \t]*limit_conn[ \t]+perip[ \t]+([0-9]+)[ \t]*;`)

// defaultConnLimit is the assumed `limit_conn perip` value when we can't
// parse one from the file. Stock lancache uses 10.
const defaultConnLimit = 10

// geoRateLimitBlock matches the existing `geo $rate_limit { ... }` block. The
// inner body capture is what we mutate to toggle exempt-status per IP.
var geoRateLimitBlock = regexp.MustCompile(`(?s)geo\s+\$rate_limit\s*\{(.*?)\}`)

// overrideEntryLine matches one entry inside a managed geo block:
// optional whitespace, IP (or CIDR), whitespace, value, semicolon, optional comment.
var overrideEntryLine = regexp.MustCompile(`^\s*([0-9A-Fa-f:.\/]+)\s+([^;\s]+)\s*;`)

// Override is one per-IP rule.
type Override struct {
	IP     string // dotted-quad or CIDR; matches what nginx expects in geo
	Rate   string // "" = no bandwidth override; "0" = unlimited; else nginx rate ("5000k")
	Exempt bool   // present in geo $rate_limit with value 0
}

// HasRate reports whether this override carries a custom limit_rate value.
func (o Override) HasRate() bool { return o.Rate != "" }

// RateLimitDoc is the parsed view of a rate-limit.conf with the lcm-managed
// region split out. Mutations go through SetOverride / ClearOverride / Migrate
// and are re-emitted by Emit.
type RateLimitDoc struct {
	// Global is the value currently embedded in the user's `limit_rate <X>;`
	// directive. After migration this is the same string we use as the empty
	// fallback in the managed map block.
	Global string

	// HasManaged is true if both markers were present.
	HasManaged bool

	// Overrides are sorted by IP for stable output.
	Overrides []Override

	// ConnLimit is the parsed `limit_conn perip <N>;` value. Falls back to
	// defaultConnLimit (10) when the directive can't be found.
	ConnLimit int

	// ConnLimitParsed is false when ConnLimit fell back to the default. The
	// UI surfaces a warning in that case so users know the assumed value.
	ConnLimitParsed bool

	// preManaged is everything before the managed block (or "" if absent).
	preManaged string
	// postManaged is everything after. When HasManaged is false, the entire
	// file content lives here so we don't lose user content.
	postManaged string
}

// ErrNoLimitRate is returned by Migrate when the file has no `limit_rate <X>;`
// line to point at $lcm_effective_rate.
var ErrNoLimitRate = errors.New("rate-limit.conf has no `limit_rate <value>;` directive — cannot enable overrides")

// ErrInvalidIP is returned by SetOverride when the address doesn't parse.
var ErrInvalidIP = errors.New("invalid IP address")

// ErrInvalidRate is returned by SetOverride when the rate value isn't a
// recognized nginx limit_rate token (digits with optional k/m, or 0).
var ErrInvalidRate = errors.New("invalid rate (expected e.g. 2500k, 5m, or 0 for unlimited)")

// ParseDoc reads a rate-limit.conf and returns its parsed view. It never
// mutates content — failures only happen if the managed block is malformed.
func ParseDoc(content string) (RateLimitDoc, error) {
	d := RateLimitDoc{}

	beginIdx := strings.Index(content, managedBeginMarker)
	endIdx := strings.Index(content, managedEndMarker)

	if beginIdx >= 0 && endIdx > beginIdx {
		d.HasManaged = true
		d.preManaged = content[:beginIdx]
		// Move endIdx past the marker plus its trailing newline if present.
		afterEnd := endIdx + len(managedEndMarker)
		if afterEnd < len(content) && content[afterEnd] == '\n' {
			afterEnd++
		}
		d.postManaged = content[afterEnd:]
		managed := content[beginIdx+len(managedBeginMarker) : endIdx]
		if err := parseManaged(managed, &d); err != nil {
			return d, err
		}
	} else {
		d.preManaged = ""
		d.postManaged = content
	}

	// Parse exempt entries from the user-owned `geo $rate_limit { ... }` block.
	// These exist whether or not the managed block is present, so we look in
	// preManaged + postManaged.
	merged := d.preManaged + d.postManaged
	if m := geoRateLimitBlock.FindStringSubmatch(merged); m != nil {
		body := m[1]
		for _, line := range strings.Split(body, "\n") {
			trim := strings.TrimSpace(line)
			if trim == "" || strings.HasPrefix(trim, "#") {
				continue
			}
			if strings.HasPrefix(trim, "default") {
				continue
			}
			if em := overrideEntryLine.FindStringSubmatch(trim); em != nil {
				ip := em[1]
				val := em[2]
				if val == "0" {
					d.upsertExempt(ip, true)
				}
			}
		}
	}

	// Capture the user's current `limit_rate <X>;` value (skipping the
	// monitor's variable substitution). If the only limit_rate is the managed
	// $lcm_effective_rate, fall back to the map's empty-fallback we store in
	// d.Global during parseManaged.
	if d.Global == "" {
		if m := limitRateLine.FindAllStringSubmatch(merged, -1); m != nil {
			for _, mm := range m {
				v := mm[2]
				if v != effectiveRateVar {
					d.Global = v
					break
				}
			}
		}
	}

	// Parse `limit_conn perip <N>;` so the UI can derive total bandwidth.
	if m := limitConnPerIP.FindStringSubmatch(merged); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			d.ConnLimit = n
			d.ConnLimitParsed = true
		}
	}
	if d.ConnLimit <= 0 {
		d.ConnLimit = defaultConnLimit
		d.ConnLimitParsed = false
	}

	d.sortOverrides()
	return d, nil
}

// parseManaged extracts overrides + the map fallback from inside the markers.
func parseManaged(managed string, d *RateLimitDoc) error {
	// Pull everything between `geo $lcm_rate_override {` and the matching `}`.
	geoStart := strings.Index(managed, "geo $lcm_rate_override")
	if geoStart < 0 {
		return fmt.Errorf("managed region missing `geo $lcm_rate_override` block")
	}
	openBrace := strings.Index(managed[geoStart:], "{")
	if openBrace < 0 {
		return fmt.Errorf("managed geo block missing `{`")
	}
	openBrace += geoStart
	closeBrace := strings.Index(managed[openBrace:], "}")
	if closeBrace < 0 {
		return fmt.Errorf("managed geo block missing `}`")
	}
	closeBrace += openBrace

	body := managed[openBrace+1 : closeBrace]
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.HasPrefix(trim, "default") {
			continue
		}
		if em := overrideEntryLine.FindStringSubmatch(trim); em != nil {
			ip := em[1]
			val := em[2]
			d.upsertRate(ip, val)
		}
	}

	// Pull the empty-string fallback from the map block — that's our global.
	mapStart := strings.Index(managed, "map $lcm_rate_override")
	if mapStart >= 0 {
		mapOpen := strings.Index(managed[mapStart:], "{")
		mapClose := strings.Index(managed[mapStart:], "}")
		if mapOpen > 0 && mapClose > mapOpen {
			mapBody := managed[mapStart+mapOpen+1 : mapStart+mapClose]
			for _, line := range strings.Split(mapBody, "\n") {
				trim := strings.TrimSpace(line)
				// Looking for a line like: ""           2500k;
				if strings.HasPrefix(trim, `""`) {
					rest := strings.TrimSpace(strings.TrimPrefix(trim, `""`))
					rest = strings.TrimSuffix(rest, ";")
					rest = strings.TrimSpace(rest)
					if rest != "" {
						d.Global = rest
					}
					break
				}
			}
		}
	}

	return nil
}

// upsertRate sets the bandwidth value for ip, creating an entry if needed.
func (d *RateLimitDoc) upsertRate(ip, rate string) {
	for i := range d.Overrides {
		if d.Overrides[i].IP == ip {
			d.Overrides[i].Rate = rate
			return
		}
	}
	d.Overrides = append(d.Overrides, Override{IP: ip, Rate: rate})
}

// upsertExempt sets the exempt flag for ip, creating an entry if needed.
func (d *RateLimitDoc) upsertExempt(ip string, exempt bool) {
	for i := range d.Overrides {
		if d.Overrides[i].IP == ip {
			d.Overrides[i].Exempt = exempt
			return
		}
	}
	d.Overrides = append(d.Overrides, Override{IP: ip, Exempt: exempt})
}

func (d *RateLimitDoc) sortOverrides() {
	sort.Slice(d.Overrides, func(i, j int) bool {
		return d.Overrides[i].IP < d.Overrides[j].IP
	})
}

// validateIP accepts plain addresses or CIDR ranges.
func validateIP(s string) error {
	if strings.Contains(s, "/") {
		if _, err := netip.ParsePrefix(s); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidIP, s)
		}
		return nil
	}
	if _, err := netip.ParseAddr(s); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidIP, s)
	}
	return nil
}

func validateRate(s string) error {
	if !rateValuePattern.MatchString(s) {
		return fmt.Errorf("%w: %q", ErrInvalidRate, s)
	}
	return nil
}

// SetOverride upserts an override. Empty rate clears just the bandwidth side
// while preserving any exempt flag; pass exempt=false and rate="" to remove
// the entry entirely (use ClearOverride instead for clarity).
func (d *RateLimitDoc) SetOverride(ip, rate string, exempt bool) error {
	if err := validateIP(ip); err != nil {
		return err
	}
	if rate != "" {
		if err := validateRate(rate); err != nil {
			return err
		}
	}
	for i := range d.Overrides {
		if d.Overrides[i].IP == ip {
			d.Overrides[i].Rate = rate
			d.Overrides[i].Exempt = exempt
			d.sortOverrides()
			return nil
		}
	}
	d.Overrides = append(d.Overrides, Override{IP: ip, Rate: rate, Exempt: exempt})
	d.sortOverrides()
	return nil
}

// ClearOverride removes any rate and exempt entry for ip.
func (d *RateLimitDoc) ClearOverride(ip string) {
	out := d.Overrides[:0]
	for _, o := range d.Overrides {
		if o.IP != ip {
			out = append(out, o)
		}
	}
	d.Overrides = out
}

// Migrate adds the managed region and rewrites the user's `limit_rate <X>;`
// directive to use $lcm_effective_rate. Idempotent: if the managed region is
// already present, returns nil with no changes.
func (d *RateLimitDoc) Migrate() error {
	if d.HasManaged {
		return nil
	}
	user := d.postManaged
	m := limitRateLine.FindStringSubmatch(user)
	if m == nil {
		return ErrNoLimitRate
	}
	currentValue := m[2]
	if currentValue == effectiveRateVar {
		// Already pointing at the variable but no managed block — odd state,
		// but we can recover by adding the block with whatever we know as
		// global. Without a value, we have to bail.
		return ErrNoLimitRate
	}
	d.Global = currentValue

	// Replace the first occurrence of the matched line with the variable form,
	// preserving its leading whitespace.
	indent := m[1]
	newLine := indent + "limit_rate " + effectiveRateVar + ";"
	d.postManaged = strings.Replace(user, m[0], newLine, 1)
	d.HasManaged = true
	return nil
}

// Emit re-renders the full file. When HasManaged is true, the managed region
// is rebuilt from Overrides + Global. The user-owned `geo $rate_limit { ... }`
// block is mutated in place to add or remove `<IP> 0;` lines for exempt
// entries; everything else outside the markers is byte-preserved.
func (d RateLimitDoc) Emit() (string, error) {
	merged := d.preManaged + d.postManaged

	// Rewrite the geo $rate_limit block to reflect current Exempt flags.
	if loc := geoRateLimitBlock.FindStringIndex(merged); loc != nil {
		full := merged[loc[0]:loc[1]]
		updated, err := rewriteGeoRateLimit(full, d.Overrides)
		if err != nil {
			return "", err
		}
		merged = merged[:loc[0]] + updated + merged[loc[1]:]
	}

	if !d.HasManaged {
		return merged, nil
	}

	// Re-split merged at the managed insertion point. preManaged is the
	// anchor; we always re-emit the managed region right after it.
	pre := merged[:len(d.preManaged)]
	post := merged[len(d.preManaged):]

	var b strings.Builder
	b.WriteString(pre)
	b.WriteString(managedBeginMarker)
	b.WriteString("\n")
	b.WriteString("geo $lcm_rate_override {\n")
	b.WriteString("    default      \"\";\n")
	for _, o := range d.Overrides {
		if !o.HasRate() {
			continue
		}
		fmt.Fprintf(&b, "    %-16s %s;\n", o.IP, o.Rate)
	}
	b.WriteString("}\n")
	b.WriteString("map $lcm_rate_override $lcm_effective_rate {\n")
	if d.Global == "" {
		// Defensive: caller should have set this via Migrate, but if we got
		// here without one we'd produce broken nginx. Refuse instead.
		return "", fmt.Errorf("emit: managed map needs a global rate value (was Migrate called?)")
	}
	fmt.Fprintf(&b, "    \"\"           %s;\n", d.Global)
	b.WriteString("    default      $lcm_rate_override;\n")
	b.WriteString("}\n")
	b.WriteString(managedEndMarker)
	b.WriteString("\n")
	// post starts with whatever followed the original managed block (or the
	// whole user file if HasManaged was just flipped on by Migrate).
	if !strings.HasPrefix(post, "\n") && !strings.HasPrefix(post, " ") && post != "" {
		// add a blank line for readability
		b.WriteString("\n")
	}
	b.WriteString(post)
	return b.String(), nil
}

// rewriteGeoRateLimit rewrites the `geo $rate_limit { ... }` block so that
// its body matches the current set of Exempt overrides. It preserves the
// user's `default ...;` line and any comments, but replaces the per-IP entries.
func rewriteGeoRateLimit(block string, overrides []Override) (string, error) {
	openBrace := strings.Index(block, "{")
	closeBrace := strings.LastIndex(block, "}")
	if openBrace < 0 || closeBrace < openBrace {
		return block, fmt.Errorf("malformed geo $rate_limit block")
	}
	header := block[:openBrace+1]
	body := block[openBrace+1 : closeBrace]
	footer := block[closeBrace:]

	// Keep default + comments, drop existing per-IP value lines.
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case trim == "":
			kept = append(kept, line)
		case strings.HasPrefix(trim, "#"):
			kept = append(kept, line)
		case strings.HasPrefix(trim, "default"):
			kept = append(kept, line)
		default:
			// drop — we'll re-emit from overrides
		}
	}
	// Trim leading and trailing blank lines from kept so our re-emitted
	// block stays stable across round-trips. Without leading-trim we'd
	// accumulate one extra blank line per Emit.
	for len(kept) > 0 && strings.TrimSpace(kept[0]) == "" {
		kept = kept[1:]
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}

	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	for _, line := range kept {
		b.WriteString(line)
		b.WriteString("\n")
	}
	for _, o := range overrides {
		if !o.Exempt {
			continue
		}
		fmt.Fprintf(&b, "    %-16s 0;\n", o.IP)
	}
	b.WriteString(footer)
	return b.String(), nil
}
