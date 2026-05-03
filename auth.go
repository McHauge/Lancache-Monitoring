package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookieName = "lcm_session"
	sessionMaxAge     = 7 * 24 * time.Hour
)

// Auth gates the dashboard behind a single shared password. If password is
// empty, all routes are open and no session cookies are issued — useful for
// dev and explicitly-LAN-only deployments.
type Auth struct {
	password string
	secret   []byte
}

// NewAuth returns an Auth gated on `password`. If password is empty, the
// gate is disabled.
func NewAuth(password, secret string) *Auth {
	return &Auth{password: password, secret: []byte(secret)}
}

// Enabled reports whether the password gate is active.
func (a *Auth) Enabled() bool { return a.password != "" }

// Check returns true if `attempt` matches the configured password (constant time).
func (a *Auth) Check(attempt string) bool {
	if !a.Enabled() {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(attempt), []byte(a.password)) == 1
}

// IssueCookie returns a fresh session cookie. The value is `expiry|hmac` so
// the server can verify it later without any persistent session store.
func (a *Auth) IssueCookie() *http.Cookie {
	exp := time.Now().Add(sessionMaxAge).Unix()
	mac := a.sign(strconv.FormatInt(exp, 10))
	value := strconv.FormatInt(exp, 10) + "." + mac
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(exp, 0),
	}
}

// ClearCookie returns a cookie that immediately expires the session.
func (a *Auth) ClearCookie() *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}

// validCookie verifies the session cookie on r.
func (a *Auth) validCookie(r *http.Request) bool {
	if !a.Enabled() {
		return true
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return false
	}
	expStr, mac := parts[0], parts[1]
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return false
	}
	if time.Now().Unix() > exp {
		return false
	}
	expected := a.sign(expStr)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(mac)) == 1
}

func (a *Auth) sign(payload string) string {
	h := hmac.New(sha256.New, a.secret)
	h.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// Require wraps next so it only runs for authenticated requests; otherwise
// the user is redirected to /login.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.validCookie(r) {
			next.ServeHTTP(w, r)
			return
		}
		// For Datastar/SSE requests, returning HTML redirects breaks the stream;
		// just send 401 so the client knows to retry.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next := url(r.URL.RequestURI())
		http.Redirect(w, r, "/login?next="+next, http.StatusSeeOther)
	})
}

// url is a tiny helper that QuotePath is overkill for; we just escape spaces
// and ampersands so the redirect param stays clean.
func url(s string) string {
	r := strings.NewReplacer(" ", "%20", "&", "%26", "?", "%3F")
	return r.Replace(s)
}

// FormatDuration is a convenience for templates / log lines.
func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return d.Round(time.Second).String()
}
