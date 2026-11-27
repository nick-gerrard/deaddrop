package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html static
var assets embed.FS

const (
	createRateLimit  = 10
	createRateWindow = 10 * time.Minute
)

type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
	now    func() time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		hits:   make(map[string][]time.Time),
		limit:  limit,
		window: window,
		now:    time.Now,
	}
}

func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	cutoff := now.Add(-rl.window)
	ts := rl.hits[key]
	kept := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rl.limit {
		rl.hits[key] = kept
		return false
	}
	rl.hits[key] = append(kept, now)
	return true
}

func (rl *rateLimiter) Prune() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := rl.now().Add(-rl.window)
	for k, ts := range rl.hits {
		kept := ts[:0]
		for _, t := range ts {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(rl.hits, k)
		} else {
			rl.hits[k] = kept
		}
	}
}

func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if ip := strings.TrimSpace(strings.Split(xff, ",")[0]); ip != "" {
			return ip
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

type App struct {
	store         *Store
	tmpl          *template.Template
	assetV        string
	createLimiter *rateLimiter
	now           func() time.Time
	timerScale    float64
}

func (a *App) nowMs() int64 { return a.now().UnixMilli() }

func NewApp(store *Store) (*App, error) {
	t, err := template.ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	for _, f := range []string{"static/style.css", "static/htmx.min.js", "static/app.js", "static/favicon.svg"} {
		b, err := fs.ReadFile(assets, f)
		if err != nil {
			return nil, err
		}
		h.Write(b)
	}
	scale := 1.0
	if raw := os.Getenv("DEADDROP_TIMER_SCALE"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 && v <= 100 {
			scale = v
		}
	}
	return &App{
		store:         store,
		tmpl:          t,
		assetV:        hex.EncodeToString(h.Sum(nil))[:10],
		createLimiter: newRateLimiter(createRateLimit, createRateWindow),
		now:           time.Now,
		timerScale:    scale,
	}, nil
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "ok")
	})
	mux.Handle("GET /static/", cacheFor(http.FileServerFS(assets)))
	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("GET /lobby", a.lobby)
	mux.HandleFunc("POST /create", a.create)
	mux.HandleFunc("POST /join", a.joinByCode)
	mux.HandleFunc("GET /r/{code}", a.room)
	mux.HandleFunc("POST /r/{code}/join", a.joinRoom)
	mux.HandleFunc("GET /r/{code}/state", a.state)
	mux.HandleFunc("POST /r/{code}/team", a.act(a.doTeam))
	mux.HandleFunc("POST /r/{code}/start", a.act(a.doStart))
	mux.HandleFunc("POST /r/{code}/abort", a.act(a.doAbort))
	mux.HandleFunc("POST /r/{code}/clue", a.act(a.doClue))
	mux.HandleFunc("POST /r/{code}/reveal", a.act(a.doReveal))
	mux.HandleFunc("POST /r/{code}/end", a.act(a.doEnd))
	mux.HandleFunc("POST /r/{code}/new", a.act(a.doNewRound))
	mux.HandleFunc("POST /r/{code}/settings", a.actWithLimit(maxSettingsBody, a.doSettings))
	mux.HandleFunc("POST /r/{code}/series-reset", a.act(a.doSeriesReset))
	mux.HandleFunc("POST /r/{code}/mark", a.act(a.doMark))
	return secureHeaders(csrfProtect(mux))
}

func cacheFor(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

func secureHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		h.ServeHTTP(w, r)
	})
}

func csrfProtect(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && !sameOriginPOST(r) {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func sameOriginPOST(r *http.Request) bool {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" {
		return sfs == "same-origin" || sfs == "none"
	}
	orig := r.Header.Get("Origin")
	if orig == "" {
		return false
	}
	u, err := url.Parse(orig)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}
