package main

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
	resp, err := c.hc.PostForm(c.base+path, form)
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

	// Anyone in the room can deal a new round; board resets.
	if code, _ := spec.post(base+"/new", nil); code != 200 {
		t.Errorf("new round = %d", code)
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
