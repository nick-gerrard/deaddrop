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

	a, b, th, top := newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h)
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
	th.post("/join", url.Values{"nickname": {"Carol"}, "code": {room}})
	top.post("/join", url.Values{"nickname": {"Dave"}, "code": {room}})

	base := "/r/" + room

	if code, _ := a.post(base+"/team", url.Values{"team": {"ember"}, "role": {"handler"}}); code != 200 {
		t.Fatalf("A team = %d", code)
	}
	if code, _ := b.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}}); code != 200 {
		t.Fatalf("B team = %d", code)
	}
	if code, _ := th.post(base+"/team", url.Values{"team": {"tide"}, "role": {"handler"}}); code != 200 {
		t.Fatalf("Carol team = %d", code)
	}
	if code, _ := top.post(base+"/team", url.Values{"team": {"tide"}, "role": {"operative"}}); code != 200 {
		t.Fatalf("Dave team = %d", code)
	}
	if code, _ := b.post(base+"/team", url.Values{"team": {"purple"}, "role": {"operative"}}); code != 400 {
		t.Errorf("bad team = %d, want 400", code)
	}

	// Start the mission (deals board and sets phase=playing).
	if code, body := a.post(base+"/start", nil); code != 200 {
		t.Fatalf("start = %d %q", code, body)
	}

	snap, err := store.Snapshot(context.Background(), room)
	if err != nil {
		t.Fatal(err)
	}
	// Pick active team's handler & operative and enemy operative.
	activeH, activeOp, enemyOp := a, b, top
	if snap.Room.Game.Turn == Tide {
		activeH, activeOp, enemyOp = th, top, b
	}

	// No clue yet: operatives cannot guess; operatives cannot clue.
	if code, _ := activeOp.post(base+"/reveal", url.Values{"card": {"0"}}); code != 409 {
		t.Errorf("guess before clue = %d, want 409", code)
	}
	if code, _ := activeOp.post(base+"/clue", url.Values{"clue": {"river"}, "number": {"2"}}); code != 403 {
		t.Errorf("operative clue = %d, want 403", code)
	}
	if code, _ := activeH.post(base+"/clue", url.Values{"clue": {"two words"}, "number": {"2"}}); code != 400 {
		t.Errorf("bad clue = %d, want 400", code)
	}
	onBoard := snap.Room.Game.Cards[0].Word
	if code, _ := activeH.post(base+"/clue", url.Values{"clue": {strings.ToUpper(onBoard)}, "number": {"2"}}); code != 400 {
		t.Errorf("board-word clue = %d, want 400", code)
	}
	if code, _ := activeH.post(base+"/clue", url.Values{"clue": {"zzzzzz"}, "number": {"0"}}); code != 400 {
		t.Errorf("zero number = %d, want 400", code)
	}
	if code, _ := activeH.post(base+"/clue", url.Values{"clue": {"zzzzzz"}, "number": {""}}); code != 400 {
		t.Errorf("empty number = %d, want 400", code)
	}
	if code, _ := activeH.post(base+"/clue", url.Values{"clue": {"zzzzzz"}, "number": {"12"}}); code != 400 {
		t.Errorf("bad number = %d, want 400", code)
	}
	if code, _ := activeH.post(base+"/clue", url.Values{"clue": {"zzzzzz"}, "number": {"2"}}); code != 200 {
		t.Fatalf("handler clue = %d", code)
	}
	if code, _ := activeH.post(base+"/clue", url.Values{"clue": {"again"}, "number": {"2"}}); code != 409 {
		t.Errorf("second clue = %d, want 409", code)
	}

	// Handler can't guess; spectator can't guess; enemy operative can't guess.
	if code, _ := activeH.post(base+"/reveal", url.Values{"card": {"0"}}); code != 403 {
		t.Errorf("handler reveal = %d, want 403", code)
	}
	spec := newClient(t, h)
	spec.post("/join", url.Values{"nickname": {"Spec"}, "code": {room}})
	if code, _ := spec.post(base+"/reveal", url.Values{"card": {"0"}}); code != 403 {
		t.Errorf("spectator reveal = %d, want 403", code)
	}
	if code, _ := enemyOp.post(base+"/reveal", url.Values{"card": {"0"}}); code != 403 {
		t.Errorf("enemy reveal = %d, want 403", code)
	}
	if code, _ := enemyOp.post(base+"/end", nil); code != 403 {
		t.Errorf("enemy end turn = %d, want 403", code)
	}
	if code, _ := activeOp.post(base+"/end", nil); code != 409 {
		t.Errorf("end turn without guess = %d, want 409", code)
	}
	if code, _ := newClient(t, h).post(base+"/reveal", url.Values{"card": {"0"}}); code != 403 {
		t.Errorf("anonymous reveal = %d, want 403", code)
	}

	// Per-viewer rendering: only handlers see unrevealed colours.
	_, ha := activeH.get(base + "/state?v=0")
	_, ob := activeOp.get(base + "/state?v=0")
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
	for name, c := range map[string]*client{"H": activeH, "Op": activeOp} {
		if code, _ := c.get(base + "/state?v=" + m[1]); code != http.StatusNoContent {
			t.Errorf("%s unchanged poll = %d, want 204", name, code)
		}
	}
	if code, _ := activeOp.post(base+"/reveal", url.Values{"card": {"0"}}); code != 200 {
		t.Fatalf("operative reveal = %d", code)
	}
	if code, _ := activeOp.post(base+"/reveal", url.Values{"card": {"0"}}); code != 409 && code != 403 {
		t.Errorf("repeat reveal = %d, want 409 (or 403 if the turn passed)", code)
	}
	for name, c := range map[string]*client{"H": activeH, "Op": activeOp} {
		code, body := c.get(base + "/state?v=" + m[1])
		if code != 200 || !strings.Contains(body, "revealed") || !strings.Contains(body, strings.ToUpper(onBoard)) {
			t.Errorf("%s did not see the reveal: %d", name, code)
		}
	}

	// Mid-round: /new is refused (409); /abort returns to setup (keeping teams).
	if code, _ := spec.post(base+"/new", nil); code != http.StatusForbidden {
		t.Errorf("spectator new round = %d, want 403", code)
	}
	if code, _ := activeOp.post(base+"/new", nil); code != http.StatusConflict {
		t.Errorf("team member mid-round /new = %d, want 409", code)
	}
	if code, _ := activeOp.post(base+"/abort", nil); code != 200 {
		t.Errorf("team member abort = %d, want 200", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.Phase != PhaseSetup || len(snap.Players) != 5 {
		t.Errorf("after abort: phase=%q players=%d", snap.Room.Phase, len(snap.Players))
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

func TestSetupPhaseAndRoleLocks(t *testing.T) {
	store, _, h := newTestApp(t)
	a, b, c, d, spec := newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h)
	_, loc := a.post("/create", url.Values{"nickname": {"Alice"}})
	room := strings.TrimPrefix(loc, "/r/")
	base := "/r/" + room
	for name, cl := range map[string]*client{"Bob": b, "Carol": c, "Dave": d, "Spec": spec} {
		if code, _ := cl.post("/join", url.Values{"nickname": {name}, "code": {room}}); code != http.StatusSeeOther {
			t.Fatalf("join %s = %d", name, code)
		}
	}

	// New room starts in setup; view contains NO board words even for handlers.
	snap, _ := store.Snapshot(context.Background(), room)
	if snap.Room.Phase != PhaseSetup {
		t.Fatalf("initial phase = %q, want %q", snap.Room.Phase, PhaseSetup)
	}
	// Max 1 handler per team in setup.
	if code, _ := a.post(base+"/team", url.Values{"team": {"ember"}, "role": {"handler"}}); code != 200 {
		t.Fatalf("1st ember handler = %d", code)
	}
	if code, _ := b.post(base+"/team", url.Values{"team": {"ember"}, "role": {"handler"}}); code != http.StatusConflict {
		t.Fatalf("2nd ember handler = %d, want 409", code)
	}
	// Setup view must not contain any board words or key classes.
	_, setupHTML := a.get(base + "/state?v=0")
	if strings.Contains(setupHTML, `class="board`) || strings.Contains(setupHTML, "k-ember") {
		t.Errorf("setup view rendered board or key")
	}
	for _, card := range snap.Room.Game.Cards {
		if strings.Contains(setupHTML, ">"+card.Word+"<") {
			t.Errorf("setup view leaked board word %q", card.Word)
		}
	}

	// Start refused without operatives/handlers.
	if code, body := a.post(base+"/start", nil); code != http.StatusConflict || !strings.Contains(body, "EMBER needs an operative") {
		t.Fatalf("start without ember op = %d %q, want 409", code, body)
	}
	b.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}})
	if code, body := a.post(base+"/start", nil); code != http.StatusConflict || !strings.Contains(body, "TIDE needs a Handler") {
		t.Fatalf("start without tide handler = %d %q, want 409", code, body)
	}
	c.post(base+"/team", url.Values{"team": {"tide"}, "role": {"handler"}})
	if code, body := a.post(base+"/start", nil); code != http.StatusConflict || !strings.Contains(body, "TIDE needs an operative") {
		t.Fatalf("start without tide op = %d %q, want 409", code, body)
	}
	d.post(base+"/team", url.Values{"team": {"tide"}, "role": {"operative"}})

	// Spectator cannot start.
	if code, _ := spec.post(base+"/start", nil); code != http.StatusForbidden {
		t.Fatalf("spectator start = %d, want 403", code)
	}

	// Valid start deals board and enters playing phase.
	if code, body := a.post(base+"/start", nil); code != 200 {
		t.Fatalf("start = %d %q", code, body)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.Phase != PhasePlaying {
		t.Fatalf("phase after start = %q, want %q", snap.Room.Phase, PhasePlaying)
	}

	// While playing: team members cannot switch roles or teams (409).
	if code, _ := b.post(base+"/team", url.Values{"team": {"ember"}, "role": {"handler"}}); code != http.StatusConflict {
		t.Errorf("operative -> handler while playing = %d, want 409", code)
	}
	if code, _ := a.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}}); code != http.StatusConflict {
		t.Errorf("handler -> operative while playing = %d, want 409", code)
	}
	if code, _ := b.post(base+"/team", url.Values{"team": {"tide"}, "role": {"operative"}}); code != http.StatusConflict {
		t.Errorf("operative switch team while playing = %d, want 409", code)
	}
	if code, _ := a.post(base+"/team", url.Values{"team": {"none"}, "role": {"operative"}}); code != http.StatusConflict {
		t.Errorf("handler -> spectate while playing = %d, want 409", code)
	}

	// While playing: spectator -> handler is 409; spectator -> operative is 200 and never sees key colours.
	if code, _ := spec.post(base+"/team", url.Values{"team": {"ember"}, "role": {"handler"}}); code != http.StatusConflict {
		t.Errorf("spectator -> handler while playing = %d, want 409", code)
	}
	code, body := spec.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}})
	if code != 200 {
		t.Errorf("spectator -> operative while playing = %d, want 200", code)
	}
	_, specState := spec.get(base + "/state?v=0")
	for _, html := range []string{body, specState} {
		if !strings.Contains(html, `class="board"`) {
			t.Errorf("mid-game operative view missing board")
		}
		for _, leak := range []string{" k-", "k-ember", "k-tide", "k-bystander", "k-burned", "keyview"} {
			if strings.Contains(html, leak) {
				t.Errorf("mid-game operative view leaked key marker %q", leak)
			}
		}
	}

	// While playing: settings (timer/pack) refused with 409, public toggle allowed.
	if code, _ := a.post(base+"/settings", url.Values{"timer": {"60"}}); code != http.StatusConflict {
		t.Errorf("timer change while playing = %d, want 409", code)
	}
	if code, _ := a.post(base+"/settings", url.Values{"pack": {"hard"}}); code != http.StatusConflict {
		t.Errorf("pack change while playing = %d, want 409", code)
	}
	if code, _ := a.post(base+"/settings", url.Values{"public": {"1"}}); code != 200 {
		t.Errorf("public toggle while playing = %d, want 200", code)
	}

	// Finish game, then Next round -> returns to setup, increments round, keeps teams & roles & series score.
	if err := store.Mutate(context.Background(), room, func(tx *sql.Tx, rm *Room) error {
		rm.Game.Winner = Ember
		rm.Game.EndReason = "test"
		rm.EmberWins = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code, _ := a.post(base+"/new", nil); code != 200 {
		t.Fatalf("next round after win = %d, want 200", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.Phase != PhaseSetup || snap.Room.Round != 2 || snap.Room.EmberWins != 1 {
		t.Fatalf("after next round: phase=%q round=%d emberWins=%d", snap.Room.Phase, snap.Room.Round, snap.Room.EmberWins)
	}
	roles := map[string]string{}
	for _, p := range snap.Players {
		roles[p.Nickname] = p.Team + ":" + p.Role
	}
	if roles["Alice"] != "ember:handler" || roles["Bob"] != "ember:operative" || roles["Carol"] != "tide:handler" || roles["Dave"] != "tide:operative" || roles["Spec"] != "ember:operative" {
		t.Errorf("teams/roles not preserved after next round: %v", roles)
	}
}

func TestNewRoundPermissions(t *testing.T) {
	store, _, h := newTestApp(t)
	a, b, c, d, spec, outsider := newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h)
	_, loc := a.post("/create", url.Values{"nickname": {"A"}})
	room := strings.TrimPrefix(loc, "/r/")
	base := "/r/" + room
	for name, cl := range map[string]*client{"B": b, "C": c, "D": d, "Spec": spec} {
		cl.post("/join", url.Values{"nickname": {name}, "code": {room}})
	}
	a.post(base+"/team", url.Values{"team": {"ember"}, "role": {"handler"}})
	b.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}})
	c.post(base+"/team", url.Values{"team": {"tide"}, "role": {"handler"}})
	d.post(base+"/team", url.Values{"team": {"tide"}, "role": {"operative"}})
	if code, _ := a.post(base+"/start", nil); code != 200 {
		t.Fatalf("start = %d", code)
	}

	// Mid-round: outsider and spectator rejected; team member /new -> 409, /abort -> 200.
	if code, _ := outsider.post(base+"/new", nil); code != http.StatusForbidden {
		t.Errorf("outsider new round = %d, want 403", code)
	}
	if code, _ := spec.post(base+"/new", nil); code != http.StatusForbidden {
		t.Errorf("spectator mid-round new = %d, want 403", code)
	}
	if code, _ := a.post(base+"/new", nil); code != http.StatusConflict {
		t.Errorf("team member mid-round new = %d, want 409", code)
	}
	if code, _ := spec.post(base+"/abort", nil); code != http.StatusForbidden {
		t.Errorf("spectator abort = %d, want 403", code)
	}
	if code, _ := a.post(base+"/abort", nil); code != 200 {
		t.Errorf("team member abort = %d, want 200", code)
	}

	// Start again and finish game: spectator still gets 403; team member /new -> 200.
	if code, _ := a.post(base+"/start", nil); code != 200 {
		t.Fatalf("restart = %d", code)
	}
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

func TestDeleteIdleRooms(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateRoom(ctx, "ABCD", newGame(), Player{ID: "p", Nickname: "n", Team: "none", Role: "operative"}, false); err != nil {
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
