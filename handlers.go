package main

import (
	"context"
	"encoding/json"
	"fmt"
	htmltemplate "html/template"
	"net/http"
	"strings"
	"time"

	log "github.com/s00500/env_logger"
	"github.com/starfederation/datastar-go/datastar"
)

// pageData is the template data shared by every full-page render.
type pageData struct {
	ThemeCSS htmltemplate.CSS
	Page     string // "dashboard" or "ratelimit" — used to highlight the nav tab
	Title    string
	Body     any // page-specific data
}

// HandleIndex renders the dashboard page.
func (app *App) HandleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := pageData{
		ThemeCSS: app.ThemeCSS,
		Page:     "dashboard",
		Title:    "Lancache Monitor",
	}
	if err := app.DashboardTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Errorf("render dashboard: %v", err)
	}
}

// HandleRateLimitPage renders the rate-limit editor.
func (app *App) HandleRateLimitPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	current, err := app.RateLim.Read()
	if err != nil {
		log.Errorf("reading %s: %v", app.RateLim.Path, err)
		current = "# ERROR: could not read " + app.RateLim.Path + "\n# " + err.Error() + "\n"
	}
	doc, _ := ParseDoc(current)
	data := pageData{
		ThemeCSS: app.ThemeCSS,
		Page:     "ratelimit",
		Title:    "Rate Limits — Lancache Monitor",
		Body: map[string]any{
			"Path":            app.RateLim.Path,
			"Content":         current,
			"HasManaged":      doc.HasManaged,
			"Global":          doc.Global,
			"Overrides":       doc.Overrides,
			"ConnLimit":       doc.ConnLimit,
			"ConnLimitParsed": doc.ConnLimitParsed,
			"OverridesBody":   htmltemplate.HTML(renderOverrideRows(doc.Overrides)),
		},
	}
	if err := app.RateLimitTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Errorf("render ratelimit: %v", err)
	}
}

// renderOverrideRows builds the <tbody id="overrides-body"> for the
// per-IP overrides table on /ratelimit. Used both for the initial page
// render and for SSE patches after a save so the table reflects on-disk
// state without a full reload. Element ID stays constant so Datastar's
// morph targets it on every push.
func renderOverrideRows(overrides []Override) string {
	var b strings.Builder
	b.WriteString(`<tbody id="overrides-body">`)
	if len(overrides) == 0 {
		b.WriteString(`<tr><td colspan="4" class="empty-state">No per-IP overrides set.</td></tr>`)
	} else {
		for _, o := range overrides {
			ip := htmlEscape(o.IP)
			rateCell := `<span class="muted">—</span>`
			switch o.Rate {
			case "":
				// keep the dash
			case "0":
				rateCell = "unlimited"
			default:
				rateCell = htmlEscape(o.Rate)
			}
			exemptCell := `<span class="muted">no</span>`
			exemptJS := "false"
			if o.Exempt {
				exemptCell = `<span class="ok-pill">yes</span>`
				exemptJS = "true"
			}
			editClick := fmt.Sprintf(
				`$override.ip=%q; $override.rate=%q; $override.totalRate=window.lcmRate.multiply(%q, $connLimit); $override.exempt=%s; $override.open=true`,
				o.IP, o.Rate, o.Rate, exemptJS,
			)
			clearClick := fmt.Sprintf(
				`$override.ip=%q; @post('/api/ratelimit/override/clear')`,
				o.IP,
			)
			fmt.Fprintf(&b,
				`<tr><td class="mono">%s</td><td class="mono">%s</td><td>%s</td><td class="num"><button class="btn-ghost btn-sm" data-on:click="%s">Edit</button> <button class="btn-ghost btn-sm btn-danger" data-on:click="%s">Clear</button></td></tr>`,
				ip, rateCell, exemptCell, htmlEscape(editClick), htmlEscape(clearClick))
		}
	}
	b.WriteString(`</tbody>`)
	return b.String()
}

// pushOverridesTable re-reads the rate-limit file and morphs the
// `<tbody id="overrides-body">` on the rate-limit page so the table
// reflects what's now on disk. Called from each override endpoint after
// a save attempt — works whether the save succeeded (new state) or
// rolled back (previous state). Errors are swallowed; the table just
// won't refresh, which is no worse than today.
func (app *App) pushOverridesTable(sse *datastar.ServerSentEventGenerator) {
	current, err := app.RateLim.Read()
	if err != nil {
		return
	}
	doc, err := ParseDoc(current)
	if err != nil {
		return
	}
	_ = sse.PatchElements(renderOverrideRows(doc.Overrides))
}

// HandleRateLimitLoad pushes the current file contents into $rateLimitContent.
// Used to refresh the editor without reloading the page.
func (app *App) HandleRateLimitLoad(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	current, err := app.RateLim.Read()
	if err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOutput": "ERROR reading file: " + err.Error(),
			"reloadOK":     false,
		})
		return
	}
	_ = sse.MarshalAndPatchSignals(map[string]any{
		"rateLimitContent": current,
		"reloadOutput":     "",
	})
}

// rateLimitSaveSignals is the request payload for /api/ratelimit/save.
type rateLimitSaveSignals struct {
	RateLimitContent string `json:"rateLimitContent"`
}

// HandleRateLimitSave validates, writes, then asks the lancache container to
// run `nginx -t && nginx -s reload`. On non-zero exit the previous file is
// restored and the nginx output is shown verbatim.
func (app *App) HandleRateLimitSave(w http.ResponseWriter, r *http.Request) {
	var sigs rateLimitSaveSignals
	if err := datastar.ReadSignals(r, &sigs); err != nil {
		log.Warnf("ratelimit save: bad signals: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	sse := datastar.NewSSE(w, r)
	app.saveAndReload(r.Context(), sse, sigs.RateLimitContent)
}

// saveAndReload writes content, runs nginx -t && nginx -s reload via docker
// exec, and rolls back on any failure. All status is reported via SSE
// signals (`reloadOK`, `reloadOutput`) plus a toast.
//
// Caller is responsible for sanity-checking the content shape they expect
// (e.g., that override mutations preserve required directives) — this helper
// runs the file-level SanityCheck and trusts whatever nginx reports.
func (app *App) saveAndReload(ctx context.Context, sse *datastar.ServerSentEventGenerator, content string) {
	if err := SanityCheck(content); err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK":     false,
			"reloadOutput": err.Error(),
		})
		_ = sse.ExecuteScript(toastJS("error", "Save rejected", err.Error()))
		return
	}

	backup, err := app.RateLim.Write(content)
	if err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK":     false,
			"reloadOutput": "writing file: " + err.Error(),
		})
		_ = sse.ExecuteScript(toastJS("error", "Save failed", err.Error()))
		return
	}

	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := app.Reloader.Reload(rctx)
	if err != nil {
		// Docker call itself failed (socket missing, container missing, etc.)
		// — file is already written, but nginx never reloaded. Roll back so
		// the running config matches what's on disk.
		if rbErr := backup.Restore(); rbErr != nil {
			log.Errorf("restore after docker error: %v", rbErr)
		}
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK":     false,
			"reloadOutput": "docker exec failed: " + err.Error() + "\n(file rolled back)",
		})
		_ = sse.ExecuteScript(toastJS("error", "Reload failed", err.Error()))
		return
	}

	if !result.OK {
		if rbErr := backup.Restore(); rbErr != nil {
			log.Errorf("restore after nginx -t failure: %v", rbErr)
		}
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK":     false,
			"reloadOutput": result.Combined() + fmt.Sprintf("\n[exit %d — file rolled back]", result.ExitCode),
		})
		_ = sse.ExecuteScript(toastJS("error", "nginx -t rejected the config",
			"Previous file restored. See output for details."))
		return
	}

	_ = sse.MarshalAndPatchSignals(map[string]any{
		"reloadOK":     true,
		"reloadOutput": result.Combined(),
	})
	_ = sse.ExecuteScript(toastJS("success", "Reloaded",
		"nginx accepted the new config and reloaded."))
}

// overrideSignals is the request payload for /api/ratelimit/override*.
// All fields are nested under `override` so the dashboard popover can drive
// them with `data-bind:override.*` signals.
type overrideSignals struct {
	Override struct {
		IP     string `json:"ip"`
		Rate   string `json:"rate"`
		Exempt bool   `json:"exempt"`
	} `json:"override"`
}

// HandleRateLimitOverrideSet upserts a per-IP override and reloads nginx.
// Empty rate = no bandwidth override; exempt may still be true.
func (app *App) HandleRateLimitOverrideSet(w http.ResponseWriter, r *http.Request) {
	var sigs overrideSignals
	if err := datastar.ReadSignals(r, &sigs); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)

	current, err := app.RateLim.Read()
	if err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK": false, "reloadOutput": "reading file: " + err.Error()})
		_ = sse.ExecuteScript(toastJS("error", "Read failed", err.Error()))
		return
	}
	doc, err := ParseDoc(current)
	if err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK": false, "reloadOutput": "parse: " + err.Error()})
		_ = sse.ExecuteScript(toastJS("error", "Parse failed", err.Error()))
		return
	}
	if !doc.HasManaged && sigs.Override.Rate != "" {
		// Setting a custom bandwidth requires the managed block — auto-migrate.
		if err := doc.Migrate(); err != nil {
			_ = sse.MarshalAndPatchSignals(map[string]any{
				"reloadOK": false, "reloadOutput": "migrate: " + err.Error()})
			_ = sse.ExecuteScript(toastJS("error", "Cannot enable overrides", err.Error()))
			return
		}
	}
	if err := doc.SetOverride(sigs.Override.IP, sigs.Override.Rate, sigs.Override.Exempt); err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK": false, "reloadOutput": err.Error()})
		_ = sse.ExecuteScript(toastJS("error", "Invalid override", err.Error()))
		return
	}
	out, err := doc.Emit()
	if err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK": false, "reloadOutput": "emit: " + err.Error()})
		_ = sse.ExecuteScript(toastJS("error", "Emit failed", err.Error()))
		return
	}
	app.saveAndReload(r.Context(), sse, out)
	app.pushOverridesTable(sse)
}

// HandleRateLimitOverrideClear removes any override (rate + exempt) for the
// given IP and reloads nginx.
func (app *App) HandleRateLimitOverrideClear(w http.ResponseWriter, r *http.Request) {
	var sigs overrideSignals
	if err := datastar.ReadSignals(r, &sigs); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)

	current, err := app.RateLim.Read()
	if err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK": false, "reloadOutput": "reading file: " + err.Error()})
		_ = sse.ExecuteScript(toastJS("error", "Read failed", err.Error()))
		return
	}
	doc, err := ParseDoc(current)
	if err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK": false, "reloadOutput": "parse: " + err.Error()})
		_ = sse.ExecuteScript(toastJS("error", "Parse failed", err.Error()))
		return
	}
	doc.ClearOverride(sigs.Override.IP)
	out, err := doc.Emit()
	if err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK": false, "reloadOutput": "emit: " + err.Error()})
		_ = sse.ExecuteScript(toastJS("error", "Emit failed", err.Error()))
		return
	}
	app.saveAndReload(r.Context(), sse, out)
	app.pushOverridesTable(sse)
}

// HandleRateLimitMigrate adds the managed region to a previously-untouched
// rate-limit.conf and reloads. No-op if the markers are already present.
func (app *App) HandleRateLimitMigrate(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)

	current, err := app.RateLim.Read()
	if err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK": false, "reloadOutput": "reading file: " + err.Error()})
		_ = sse.ExecuteScript(toastJS("error", "Read failed", err.Error()))
		return
	}
	doc, err := ParseDoc(current)
	if err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK": false, "reloadOutput": "parse: " + err.Error()})
		_ = sse.ExecuteScript(toastJS("error", "Parse failed", err.Error()))
		return
	}
	if err := doc.Migrate(); err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK": false, "reloadOutput": err.Error()})
		_ = sse.ExecuteScript(toastJS("error", "Migrate failed", err.Error()))
		return
	}
	out, err := doc.Emit()
	if err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK": false, "reloadOutput": "emit: " + err.Error()})
		_ = sse.ExecuteScript(toastJS("error", "Emit failed", err.Error()))
		return
	}
	app.saveAndReload(r.Context(), sse, out)
	app.pushOverridesTable(sse)
}

// HandleDashboardStream is a long-lived SSE that pushes dashboard updates
// every two seconds. It returns when the client disconnects. Scalar values
// are sent as Datastar signals; tabular data is rendered server-side and
// pushed as HTML morphs (Datastar v1 has no client-side `for` directive).
func (app *App) HandleDashboardStream(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	ctx := r.Context()

	push := func() error {
		ips := app.Live.SnapshotIPs()
		hosts := app.Live.SnapshotHosts()
		overrides, connLimit, connLimitParsed := readOverridesAndConnLimit(app.RateLim)

		minutes, err := app.Agg.LastMinutes(60)
		if err != nil {
			log.Warnf("dashboard query: %v", err)
		}

		dayFrom := time.Now().Add(-24 * time.Hour).Unix() / 60
		day, _ := app.Agg.SinceMinute(dayFrom)

		// Build chart payload as JSON for the client.
		type chartPoint struct {
			TS     int64   `json:"ts"`
			Mbps   float64 `json:"mbps"`
			HitGB  float64 `json:"hitGB"`
			MissGB float64 `json:"missGB"`
		}
		chart := make([]chartPoint, 0, len(minutes))
		for _, m := range minutes {
			totalBytes := float64(m.BytesHit + m.BytesMiss)
			chart = append(chart, chartPoint{
				TS:     m.TS * 60,
				Mbps:   (totalBytes * 8) / 60 / 1e6,
				HitGB:  float64(m.BytesHit) / 1e9,
				MissGB: float64(m.BytesMiss) / 1e9,
			})
		}
		chartJSON, _ := json.Marshal(chart)

		signals := map[string]any{
			"day": map[string]any{
				"bytesHitH":  humanBytes(day.BytesHit),
				"bytesMissH": humanBytes(day.BytesMiss),
				"hitPct":     int(day.ByteHitRatio() * 100),
				"requests":   day.RequestsTotal(),
			},
			"activeCount":     len(ips),
			"updated":         time.Now().Format("15:04:05"),
			"connLimit":       connLimit,
			"connLimitParsed": connLimitParsed,
		}
		if err := sse.MarshalAndPatchSignals(signals); err != nil {
			return err
		}

		// Render and patch the IP table body.
		if err := sse.PatchElements(renderIPRows(ips, overrides)); err != nil {
			return err
		}
		// Render and patch the host table body.
		if err := sse.PatchElements(renderHostRows(hosts)); err != nil {
			return err
		}

		// Stash the chart payload on the DOM and call the redraw.
		// json.Marshal output is safe to embed in a backtick string literal.
		js := fmt.Sprintf("window.lcmSetChart && window.lcmSetChart(%s);", string(chartJSON))
		return sse.ExecuteScript(js)
	}

	if err := push(); err != nil {
		return
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := push(); err != nil {
				return
			}
		}
	}
}

const maxIPRows = 30
const maxHostRows = 20

// readOverridesAndConnLimit loads the current per-IP overrides plus the
// `limit_conn perip` value (and whether it was actually parsed or fell back
// to the default). On any read/parse failure it returns the default conn
// limit with parsed=false and a nil override map.
func readOverridesAndConnLimit(rl *RateLimitFile) (map[string]Override, int, bool) {
	current, err := rl.Read()
	if err != nil {
		return nil, defaultConnLimit, false
	}
	doc, err := ParseDoc(current)
	if err != nil {
		return nil, defaultConnLimit, false
	}
	out := make(map[string]Override, len(doc.Overrides))
	for _, o := range doc.Overrides {
		out[o.IP] = o
	}
	return out, doc.ConnLimit, doc.ConnLimitParsed
}

// renderIPRows builds the <tbody id="active-ips-body"> fragment that morphs
// into the dashboard. Datastar matches by element ID and replaces children.
// Each row carries a per-IP Limit cell with a button that arms the page-level
// `$override` signal so the shared editor modal opens pre-populated.
func renderIPRows(ips []IPSnapshot, overrides map[string]Override) string {
	var b strings.Builder
	b.WriteString(`<tbody id="active-ips-body">`)
	if len(ips) == 0 {
		b.WriteString(`<tr><td colspan="7" class="empty-state">No active clients in the last 5 minutes.</td></tr>`)
	} else {
		for i, s := range ips {
			if i >= maxIPRows {
				break
			}
			ov, has := overrides[s.IP]
			limitCell := overrideCell(s.IP, ov, has)
			fmt.Fprintf(&b,
				`<tr><td class="mono">%s</td><td class="mono">%s</td><td class="num">%s</td><td class="num">%d</td><td class="num">%d%%</td><td class="muted">%s</td><td>%s</td></tr>`,
				htmlEscape(s.IP), htmlEscape(s.TopHost), humanBytes(s.Bytes),
				s.Requests, int(s.HitRatio*100), humanAgo(time.Since(s.LastSeen)),
				limitCell)
		}
	}
	b.WriteString(`</tbody>`)
	return b.String()
}

// overrideCell renders the Limit-column body for one IP row: a small badge
// describing the current override (if any) plus an "Edit" button that arms
// the shared `$override` signal so the page-level modal opens pre-filled.
// `data-on:click` JSON-encodes the IP so it survives quoting.
func overrideCell(ip string, ov Override, has bool) string {
	var badge string
	switch {
	case has && ov.Rate != "" && ov.Rate != "0":
		badge = `<span class="override-pill">` + htmlEscape(ov.Rate) + `</span>`
	case has && ov.Rate == "0":
		badge = `<span class="override-pill override-pill--unlimited">unlimited</span>`
	case has && ov.Exempt:
		badge = `<span class="override-pill override-pill--exempt">exempt</span>`
	default:
		badge = `<span class="muted">—</span>`
	}
	rate := htmlEscape(ov.Rate)
	exempt := "false"
	if ov.Exempt {
		exempt = "true"
	}
	// totalRate is computed client-side from the per-connection rate so it
	// stays in sync with $connLimit (which the dashboard SSE pushes).
	click := fmt.Sprintf(
		`$override.ip=%q; $override.rate=%q; $override.totalRate=window.lcmRate.multiply(%q, $connLimit); $override.exempt=%s; $override.open=true`,
		ip, rate, rate, exempt,
	)
	return fmt.Sprintf(
		`%s <button class="btn-ghost btn-sm" data-on:click="%s">Edit</button>`,
		badge, htmlEscape(click),
	)
}

func renderHostRows(hosts []HostSnapshot) string {
	var b strings.Builder
	b.WriteString(`<tbody id="top-hosts-body">`)
	if len(hosts) == 0 {
		b.WriteString(`<tr><td colspan="4" class="empty-state">No CDN traffic in the last 5 minutes.</td></tr>`)
	} else {
		for i, s := range hosts {
			if i >= maxHostRows {
				break
			}
			fmt.Fprintf(&b,
				`<tr><td class="mono">%s</td><td class="num">%s</td><td class="num">%d</td><td class="num">%d%%</td></tr>`,
				htmlEscape(s.Host), humanBytes(s.Bytes), s.Requests, int(s.HitRatio*100))
		}
	}
	b.WriteString(`</tbody>`)
	return b.String()
}

// htmlEscape is a tight subset of html.EscapeString — log values are
// already constrained by the lancache regex but this keeps us safe if the
// upstream ever logs something unusual.
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// HandleLogin renders the login form (GET) or processes a submission (POST).
func (app *App) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if !app.Auth.Enabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data := pageData{
			ThemeCSS: app.ThemeCSS,
			Page:     "login",
			Title:    "Lancache Monitor — Login",
			Body:     map[string]any{"Error": ""},
		}
		_ = app.LoginTmpl.ExecuteTemplate(w, "login", data)
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		pwd := r.PostForm.Get("password")
		if !app.Auth.Check(pwd) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			data := pageData{
				ThemeCSS: app.ThemeCSS,
				Page:     "login",
				Title:    "Lancache Monitor — Login",
				Body:     map[string]any{"Error": "Incorrect password"},
			}
			_ = app.LoginTmpl.ExecuteTemplate(w, "login", data)
			return
		}
		http.SetCookie(w, app.Auth.IssueCookie())
		next := r.URL.Query().Get("next")
		if next == "" || !strings.HasPrefix(next, "/") {
			next = "/"
		}
		http.Redirect(w, r, next, http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// HandleLogout clears the session cookie and redirects to /login.
func (app *App) HandleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, app.Auth.ClearCookie())
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// toastJS builds the JS the SSE handler emits to fire a basecoat toast.
// Layout's <section id="toaster"> listens for the event.
func toastJS(kind, title, msg string) string {
	return fmt.Sprintf(
		`document.dispatchEvent(new CustomEvent('basecoat:toast', {detail: {category: %q, title: %q, description: %q}}));`,
		kind, title, msg)
}

// humanBytes formats a byte count as a short string (e.g. "1.4 GB").
func humanBytes(n int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
		tib = 1024 * gib
	)
	switch {
	case n >= tib:
		return fmt.Sprintf("%.2f TiB", float64(n)/float64(tib))
	case n >= gib:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func humanAgo(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh ago", int(d.Hours()))
}

