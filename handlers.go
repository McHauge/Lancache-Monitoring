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
	data := pageData{
		ThemeCSS: app.ThemeCSS,
		Page:     "ratelimit",
		Title:    "Rate Limits — Lancache Monitor",
		Body: map[string]any{
			"Path":    app.RateLim.Path,
			"Content": current,
		},
	}
	if err := app.RateLimitTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Errorf("render ratelimit: %v", err)
	}
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

	if err := SanityCheck(sigs.RateLimitContent); err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK":     false,
			"reloadOutput": err.Error(),
		})
		_ = sse.ExecuteScript(toastJS("error", "Save rejected", err.Error()))
		return
	}

	backup, err := app.RateLim.Write(sigs.RateLimitContent)
	if err != nil {
		_ = sse.MarshalAndPatchSignals(map[string]any{
			"reloadOK":     false,
			"reloadOutput": "writing file: " + err.Error(),
		})
		_ = sse.ExecuteScript(toastJS("error", "Save failed", err.Error()))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, err := app.Reloader.Reload(ctx)
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
			"activeCount": len(ips),
			"updated":     time.Now().Format("15:04:05"),
		}
		if err := sse.MarshalAndPatchSignals(signals); err != nil {
			return err
		}

		// Render and patch the IP table body.
		if err := sse.PatchElements(renderIPRows(ips)); err != nil {
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

// renderIPRows builds the <tbody id="active-ips-body"> fragment that morphs
// into the dashboard. Datastar matches by element ID and replaces children.
func renderIPRows(ips []IPSnapshot) string {
	var b strings.Builder
	b.WriteString(`<tbody id="active-ips-body">`)
	if len(ips) == 0 {
		b.WriteString(`<tr><td colspan="6" class="empty-state">No active clients in the last 5 minutes.</td></tr>`)
	} else {
		for i, s := range ips {
			if i >= maxIPRows {
				break
			}
			fmt.Fprintf(&b,
				`<tr><td class="mono">%s</td><td class="mono">%s</td><td class="num">%s</td><td class="num">%d</td><td class="num">%d%%</td><td class="muted">%s</td></tr>`,
				htmlEscape(s.IP), htmlEscape(s.TopHost), humanBytes(s.Bytes),
				s.Requests, int(s.HitRatio*100), humanAgo(time.Since(s.LastSeen)))
		}
	}
	b.WriteString(`</tbody>`)
	return b.String()
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

