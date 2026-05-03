package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	htmltemplate "html/template"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	log "github.com/s00500/env_logger"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

//go:embed themes/*.json
var themeFS embed.FS

// Theme holds a parsed theme definition.
type Theme struct {
	Name   string            `json:"name"`
	Label  string            `json:"label"`
	Colors map[string]string `json:"colors"`
}

// CSS renders the theme as a :root block of CSS custom properties,
// also remapping accent → --primary and accent-darker → --ring so
// Basecoat tokens pick up the theme automatically.
func (t Theme) CSS() string {
	var b strings.Builder
	b.WriteString(":root {\n")
	for _, key := range slices.Sorted(maps.Keys(t.Colors)) {
		fmt.Fprintf(&b, "  --theme-%s: %s;\n", key, t.Colors[key])
	}
	if accent, ok := t.Colors["accent"]; ok {
		fmt.Fprintf(&b, "  --primary: %s;\n", accent)
	}
	if accentDarker, ok := t.Colors["accent-darker"]; ok {
		fmt.Fprintf(&b, "  --ring: %s;\n", accentDarker)
	}
	b.WriteString("}")
	return b.String()
}

func loadTheme(name string) (Theme, error) {
	data, err := themeFS.ReadFile("themes/theme-" + name + ".json")
	if err != nil {
		return Theme{}, fmt.Errorf("theme %q not found: %w", name, err)
	}
	var t Theme
	if err := json.Unmarshal(data, &t); err != nil {
		return Theme{}, fmt.Errorf("parsing theme %q: %w", name, err)
	}
	return t, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// defaultDockerHost returns the OS-appropriate Docker daemon endpoint:
// the npipe on Windows (Docker Desktop) and the unix socket elsewhere.
func defaultDockerHost() string {
	if runtime.GOOS == "windows" {
		return "npipe:////./pipe/docker_engine"
	}
	return "unix:///var/run/docker.sock"
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return fallback
}

// loadEnv reads a .env file and sets any variables not already in the environment.
func loadEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.Index(line, "="); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			value := strings.TrimSpace(line[idx+1:])
			value = strings.Trim(value, `"'`)
			if os.Getenv(key) == "" {
				os.Setenv(key, value)
			}
		}
	}
}

// loadOrGenerateSessionSecret returns a 32-byte hex secret. If LCM_SESSION_SECRET
// is set in the environment, use it. Otherwise generate one and persist it next
// to the SQLite DB so sessions survive restarts.
func loadOrGenerateSessionSecret(dbPath string) string {
	if v := os.Getenv("LCM_SESSION_SECRET"); v != "" {
		return v
	}
	secretPath := dbPath + ".secret"
	if data, err := os.ReadFile(secretPath); err == nil && len(data) >= 32 {
		return strings.TrimSpace(string(data))
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("generating session secret: %v", err)
	}
	secret := hex.EncodeToString(buf)
	if err := os.WriteFile(secretPath, []byte(secret), 0600); err != nil {
		log.Warnf("could not persist session secret to %s: %v (sessions will not survive restart)", secretPath, err)
	}
	return secret
}

// App holds shared state for HTTP handlers and background workers.
type App struct {
	DashboardTmpl *htmltemplate.Template
	RateLimitTmpl *htmltemplate.Template
	LoginTmpl     *htmltemplate.Template
	ThemeCSS      htmltemplate.CSS

	Live     *LiveTracker
	Agg      *Aggregator
	RateLim  *RateLimitFile
	Reloader *Reloader
	Auth     *Auth
}

func main() {
	loadEnv(".env")

	addr := envOr("LCM_ADDR", ":8080")
	themeName := envOr("LCM_THEME", "teal")
	logPath := envOr("LCM_LOG_PATH", "/data/logs/access.log")
	dbPath := envOr("LCM_DB_PATH", "/data/monitor.db")
	rateLimitPath := envOr("LCM_RATELIMIT_PATH", "/etc/nginx/conf.d/rate-limit.conf")
	lancacheContainer := envOr("LCM_LANCACHE_CONTAINER", "lancache")
	dockerHost := envOr("LCM_DOCKER_HOST", defaultDockerHost())
	password := os.Getenv("LCM_PASSWORD")
	retentionDays := envInt("LCM_RETENTION_DAYS", 30)

	theme, err := loadTheme(themeName)
	if err != nil {
		log.Fatal("loading theme: ", err)
	}
	log.Infof("theme: %s (%s)", theme.Name, theme.Label)

	dashboardTmpl := htmltemplate.Must(htmltemplate.ParseFS(templateFS,
		"templates/layout.html", "templates/dashboard.html"))
	rateLimitTmpl := htmltemplate.Must(htmltemplate.ParseFS(templateFS,
		"templates/layout.html", "templates/ratelimit.html"))
	loginTmpl := htmltemplate.Must(htmltemplate.ParseFS(templateFS, "templates/login.html"))

	agg, err := NewAggregator(dbPath, retentionDays)
	if err != nil {
		log.Fatal("opening aggregator db: ", err)
	}
	defer agg.Close()

	live := NewLiveTracker(5*time.Minute, 10*time.Second)

	rateLim := &RateLimitFile{Path: rateLimitPath}

	reloader := NewReloader(dockerHost, lancacheContainer)

	sessionSecret := loadOrGenerateSessionSecret(dbPath)
	auth := NewAuth(password, sessionSecret)

	app := &App{
		DashboardTmpl: dashboardTmpl,
		RateLimitTmpl: rateLimitTmpl,
		LoginTmpl:     loginTmpl,
		ThemeCSS:      htmltemplate.CSS(theme.CSS()),
		Live:          live,
		Agg:           agg,
		RateLim:       rateLim,
		Reloader:      reloader,
		Auth:          auth,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go live.Run(ctx)
	go agg.Run(ctx)
	go func() {
		if err := TailLog(ctx, logPath, func(line LogLine) {
			live.Track(line)
			agg.Ingest(line)
		}); err != nil && ctx.Err() == nil {
			log.Errorf("log tailer stopped: %v", err)
		}
	}()

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatal("static sub fs: ", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	mux.HandleFunc("/login", app.HandleLogin)
	mux.HandleFunc("/logout", app.HandleLogout)

	mux.Handle("/", auth.Require(http.HandlerFunc(app.HandleIndex)))
	mux.Handle("/api/dashboard", auth.Require(http.HandlerFunc(app.HandleDashboardStream)))
	mux.Handle("/ratelimit", auth.Require(http.HandlerFunc(app.HandleRateLimitPage)))
	mux.Handle("/api/ratelimit/load", auth.Require(http.HandlerFunc(app.HandleRateLimitLoad)))
	mux.Handle("/api/ratelimit/save", auth.Require(http.HandlerFunc(app.HandleRateLimitSave)))

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0, // SSE streams have no write deadline
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Infof("listening on %s", addr)
		log.Infof("log: %s  db: %s  ratelimit: %s  container: %s",
			logPath, dbPath, rateLimitPath, lancacheContainer)
		if !auth.Enabled() {
			log.Warnf("LCM_PASSWORD not set — running without auth (LAN-only mode)")
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("server: ", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
}
