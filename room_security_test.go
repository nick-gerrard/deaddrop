package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCSRFProtection(t *testing.T) {
	_, _, h := newTestApp(t)
	c := newClient(t, h)

	cases := []struct {
		name string
		hdrs map[string]string
		want int
	}{
		{"missing headers", nil, http.StatusForbidden},
		{"cross-site sec-fetch-site", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": c.base}, http.StatusForbidden},
		{"same-site sec-fetch-site", map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"mismatched origin", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"same-origin sec-fetch-site", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusSeeOther},
		{"none sec-fetch-site", map[string]string{"Sec-Fetch-Site": "none"}, http.StatusSeeOther},
		{"matching origin", map[string]string{"Origin": c.base}, http.StatusSeeOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl := newClient(t, h)
			code, _ := cl.postHeaders("/create", url.Values{"nickname": {"Fox"}}, tc.hdrs)
			if code != tc.want {
				t.Errorf("POST /create = %d, want %d", code, tc.want)
			}
		})
	}

	// Also verify room action rejects missing Origin/Sec-Fetch-Site.
	_, loc := c.post("/create", url.Values{"nickname": {"Fox"}})
	code, _ := c.postHeaders(loc+"/team", url.Values{"team": {"ember"}, "role": {"operative"}}, nil)
	if code != http.StatusForbidden {
		t.Errorf("POST %s/team without CSRF headers = %d, want 403", loc, code)
	}
}

func TestRateLimitAndRoomCap(t *testing.T) {
	store, app, h := newTestApp(t)
	nowT := time.Now()
	app.createLimiter.now = func() time.Time { return nowT }

	// 10 creates from same IP succeed, 11th returns 429.
	for i := range createRateLimit {
		c := newClient(t, h)
		code, _ := c.postHeaders("/create", url.Values{"nickname": {"P"}}, map[string]string{
			"Origin":    c.base,
			"X-Real-IP": "1.2.3.4",
		})
		if code != http.StatusSeeOther {
			t.Fatalf("create %d = %d, want 303", i+1, code)
		}
	}
	c := newClient(t, h)
	code, body := c.postHeaders("/create", url.Values{"nickname": {"P"}}, map[string]string{
		"Origin":    c.base,
		"X-Real-IP": "1.2.3.4",
	})
	if code != http.StatusTooManyRequests || !strings.Contains(body, "Too many rooms") {
		t.Fatalf("11th create = %d, want 429 friendly page", code)
	}

	// Different IP via X-Forwarded-For first entry still allowed.
	code, _ = c.postHeaders("/create", url.Values{"nickname": {"P"}}, map[string]string{
		"Origin":          c.base,
		"X-Forwarded-For": "5.6.7.8, 10.0.0.1",
	})
	if code != http.StatusSeeOther {
		t.Fatalf("other IP create = %d, want 303", code)
	}

	// Advance clock past window and prune -> 1.2.3.4 allowed again.
	nowT = nowT.Add(createRateWindow + time.Second)
	app.createLimiter.Prune()
	if len(app.createLimiter.hits) != 0 {
		t.Fatalf("Prune left %d entries", len(app.createLimiter.hits))
	}

	// Room cap: lower cap to current count and verify friendly 503 error page.
	var count int
	store.r.QueryRow(`SELECT COUNT(*) FROM rooms`).Scan(&count)
	store.maxRooms = count
	code, body = c.postHeaders("/create", url.Values{"nickname": {"P"}}, map[string]string{
		"Origin":    c.base,
		"X-Real-IP": "1.2.3.4",
	})
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "Too many active rooms") {
		t.Fatalf("over room cap = %d, want 503 friendly page", code)
	}
}

func TestStateTouchOnlyForMembers(t *testing.T) {
	store, _, h := newTestApp(t)
	a, outsider := newClient(t, h), newClient(t, h)
	_, loc := a.post("/create", url.Values{"nickname": {"A"}})
	room := strings.TrimPrefix(loc, "/r/")

	// Make room look stale (> 600s old).
	stale := now() - 1000
	if _, err := store.w.Exec(`UPDATE rooms SET updated_at=? WHERE code=?`, stale, room); err != nil {
		t.Fatal(err)
	}

	// Outsider polling /state must NOT touch updated_at.
	if code, _ := outsider.get("/r/" + room + "/state?v=1"); code != http.StatusForbidden {
		t.Fatalf("outsider state = %d, want 403", code)
	}
	_, updated, _, _ := store.RoomMeta(context.Background(), room, "")
	if updated != stale {
		t.Fatalf("outsider touched updated_at: got %d, want %d", updated, stale)
	}

	// Member polling /state DOES touch updated_at.
	if code, _ := a.get("/r/" + room + "/state?v=1"); code != http.StatusNoContent {
		t.Fatalf("member state = %d, want 204", code)
	}
	_, updated, _, _ = store.RoomMeta(context.Background(), room, "")
	if updated <= stale {
		t.Fatalf("member did not touch updated_at: got %d, stale %d", updated, stale)
	}
}

func TestReadOnlySnapshotAndHeaders(t *testing.T) {
	store, _, h := newTestApp(t)
	ctx := context.Background()
	if err := store.CreateRoom(ctx, "ABCD", newGame(), Player{ID: "p1", Nickname: "Alice", Team: "none", Role: "operative"}, false); err != nil {
		t.Fatal(err)
	}

	// Confirm concurrent read-only transactions work with modernc sqlite and reader pool.
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := store.Snapshot(ctx, "ABCD")
			if err != nil {
				errs <- err
				return
			}
			if snap.Room.Code != "ABCD" || len(snap.Players) != 1 {
				errs <- context.Canceled
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read-only snapshot failed: %v", err)
	}

	// Confirm CSP and security headers on HTML response.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("unexpected CSP: %q", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing X-Content-Type-Options")
	}
	if rec.Header().Get("Referrer-Policy") != "same-origin" {
		t.Errorf("missing Referrer-Policy")
	}

	// Confirm cleanNick strips Unicode Cf (e.g. bidi override U+202E).
	got, err := cleanNick("A\u202elice\u200b")
	if err != nil || got != "Alice" {
		t.Errorf("cleanNick bidi = %q, %v; want Alice", got, err)
	}
	if _, err := cleanNick("\u202e\u200b"); err == nil {
		t.Errorf("cleanNick only-Cf should fail")
	}
}
