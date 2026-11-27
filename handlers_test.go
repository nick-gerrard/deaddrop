package main

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type client struct {
	t    *testing.T
	base string
	hc   *http.Client
}

// handlerTransport serves requests straight from the handler (no sockets).
type handlerTransport struct{ h http.Handler }

func (ht handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	ht.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func newClient(t *testing.T, h http.Handler) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: "http://deaddrop.test", hc: &http.Client{
		Transport:     handlerTransport{h},
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *client) post(path string, form url.Values) (int, string) {
	c.t.Helper()
	return c.postHeaders(path, form, map[string]string{"Origin": c.base})
}

func (c *client) postHeaders(path string, form url.Values, hdrs map[string]string) (int, string) {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if loc := resp.Header.Get("Location"); loc != "" {
		return resp.StatusCode, loc
	}
	return resp.StatusCode, string(b)
}

func (c *client) get(path string) (int, string) {
	c.t.Helper()
	resp, err := c.hc.Get(c.base + path)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

var versionRe = regexp.MustCompile(`state\?v=(\d+)`)

func TestServerEnforcesRules(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app, err := NewApp(store)
	if err != nil {
		t.Fatal(err)
	}
	h := app.Routes()

	if code, body := newClient(t, h).get("/healthz"); code != 200 || body != "ok" {
		t.Fatalf("healthz = %d %q", code, body)
	}

	a, b := newClient(t, h), newClient(t, h)
	code, loc := a.post("/create", url.Values{"nickname": {"Alice"}})
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/r/") {
		t.Fatalf("create = %d %q", code, loc)
	}
	room := strings.TrimPrefix(loc, "/r/")
	if len(room) != 4 {
		t.Fatalf("room code %q", room)
	}
	if code, _ := b.post("/join", url.Values{"nickname": {"Bob"}, "code": {strings.ToLower(room)}}); code != http.StatusSeeOther {
		t.Fatalf("join = %d", code)
	}
	if code, _ := b.post("/join", url.Values{"nickname": {"Bob"}, "code": {"ZZZZ"}}); code != http.StatusNotFound {
		t.Fatalf("join missing room = %d", code)
	}

	snap, err := store.Snapshot(context.Background(), room)
	if err != nil {
		t.Fatal(err)
	}
	team := string(snap.Room.Game.Turn)
	other := string(snap.Room.Game.Turn.Other())
	base := "/r/" + room

	if code, _ := a.post(base+"/team", url.Values{"team": {team}, "role": {"handler"}}); code != 200 {
		t.Fatalf("A team = %d", code)
	}
	if code, _ := b.post(base+"/team", url.Values{"team": {team}, "role": {"operative"}}); code != 200 {
		t.Fatalf("B team = %d", code)
	}
	if code, _ := b.post(base+"/team", url.Values{"team": {"purple"}, "role": {"operative"}}); code != 400 {
		t.Errorf("bad team = %d, want 400", code)
	}

	// No clue yet: operatives cannot guess; operatives cannot clue.
	if code, _ := b.post(base+"/reveal", url.Values{"card": {"0"}}); code != 409 {
		t.Errorf("guess before clue = %d, want 409", code)
	}
	if code, _ := b.post(base+"/clue", url.Values{"clue": {"river"}, "number": {"2"}}); code != 403 {
		t.Errorf("operative clue = %d, want 403", code)
	}
	if code, _ := a.post(base+"/clue", url.Values{"clue": {"two words"}, "number": {"2"}}); code != 400 {
		t.Errorf("bad clue = %d, want 400", code)
	}
	onBoard := snap.Room.Game.Cards[0].Word
	if code, _ := a.post(base+"/clue", url.Values{"clue": {strings.ToUpper(onBoard)}, "number": {"2"}}); code != 400 {
		t.Errorf("board-word clue = %d, want 400", code)
	}
	if code, _ := a.post(base+"/clue", url.Values{"clue": {"zzzzzz"}, "number": {"12"}}); code != 400 {
		t.Errorf("bad number = %d, want 400", code)
	}
	if code, _ := a.post(base+"/clue", url.Values{"clue": {"zzzzzz"}, "number": {"2"}}); code != 200 {
		t.Fatalf("handler clue = %d", code)
	}
	if code, _ := a.post(base+"/clue", url.Values{"clue": {"again"}, "number": {"2"}}); code != 409 {
		t.Errorf("second clue = %d, want 409", code)
	}

	// Handler can't guess; spectator can't guess; enemy operative can't guess.
	if code, _ := a.post(base+"/reveal", url.Values{"card": {"0"}}); code != 403 {
		t.Errorf("handler reveal = %d, want 403", code)
	}
	spec := newClient(t, h)
	spec.post("/join", url.Values{"nickname": {"Spec"}, "code": {room}})
	if code, _ := spec.post(base+"/reveal", url.Values{"card": {"0"}}); code != 403 {
		t.Errorf("spectator reveal = %d, want 403", code)
	}
	enemy := newClient(t, h)
	enemy.post("/join", url.Values{"nickname": {"Eve"}, "code": {room}})
	enemy.post(base+"/team", url.Values{"team": {other}, "role": {"operative"}})
	if code, _ := enemy.post(base+"/reveal", url.Values{"card": {"0"}}); code != 403 {
		t.Errorf("enemy reveal = %d, want 403", code)
	}
	if code, _ := enemy.post(base+"/end", nil); code != 403 {
		t.Errorf("enemy end turn = %d, want 403", code)
	}
	if code, _ := b.post(base+"/end", nil); code != 409 {
		t.Errorf("end turn without guess = %d, want 409", code)
	}
	if code, _ := newClient(t, h).post(base+"/reveal", url.Values{"card": {"0"}}); code != 403 {
		t.Errorf("anonymous reveal = %d, want 403", code)
	}

	// Per-viewer rendering: only handlers see unrevealed colours.
	_, ha := a.get(base + "/state?v=0")
	_, ob := b.get(base + "/state?v=0")
	if !strings.Contains(ha, " k-") {
		t.Error("handler view is missing the key")
	}
	if strings.Contains(ob, " k-") || strings.Contains(ob, "k-ember") || strings.Contains(ob, "k-tide") {
		t.Error("operative view leaks the key")
	}

	// Polling: unchanged version -> 204; reveal bumps it for everyone.
	m := versionRe.FindStringSubmatch(ob)
	if m == nil {
		t.Fatal("no version in fragment")
	}
	for name, c := range map[string]*client{"A": a, "B": b} {
		if code, _ := c.get(base + "/state?v=" + m[1]); code != http.StatusNoContent {
			t.Errorf("%s unchanged poll = %d, want 204", name, code)
		}
	}
	if code, _ := b.post(base+"/reveal", url.Values{"card": {"0"}}); code != 200 {
		t.Fatalf("operative reveal = %d", code)
	}
	if code, _ := b.post(base+"/reveal", url.Values{"card": {"0"}}); code != 409 && code != 403 {
		t.Errorf("repeat reveal = %d, want 409 (or 403 if the turn passed)", code)
	}
	for name, c := range map[string]*client{"A": a, "B": b} {
		code, body := c.get(base + "/state?v=" + m[1])
		if code != 200 || !strings.Contains(body, "revealed") || !strings.Contains(body, strings.ToUpper(onBoard)) {
			t.Errorf("%s did not see the reveal: %d", name, code)
		}
	}

	// Spectators cannot deal a new round; team members can; board resets.
	if code, _ := spec.post(base+"/new", nil); code != http.StatusForbidden {
		t.Errorf("spectator new round = %d, want 403", code)
	}
	if code, _ := b.post(base+"/new", nil); code != 200 {
		t.Errorf("team member new round = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.Round != 2 || snap.Room.Game.HasClue || len(snap.Players) != 4 {
		t.Errorf("after new round: round=%d hasClue=%v players=%d", snap.Room.Round, snap.Room.Game.HasClue, len(snap.Players))
	}
	for _, c := range snap.Room.Game.Cards {
		if c.Revealed {
			t.Error("card still revealed after new round")
		}
	}
}

func newTestApp(t *testing.T) (*Store, *App, http.Handler) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	app, err := NewApp(store)
	if err != nil {
		t.Fatal(err)
	}
	return store, app, app.Routes()
}

func TestHandlerRoleRules(t *testing.T) {
	store, _, h := newTestApp(t)
	a, b, c, d := newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h)
	_, loc := a.post("/create", url.Values{"nickname": {"A"}})
	room := strings.TrimPrefix(loc, "/r/")
	base := "/r/" + room
	for name, cl := range map[string]*client{"B": b, "C": c, "D": d} {
		if code, _ := cl.post("/join", url.Values{"nickname": {name}, "code": {room}}); code != http.StatusSeeOther {
			t.Fatalf("join %s = %d", name, code)
		}
	}
	snap, _ := store.Snapshot(context.Background(), room)
	team := string(snap.Room.Game.Turn)
	other := string(snap.Room.Game.Turn.Other())

	// Max 2 handlers per team even before round starts.
	if code, _ := a.post(base+"/team", url.Values{"team": {team}, "role": {"handler"}}); code != 200 {
		t.Fatalf("1st handler = %d", code)
	}
	if code, _ := b.post(base+"/team", url.Values{"team": {team}, "role": {"handler"}}); code != 200 {
		t.Fatalf("2nd handler = %d", code)
	}
	if code, _ := c.post(base+"/team", url.Values{"team": {team}, "role": {"handler"}}); code != http.StatusConflict {
		t.Fatalf("3rd handler = %d, want 409", code)
	}
	// B switches away from handler -> allowed.
	if code, _ := b.post(base+"/team", url.Values{"team": {team}, "role": {"operative"}}); code != 200 {
		t.Fatalf("switch away from handler = %d", code)
	}

	// Start round by giving a clue.
	if code, _ := a.post(base+"/clue", url.Values{"clue": {"zzzzzz"}, "number": {"1"}}); code != 200 {
		t.Fatalf("clue = %d", code)
	}

	// Mid-round: team already has handler A, so B cannot become handler (409 + toast message).
	code, body := b.post(base+"/team", url.Values{"team": {team}, "role": {"handler"}})
	if code != http.StatusConflict || !strings.Contains(body, "Can't become handler mid-round") {
		t.Fatalf("mid-round handler = %d %q, want 409 with message", code, body)
	}

	// Mid-round: other team has NO handler, so C CAN become handler on other team.
	if code, _ := c.post(base+"/team", url.Values{"team": {other}, "role": {"handler"}}); code != 200 {
		t.Fatalf("mid-round handler when team has none = %d, want 200", code)
	}
	// Now other team has 1 handler, so D cannot become handler mid-round.
	if code, _ := d.post(base+"/team", url.Values{"team": {other}, "role": {"handler"}}); code != http.StatusConflict {
		t.Fatalf("2nd mid-round handler on other = %d, want 409", code)
	}

	// Switching away from handler mid-round is always allowed.
	if code, _ := a.post(base+"/team", url.Values{"team": {"none"}, "role": {"operative"}}); code != 200 {
		t.Fatalf("handler -> spectate mid-round = %d, want 200", code)
	}
	// Now team has no handler again, so B can step in.
	if code, _ := b.post(base+"/team", url.Values{"team": {team}, "role": {"handler"}}); code != 200 {
		t.Fatalf("replacement handler mid-round = %d, want 200", code)
	}
}

func TestNewRoundPermissions(t *testing.T) {
	store, _, h := newTestApp(t)
	a, spec, outsider := newClient(t, h), newClient(t, h), newClient(t, h)
	_, loc := a.post("/create", url.Values{"nickname": {"A"}})
	room := strings.TrimPrefix(loc, "/r/")
	base := "/r/" + room
	spec.post("/join", url.Values{"nickname": {"Spec"}, "code": {room}})

	// Mid-round: outsider and spectator rejected; team member allowed.
	if code, _ := outsider.post(base+"/new", nil); code != http.StatusForbidden {
		t.Errorf("outsider new round = %d, want 403", code)
	}
	if code, _ := spec.post(base+"/new", nil); code != http.StatusForbidden {
		t.Errorf("spectator mid-round new = %d, want 403", code)
	}
	a.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}})
	if code, _ := a.post(base+"/new", nil); code != 200 {
		t.Errorf("team member mid-round new = %d, want 200", code)
	}

	// Game over: spectator still gets 403; team member allowed.
	err := store.Mutate(context.Background(), room, func(tx *sql.Tx, rm *Room) error {
		rm.Game.Winner = Ember
		rm.Game.EndReason = "test"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := spec.post(base+"/new", nil); code != http.StatusForbidden {
		t.Errorf("spectator game-over new = %d, want 403", code)
	}
	if code, _ := a.post(base+"/new", nil); code != 200 {
		t.Errorf("team member game-over new = %d, want 200", code)
	}
}

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
	if err := store.CreateRoom(ctx, "ABCD", newGame(), Player{ID: "p1", Nickname: "Alice", Team: "none", Role: "operative"}); err != nil {
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

func TestDeleteIdleRooms(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateRoom(ctx, "ABCD", newGame(), Player{ID: "p", Nickname: "n", Team: "none", Role: "operative"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.DeleteIdleRooms(ctx, 48*3600e9); n != 0 {
		t.Fatalf("fresh room deleted")
	}
	store.w.Exec(`UPDATE rooms SET updated_at = updated_at - 49*3600`)
	if n, err := store.DeleteIdleRooms(ctx, 48*3600e9); err != nil || n != 1 {
		t.Fatalf("idle room not deleted: %d %v", n, err)
	}
	if _, err := store.Snapshot(ctx, "ABCD"); err != ErrRoomNotFound {
		t.Fatalf("room still present: %v", err)
	}
	var n int
	store.r.QueryRow(`SELECT COUNT(*) FROM players`).Scan(&n)
	if n != 0 {
		t.Fatalf("players not cleaned: %d", n)
	}
}
