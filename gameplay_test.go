package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestScoreboardIncrementsOncePerWinAndRecap(t *testing.T) {
	store, _, h := newTestApp(t)
	a, b, spec := newClient(t, h), newClient(t, h), newClient(t, h)
	_, loc := a.post("/create", url.Values{"nickname": {"Alice"}})
	room := strings.TrimPrefix(loc, "/r/")
	base := "/r/" + room
	b.post("/join", url.Values{"nickname": {"Bob"}, "code": {room}})
	spec.post("/join", url.Values{"nickname": {"Spec"}, "code": {room}})

	a.post(base+"/team", url.Values{"team": {"ember"}, "role": {"handler"}})
	b.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}})
	// Install deterministic board and enter playing phase: ember starts, idx 24 is burned.
	if err := store.Mutate(context.Background(), room, func(tx *sql.Tx, rm *Room) error {
		rm.Phase = PhasePlaying
		rm.Game = fixedGame()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.post(base+"/clue", url.Values{"clue": {"zzzzzz"}, "number": {"2"}})

	// Reveal one ember agent, then hit burned card -> Tide wins!
	if code, _ := b.post(base+"/reveal", url.Values{"card": {"0"}}); code != 200 {
		t.Fatalf("reveal 0 = %d", code)
	}
	code, body := b.post(base+"/reveal", url.Values{"card": {"24"}})
	if code != 200 {
		t.Fatalf("reveal burned = %d", code)
	}
	if !strings.Contains(body, `data-ember-wins="0"`) || !strings.Contains(body, `data-tide-wins="1"`) {
		t.Errorf("expected 0-1 series score after tide win")
	}
	// Recap should list the clue, guessed words, and unrevealed agents.
	if !strings.Contains(body, "ZZZZZZ 2") || !strings.Contains(body, "WORDA") || !strings.Contains(body, "Unrevealed:") {
		t.Errorf("winner overlay missing round recap details")
	}

	// Repeated /state fetches and extra reveal attempts must NOT increment score again.
	for range 5 {
		a.get(base + "/state?v=0")
		b.get(base + "/state?v=0")
		b.post(base+"/reveal", url.Values{"card": {"1"}})
	}
	snap, _ := store.Snapshot(context.Background(), room)
	if snap.Room.EmberWins != 0 || snap.Room.TideWins != 1 {
		t.Fatalf("wins mutated on repeated fetches: ember=%d tide=%d", snap.Room.EmberWins, snap.Room.TideWins)
	}

	// Spectator cannot reset series; team member can.
	if code, _ := spec.post(base+"/series-reset", nil); code != http.StatusForbidden {
		t.Errorf("spectator series reset = %d, want 403", code)
	}
	if code, _ := a.post(base+"/series-reset", nil); code != 200 {
		t.Fatalf("team member series reset = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.EmberWins != 0 || snap.Room.TideWins != 0 {
		t.Fatalf("series not reset: ember=%d tide=%d", snap.Room.EmberWins, snap.Room.TideWins)
	}
}

func TestMarksPermissionsAndLifecycle(t *testing.T) {
	store, _, h := newTestApp(t)
	handler, op1, op2, enemy, enemyHandler, spec := newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h)
	_, loc := handler.post("/create", url.Values{"nickname": {"Hannah"}})
	room := strings.TrimPrefix(loc, "/r/")
	base := "/r/" + room
	op1.post("/join", url.Values{"nickname": {"Jane Doe"}, "code": {room}})
	op2.post("/join", url.Values{"nickname": {"Bob"}, "code": {room}})
	enemy.post("/join", url.Values{"nickname": {"Eve"}, "code": {room}})
	enemyHandler.post("/join", url.Values{"nickname": {"EnemyH"}, "code": {room}})
	spec.post("/join", url.Values{"nickname": {"Spec"}, "code": {room}})

	handler.post(base+"/team", url.Values{"team": {"ember"}, "role": {"handler"}})
	op1.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}})
	op2.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}})
	enemy.post(base+"/team", url.Values{"team": {"tide"}, "role": {"operative"}})
	enemyHandler.post(base+"/team", url.Values{"team": {"tide"}, "role": {"handler"}})

	if err := store.Mutate(context.Background(), room, func(tx *sql.Tx, rm *Room) error {
		rm.Phase = PhasePlaying
		rm.Game = fixedGame()
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Before clue: operative cannot mark.
	if code, _ := op1.post(base+"/mark", url.Values{"card": {"0"}}); code != http.StatusConflict {
		t.Errorf("mark before clue = %d, want 409", code)
	}

	handler.post(base+"/clue", url.Values{"clue": {"zzzzzz"}, "number": {"2"}})

	// Handler, spectator, enemy operative cannot mark.
	if code, _ := handler.post(base+"/mark", url.Values{"card": {"0"}}); code != http.StatusForbidden {
		t.Errorf("handler mark = %d, want 403", code)
	}
	if code, _ := spec.post(base+"/mark", url.Values{"card": {"0"}}); code != http.StatusForbidden {
		t.Errorf("spectator mark = %d, want 403", code)
	}
	if code, _ := enemy.post(base+"/mark", url.Values{"card": {"0"}}); code != http.StatusForbidden {
		t.Errorf("enemy mark = %d, want 403", code)
	}

	// Guessing team operatives can mark & toggle off.
	code, body := op1.post(base+"/mark", url.Values{"card": {"0"}})
	if code != 200 || !strings.Contains(body, ">JD<") {
		t.Fatalf("op1 mark = %d (expected JD chip)", code)
	}
	code, body = op2.post(base+"/mark", url.Values{"card": {"0"}})
	if code != 200 || !strings.Contains(body, ">BO<") {
		t.Fatalf("op2 mark = %d (expected BO chip)", code)
	}
	// Marks are visible to operatives and handlers on the marking team,
	// and hidden from the other team and spectators.
	_, hBody := handler.get(base + "/state?v=0")
	if !strings.Contains(hBody, ">JD<") || !strings.Contains(hBody, ">BO<") {
		t.Errorf("marking team handler did not see operative mark chips")
	}
	_, eBody := enemy.get(base + "/state?v=0")
	if strings.Contains(eBody, ">JD<") || strings.Contains(eBody, ">BO<") {
		t.Errorf("other team operative should not see marking team's marks")
	}
	_, sBody := spec.get(base + "/state?v=0")
	if strings.Contains(sBody, ">JD<") || strings.Contains(sBody, ">BO<") {
		t.Errorf("spectator should not see marking team's marks")
	}
	_, ehBody := enemyHandler.get(base + "/state?v=0")
	if strings.Contains(ehBody, ">JD<") || strings.Contains(ehBody, ">BO<") {
		t.Errorf("other team handler should not see marking team's marks")
	}

	// Toggle op2's mark off on card 0, and put a mark on card 1.
	op2.post(base+"/mark", url.Values{"card": {"0"}})
	op2.post(base+"/mark", url.Values{"card": {"1"}})
	snap, _ := store.Snapshot(context.Background(), room)
	if len(snap.Marks) != 2 {
		t.Fatalf("expected 2 marks (op1 on 0, op2 on 1), got %v", snap.Marks)
	}

	// Revealing card 0 clears marks on card 0 and disallows marking revealed cards.
	if code, _ := op1.post(base+"/reveal", url.Values{"card": {"0"}}); code != 200 {
		t.Fatalf("reveal 0 = %d", code)
	}
	if code, _ := op1.post(base+"/mark", url.Values{"card": {"0"}}); code != http.StatusConflict {
		t.Errorf("mark revealed card = %d, want 409", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if len(snap.Marks) != 1 || snap.Marks[0].Card != 1 {
		t.Fatalf("expected only card 1 mark remaining, got %v", snap.Marks)
	}

	// Ending the turn clears all remaining marks.
	if code, _ := op1.post(base+"/end", nil); code != 200 {
		t.Fatalf("end turn = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if len(snap.Marks) != 0 {
		t.Fatalf("marks not cleared on end turn: %v", snap.Marks)
	}
}
