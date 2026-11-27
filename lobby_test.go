package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

var lobbyVersionRe = regexp.MustCompile(`hx-get="/lobby\?v=([0-9a-f]+)"`)

func TestListPublicRoomsDB(t *testing.T) {
	store, _, _ := newTestApp(t)
	ctx := context.Background()
	base := int64(1_700_000_000)

	// Create one private room, one stale public room, and two active public rooms.
	if err := store.CreateRoom(ctx, "PRIV", newGame(), Player{ID: "p1", Nickname: "Secret", Team: "ember", Role: "handler"}, false); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRoom(ctx, "OLD1", newGame(), Player{ID: "p2", Nickname: "Old", Team: "ember", Role: "handler"}, true); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRoom(ctx, "PUBA", newGame(), Player{ID: "p3", Nickname: "A1", Team: "ember", Role: "handler"}, true); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRoom(ctx, "PUBB", newGame(), Player{ID: "p4", Nickname: "B1", Team: "tide", Role: "operative"}, true); err != nil {
		t.Fatal(err)
	}

	// Add extra players (including a spectator) to PUBA and mark PUBB finished.
	if err := store.Mutate(ctx, "PUBA", func(tx *sql.Tx, rm *Room) error {
		if err := upsertPlayer(ctx, tx, "PUBA", Player{ID: "p5", Nickname: "A2", Team: "ember", Role: "operative"}); err != nil {
			return err
		}
		if err := upsertPlayer(ctx, tx, "PUBA", Player{ID: "p6", Nickname: "A3", Team: "tide", Role: "handler"}); err != nil {
			return err
		}
		return upsertPlayer(ctx, tx, "PUBA", Player{ID: "p7", Nickname: "Spec", Team: "none", Role: "operative"})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Mutate(ctx, "PUBB", func(tx *sql.Tx, rm *Room) error {
		rm.Game.Winner = Ember
		rm.Game.EndReason = "test win"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Set deterministic updated_at timestamps.
	if _, err := store.w.Exec(`UPDATE rooms SET updated_at=? WHERE code='PRIV'`, base); err != nil {
		t.Fatal(err)
	}
	if _, err := store.w.Exec(`UPDATE rooms SET updated_at=? WHERE code='OLD1'`, base-31*60); err != nil {
		t.Fatal(err)
	}
	if _, err := store.w.Exec(`UPDATE rooms SET updated_at=? WHERE code='PUBA'`, base-120); err != nil {
		t.Fatal(err)
	}
	if _, err := store.w.Exec(`UPDATE rooms SET updated_at=? WHERE code='PUBB'`, base-60); err != nil {
		t.Fatal(err)
	}

	got, err := store.ListPublicRooms(ctx, base-30*60, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (%+v)", len(got), got)
	}
	// Ordered by updated_at DESC: PUBB (-60s) before PUBA (-120s).
	if got[0].Code != "PUBB" || got[0].EmberCount != 0 || got[0].TideCount != 1 || !got[0].Game.Over() {
		t.Errorf("got[0] = %+v, want PUBB ember=0 tide=1 over=true", got[0])
	}
	if got[1].Code != "PUBA" || got[1].EmberCount != 2 || got[1].TideCount != 1 {
		t.Errorf("got[1] = %+v, want PUBA ember=2 tide=1", got[1])
	}

	// Limit parameter respected.
	limited, err := store.ListPublicRooms(ctx, base-30*60, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].Code != "PUBB" {
		t.Errorf("limit=1 got %+v, want [PUBB]", limited)
	}
}

func TestLobbyEndpointAndToggle(t *testing.T) {
	store, app, h := newTestApp(t)
	nowT := time.Now()
	app.now = func() time.Time { return nowT }

	// Home page has checkbox and #lobby-list polling /lobby every 5s.
	guest := newClient(t, h)
	code, homeBody := guest.get("/")
	if code != 200 {
		t.Fatalf("GET / = %d", code)
	}
	for _, want := range []string{
		`id="lobby-list"`,
		`hx-get="/lobby"`,
		`hx-trigger="load, every 5s"`,
		`List in public lobby`,
		`Open rooms`,
	} {
		if !strings.Contains(homeBody, want) {
			t.Errorf("home page missing %q", want)
		}
	}

	// Empty lobby state + Cache-Control: no-store.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lobby", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /lobby = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if !strings.Contains(rec.Body.String(), "No public rooms right now. Create one and tick List in public lobby.") {
		t.Errorf("unexpected empty lobby body: %q", rec.Body.String())
	}

	// Create one public room and one private room.
	pubClient, privClient, specClient := newClient(t, h), newClient(t, h), newClient(t, h)
	_, pubLoc := pubClient.post("/create", url.Values{"nickname": {"SecretAlice"}, "public": {"1"}})
	pubRoom := strings.TrimPrefix(pubLoc, "/r/")
	_, privLoc := privClient.post("/create", url.Values{"nickname": {"SecretBob"}})
	privRoom := strings.TrimPrefix(privLoc, "/r/")

	// Check room headers show PUBLIC / PRIVATE tag.
	_, pubPage := pubClient.get(pubLoc)
	if !strings.Contains(pubPage, `id="room-vis">PUBLIC<`) {
		t.Errorf("public room missing PUBLIC header tag")
	}
	_, privPage := privClient.get(privLoc)
	if !strings.Contains(privPage, `id="room-vis">PRIVATE<`) {
		t.Errorf("private room missing PRIVATE header tag")
	}

	// Put pubClient on ember, specClient as spectator.
	snap, _ := store.Snapshot(context.Background(), pubRoom)
	firstTeam := string(snap.Room.Game.Turn)
	pubClient.post(pubLoc+"/team", url.Values{"team": {firstTeam}, "role": {"handler"}})
	specClient.post("/join", url.Values{"nickname": {"SpecNick"}, "code": {pubRoom}})

	// GET /lobby lists only pubRoom, with team counts, join link, status, and NO nicknames.
	code, lobbyBody := guest.get("/lobby")
	if code != 200 {
		t.Fatalf("GET /lobby = %d", code)
	}
	if !strings.Contains(lobbyBody, pubRoom) || !strings.Contains(lobbyBody, `href="/r/`+pubRoom+`"`) {
		t.Errorf("lobby missing public room %s: %s", pubRoom, lobbyBody)
	}
	if strings.Contains(lobbyBody, privRoom) {
		t.Errorf("lobby leaked private room %s", privRoom)
	}
	for _, nick := range []string{"SecretAlice", "SecretBob", "SpecNick"} {
		if strings.Contains(lobbyBody, nick) {
			t.Errorf("lobby leaked nickname %q", nick)
		}
	}
	wantCount := fmt.Sprintf("%s 1", strings.ToUpper(firstTeam))
	if !strings.Contains(lobbyBody, wantCount) {
		t.Errorf("lobby missing team count %q: %s", wantCount, lobbyBody)
	}
	if !strings.Contains(lobbyBody, "Waiting for players") {
		t.Errorf("lobby missing Waiting for players status: %s", lobbyBody)
	}

	// Give a clue -> status becomes "In progress round 1".
	if code, _ := pubClient.post(pubLoc+"/clue", url.Values{"clue": {"zzzzzz"}, "number": {"1"}}); code != 200 {
		t.Fatalf("clue = %d", code)
	}
	_, lobbyBody = guest.get("/lobby")
	if !strings.Contains(lobbyBody, "In progress round 1") {
		t.Errorf("lobby missing In progress round 1: %s", lobbyBody)
	}

	// Spectator cannot toggle visibility.
	if code, _ := specClient.post(pubLoc+"/settings", url.Values{"public": {"0"}}); code != http.StatusForbidden {
		t.Errorf("spectator toggle public = %d, want 403", code)
	}

	// Team member toggles room to private -> version bumps, removed from /lobby.
	snapBefore, _ := store.Snapshot(context.Background(), pubRoom)
	code, frag := pubClient.post(pubLoc+"/settings", url.Values{"public": {"0"}})
	if code != 200 {
		t.Fatalf("toggle private = %d", code)
	}
	if !strings.Contains(frag, `data-public="0"`) {
		t.Errorf("fragment missing data-public=0 after toggle")
	}
	snapAfter, _ := store.Snapshot(context.Background(), pubRoom)
	if snapAfter.Room.Public || snapAfter.Room.Version <= snapBefore.Room.Version {
		t.Errorf("after toggle private: public=%v ver=%d (was %d)", snapAfter.Room.Public, snapAfter.Room.Version, snapBefore.Room.Version)
	}
	_, lobbyBody = guest.get("/lobby")
	if strings.Contains(lobbyBody, pubRoom) {
		t.Errorf("toggled-private room still in lobby: %s", lobbyBody)
	}

	// Toggle back to public using checkbox+hidden form values (["0", "1"]).
	code, frag = pubClient.post(pubLoc+"/settings", url.Values{"public": {"0", "1"}})
	if code != 200 || !strings.Contains(frag, `data-public="1"`) {
		t.Fatalf("toggle back to public = %d", code)
	}
	_, lobbyBody = guest.get("/lobby")
	if !strings.Contains(lobbyBody, pubRoom) {
		t.Errorf("re-publicized room missing from lobby: %s", lobbyBody)
	}
}

func TestLobbyFingerprintAnd204(t *testing.T) {
	_, app, h := newTestApp(t)
	nowT := time.Now()
	app.now = func() time.Time { return nowT }

	guest := newClient(t, h)
	code, body := guest.get("/lobby")
	if code != http.StatusOK {
		t.Fatalf("initial GET /lobby = %d, want 200", code)
	}
	if !strings.Contains(body, `id="lobby-list"`) || !strings.Contains(body, `hx-swap="outerHTML"`) {
		t.Fatalf("lobby fragment missing #lobby-list outerHTML wrapper: %s", body)
	}
	m := lobbyVersionRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("lobby fragment missing hx-get=/lobby?v=...: %s", body)
	}
	vEmpty := m[1]

	// Same v -> 204 No Content.
	if code, b := guest.get("/lobby?v=" + vEmpty); code != http.StatusNoContent || b != "" {
		t.Fatalf("same v on empty lobby = %d %q, want 204", code, b)
	}
	// Different v -> 200 OK.
	if code, _ := guest.get("/lobby?v=deadbeef0000"); code != http.StatusOK {
		t.Fatalf("different v = %d, want 200", code)
	}

	// Create a public room -> old v now returns 200 with a new v.
	pub := newClient(t, h)
	pub.post("/create", url.Values{"nickname": {"Alice"}, "public": {"1"}})
	code, body = guest.get("/lobby?v=" + vEmpty)
	if code != http.StatusOK {
		t.Fatalf("after create with old v = %d, want 200", code)
	}
	m = lobbyVersionRe.FindStringSubmatch(body)
	if m == nil || m[1] == vEmpty {
		t.Fatalf("expected new v after create, got %v", m)
	}
	vWithRoom := m[1]

	// Same v -> 204.
	if code, _ := guest.get("/lobby?v=" + vWithRoom); code != http.StatusNoContent {
		t.Fatalf("same v with room = %d, want 204", code)
	}

	// Advance clock so rendered "ago" string changes -> 200 with new v.
	nowT = nowT.Add(2 * time.Minute)
	code, body = guest.get("/lobby?v=" + vWithRoom)
	if code != http.StatusOK {
		t.Fatalf("after time-ago change = %d, want 200", code)
	}
	m = lobbyVersionRe.FindStringSubmatch(body)
	if m == nil || m[1] == vWithRoom {
		t.Fatalf("expected new v after time-ago change, got %v", m)
	}
	if code, _ := guest.get("/lobby?v=" + m[1]); code != http.StatusNoContent {
		t.Fatalf("same v after time-ago change = %d, want 204", code)
	}
}
