package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
)

func TestHardWordList(t *testing.T) {
	seen := map[string]bool{}
	for _, w := range hardWordList {
		if w == "" || w != strings.ToLower(w) {
			t.Errorf("bad hard word %q", w)
		}
		for _, r := range w {
			if !unicode.IsLetter(r) {
				t.Errorf("non-letter in hard word %q", w)
			}
		}
		if seen[w] {
			t.Errorf("duplicate hard word %q", w)
		}
		seen[w] = true
	}
	if len(hardWordList) < 240 {
		t.Errorf("hard word list too short: %d", len(hardWordList))
	}
}

func TestMigrateLegacyDB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	// Create pre-round2 rooms table without the new columns.
	_, err = db.Exec(`CREATE TABLE rooms (
		code       TEXT PRIMARY KEY,
		version    INTEGER NOT NULL DEFAULT 1,
		round      INTEGER NOT NULL DEFAULT 1,
		game       TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// OpenStore twice to verify idempotency.
	for i := range 2 {
		s, err := OpenStore(p)
		if err != nil {
			t.Fatalf("OpenStore pass %d: %v", i+1, err)
		}
		s.Close()
	}
}

func TestMigrateBackfillsPhaseForDealtGames(t *testing.T) {
	p := filepath.Join(t.TempDir(), "prephase.db")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE rooms (
		code       TEXT PRIMARY KEY,
		version    INTEGER NOT NULL DEFAULT 1,
		round      INTEGER NOT NULL DEFAULT 1,
		game       TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`)
	if err != nil {
		t.Fatal(err)
	}
	dealt := fixedGame()
	dealtJSON, err := json.Marshal(dealt)
	if err != nil {
		t.Fatal(err)
	}
	emptyJSON, err := json.Marshal(&Game{})
	if err != nil {
		t.Fatal(err)
	}
	ts := now()
	if _, err := db.Exec(`INSERT INTO rooms(code, game, created_at, updated_at) VALUES ('LIVE', ?, ?, ?), ('EMPT', ?, ?, ?)`,
		string(dealtJSON), ts, ts, string(emptyJSON), ts, ts); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := OpenStore(p)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	liveSnap, err := s.Snapshot(context.Background(), "LIVE")
	if err != nil {
		t.Fatalf("snapshot LIVE: %v", err)
	}
	if liveSnap.Room.Phase != PhasePlaying {
		t.Fatalf("LIVE phase = %q, want %q", liveSnap.Room.Phase, PhasePlaying)
	}
	if len(liveSnap.Room.Game.Cards) != len(dealt.Cards) {
		t.Fatalf("LIVE cards len = %d, want %d", len(liveSnap.Room.Game.Cards), len(dealt.Cards))
	}
	for i, c := range dealt.Cards {
		if liveSnap.Room.Game.Cards[i] != c {
			t.Fatalf("LIVE card %d = %+v, want %+v", i, liveSnap.Room.Game.Cards[i], c)
		}
	}

	emptySnap, err := s.Snapshot(context.Background(), "EMPT")
	if err != nil {
		t.Fatalf("snapshot EMPT: %v", err)
	}
	if emptySnap.Room.Phase != PhaseSetup {
		t.Fatalf("EMPT phase = %q, want %q", emptySnap.Room.Phase, PhaseSetup)
	}
}

func TestTimerExpiryWithInjectedClock(t *testing.T) {
	store, app, h := newTestApp(t)
	nowT := time.Unix(1_700_000_000, 0)
	app.now = func() time.Time { return nowT }

	a, b, c, d := newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h)
	_, loc := a.post("/create", url.Values{"nickname": {"Alice"}})
	room := strings.TrimPrefix(loc, "/r/")
	base := "/r/" + room
	b.post("/join", url.Values{"nickname": {"Bob"}, "code": {room}})
	c.post("/join", url.Values{"nickname": {"Carol"}, "code": {room}})
	d.post("/join", url.Values{"nickname": {"Dave"}, "code": {room}})

	a.post(base+"/team", url.Values{"team": {"ember"}, "role": {"handler"}})
	b.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}})
	c.post(base+"/team", url.Values{"team": {"tide"}, "role": {"handler"}})
	d.post(base+"/team", url.Values{"team": {"tide"}, "role": {"operative"}})

	// Enable 60s turn timer in setup (deadline stays 0 until start).
	if code, _ := a.post(base+"/settings", url.Values{"timer": {"60"}}); code != 200 {
		t.Fatalf("enable timer = %d", code)
	}
	snap, _ := store.Snapshot(context.Background(), room)
	if snap.Room.Deadline != 0 {
		t.Fatalf("deadline in setup = %d, want 0", snap.Room.Deadline)
	}

	// Start mission: deadline starts now.
	if code, body := a.post(base+"/start", nil); code != 200 || !strings.Contains(body, `data-deadline="`) {
		t.Fatalf("start = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	firstTeam := snap.Room.Game.Turn
	activeH, activeOp := a, b
	if firstTeam == Tide {
		activeH, activeOp = c, d
	}
	dl1 := snap.Room.Deadline
	if dl1 != nowT.UnixMilli()+60_000 {
		t.Fatalf("deadline = %d, want %d", dl1, nowT.UnixMilli()+60_000)
	}

	// Advance 30s and give a clue: deadline must restart for the guess phase.
	nowT = nowT.Add(30 * time.Second)
	if code, _ := activeH.post(base+"/clue", url.Values{"clue": {"zzzzzz"}, "number": {"2"}}); code != 200 {
		t.Fatalf("clue = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	dl2 := snap.Room.Deadline
	if dl2 != nowT.UnixMilli()+60_000 {
		t.Fatalf("deadline after clue = %d, want %d", dl2, nowT.UnixMilli()+60_000)
	}

	// Place a mark before timeout to ensure timeout clears marks too.
	if code, _ := activeOp.post(base+"/mark", url.Values{"card": {"0"}}); code != 200 {
		t.Fatalf("mark = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	verBefore := snap.Room.Version

	// Advance past deadline and fetch /state: turn must expire, log "time ran out", bump version, clear marks.
	nowT = nowT.Add(61 * time.Second)
	code, body := a.get(fmt.Sprintf("%s/state?v=%d", base, verBefore))
	if code != 200 {
		t.Fatalf("state after expiry = %d, want 200 (version bumped)", code)
	}
	if !strings.Contains(body, "time ran out") {
		t.Errorf("expected 'time ran out' in log")
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.Game.Turn != firstTeam.Other() {
		t.Errorf("turn = %s, want %s", snap.Room.Game.Turn, firstTeam.Other())
	}
	if snap.Room.Game.HasClue {
		t.Errorf("HasClue still true after timeout")
	}
	if len(snap.Marks) != 0 {
		t.Errorf("marks not cleared on timeout: %v", snap.Marks)
	}
	if snap.Room.Version <= verBefore {
		t.Errorf("version not bumped on timeout: %d <= %d", snap.Room.Version, verBefore)
	}
	if snap.Room.Deadline != nowT.UnixMilli()+60_000 {
		t.Errorf("new turn deadline = %d, want %d", snap.Room.Deadline, nowT.UnixMilli()+60_000)
	}
}

func TestWordPackSelectionAndCustomValidation(t *testing.T) {
	store, _, h := newTestApp(t)
	a, b, c, d, spec := newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h)
	_, loc := a.post("/create", url.Values{"nickname": {"Alice"}})
	room := strings.TrimPrefix(loc, "/r/")
	base := "/r/" + room
	for name, cl := range map[string]*client{"B": b, "C": c, "D": d, "Spec": spec} {
		cl.post("/join", url.Values{"nickname": {name}, "code": {room}})
	}
	a.post(base+"/team", url.Values{"team": {"ember"}, "role": {"handler"}})
	b.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}})
	c.post(base+"/team", url.Values{"team": {"tide"}, "role": {"handler"}})
	d.post(base+"/team", url.Values{"team": {"tide"}, "role": {"operative"}})

	// Spectator cannot change settings.
	if code, _ := spec.post(base+"/settings", url.Values{"pack": {"hard"}}); code != http.StatusForbidden {
		t.Fatalf("spectator settings = %d, want 403", code)
	}

	// Select Hard pack in setup; start and verify hardWordList is used.
	if code, _ := a.post(base+"/settings", url.Values{"pack": {"hard"}}); code != 200 {
		t.Fatalf("set hard pack = %d", code)
	}
	if code, _ := a.post(base+"/start", nil); code != 200 {
		t.Fatalf("start hard = %d", code)
	}
	hardSet := map[string]bool{}
	for _, w := range hardWordList {
		hardSet[w] = true
	}
	snap, _ := store.Snapshot(context.Background(), room)
	for _, card := range snap.Room.Game.Cards {
		if !hardSet[card.Word] {
			t.Errorf("word %q not from hardWordList", card.Word)
		}
	}

	// Abort back to setup to test custom pack validation and selection.
	if code, _ := a.post(base+"/abort", nil); code != 200 {
		t.Fatalf("abort = %d", code)
	}

	// Custom pack validation errors.
	if code, body := a.post(base+"/settings", url.Values{"pack": {"custom"}, "custom": {"one, two, three"}}); code != 400 || !strings.Contains(body, "25") {
		t.Errorf("too few custom words = %d %q, want 400", code, body)
	}
	if code, _ := a.post(base+"/settings", url.Values{"pack": {"custom"}, "custom": {"bad123"}}); code != 400 {
		t.Errorf("digits in custom word = %d, want 400", code)
	}
	if code, _ := a.post(base+"/settings", url.Values{"pack": {"custom"}, "custom": {strings.Repeat("a", 21)}}); code != 400 {
		t.Errorf("too long custom word = %d, want 400", code)
	}
	// >400 custom words rejected.
	var over []string
	for i := range 401 {
		over = append(over, fmt.Sprintf("w%c%c", 'a'+rune(i/26), 'a'+rune(i%26)))
	}
	if code, _ := a.post(base+"/settings", url.Values{"pack": {"custom"}, "custom": {strings.Join(over, "\n")}}); code != 400 {
		t.Errorf("401 custom words = %d, want 400", code)
	}

	// Valid 25 custom words with duplicates, mixed case, hyphens, spaces.
	custom25 := []string{
		"alpha", "ALPHA", "bravo", "charlie", "delta", "echo-one",
		"foxtrot", "golf club", "hotel", "india", "juliet", "kilo",
		"lima", "mike", "november", "oscar", "papa", "quebec",
		"romeo", "sierra", "tango", "uniform", "victor", "whiskey",
		"xray", "yankee",
	}
	if code, body := a.post(base+"/settings", url.Values{"pack": {"custom"}, "custom": {strings.Join(custom25, ", ")}}); code != 200 {
		t.Fatalf("valid custom pack = %d %q", code, body)
	}
	if code, _ := a.post(base+"/start", nil); code != 200 {
		t.Fatalf("start custom = %d", code)
	}
	customSet := map[string]bool{}
	for _, w := range custom25 {
		customSet[strings.ToLower(w)] = true
	}
	snap, _ = store.Snapshot(context.Background(), room)
	for _, card := range snap.Room.Game.Cards {
		if !customSet[card.Word] {
			t.Errorf("word %q not from custom pack", card.Word)
		}
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

func TestSettingsTimerDeadlinePreservation(t *testing.T) {
	store, app, h := newTestApp(t)
	nowT := time.Unix(1_700_000_000, 0)
	app.now = func() time.Time { return nowT }

	a, b, c, d := newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h)
	_, loc := a.post("/create", url.Values{"nickname": {"Alice"}})
	room := strings.TrimPrefix(loc, "/r/")
	base := "/r/" + room
	for name, cl := range map[string]*client{"B": b, "C": c, "D": d} {
		cl.post("/join", url.Values{"nickname": {name}, "code": {room}})
	}
	a.post(base+"/team", url.Values{"team": {"ember"}, "role": {"handler"}})
	b.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}})
	c.post(base+"/team", url.Values{"team": {"tide"}, "role": {"handler"}})
	d.post(base+"/team", url.Values{"team": {"tide"}, "role": {"operative"}})

	// Enable 60s timer in setup -> TimerSecs saved, deadline stays 0 until start.
	if code, _ := a.post(base+"/settings", url.Values{"timer": {"60"}}); code != 200 {
		t.Fatalf("enable timer = %d", code)
	}
	snap, _ := store.Snapshot(context.Background(), room)
	if snap.Room.TimerSecs != 60 || snap.Room.Deadline != 0 {
		t.Fatalf("setup timer = %d deadline = %d, want 60 / 0", snap.Room.TimerSecs, snap.Room.Deadline)
	}

	// Start mission -> deadline starts at now + 60s.
	if code, _ := a.post(base+"/start", nil); code != 200 {
		t.Fatalf("start = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	origDeadline := snap.Room.Deadline
	if origDeadline != nowT.UnixMilli()+60_000 {
		t.Fatalf("origDeadline = %d, want %d", origDeadline, nowT.UnixMilli()+60_000)
	}

	// While playing, toggling public lobby does NOT move the running deadline.
	nowT = nowT.Add(20 * time.Second)
	if code, _ := a.post(base+"/settings", url.Values{"public": {"1"}}); code != 200 {
		t.Fatalf("toggle public while playing = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.Deadline != origDeadline {
		t.Fatalf("toggling public moved deadline: got %d, want %d", snap.Room.Deadline, origDeadline)
	}

	// While playing, changing timer or pack is refused with 409.
	if code, _ := a.post(base+"/settings", url.Values{"timer": {"120"}}); code != http.StatusConflict {
		t.Fatalf("change timer while playing = %d, want 409", code)
	}
	if code, _ := a.post(base+"/settings", url.Values{"pack": {"hard"}}); code != http.StatusConflict {
		t.Fatalf("change pack while playing = %d, want 409", code)
	}

	// Abort back to setup clears deadline and allows disabling timer.
	if code, _ := a.post(base+"/abort", nil); code != 200 {
		t.Fatalf("abort = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.Deadline != 0 {
		t.Fatalf("abort did not clear deadline: got %d", snap.Room.Deadline)
	}
	if code, _ := a.post(base+"/settings", url.Values{"timer": {"0"}}); code != 200 {
		t.Fatalf("disable timer in setup = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.TimerSecs != 0 || snap.Room.Deadline != 0 {
		t.Fatalf("turning timer off failed: secs=%d deadline=%d", snap.Room.TimerSecs, snap.Room.Deadline)
	}
}

func TestTimerPauseAfterThreeIdleTimeoutsAndNonMemberExpiryNoTouch(t *testing.T) {
	store, app, h := newTestApp(t)
	nowT := time.Unix(1_700_000_000, 0)
	app.now = func() time.Time { return nowT }

	a, b, outsider := newClient(t, h), newClient(t, h), newClient(t, h)
	_, loc := a.post("/create", url.Values{"nickname": {"Alice"}})
	room := strings.TrimPrefix(loc, "/r/")
	base := "/r/" + room
	b.post("/join", url.Values{"nickname": {"Bob"}, "code": {room}})

	a.post(base+"/team", url.Values{"team": {"ember"}, "role": {"handler"}})
	b.post(base+"/team", url.Values{"team": {"tide"}, "role": {"handler"}})

	// Enable 60s timer in setup, then enter playing phase with fixedGame.
	if code, _ := a.post(base+"/settings", url.Values{"timer": {"60"}}); code != 200 {
		t.Fatalf("enable timer = %d", code)
	}
	if err := store.Mutate(context.Background(), room, func(tx *sql.Tx, rm *Room) error {
		rm.Phase = PhasePlaying
		rm.Game = fixedGame()
		app.restartTimer(rm)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Make room appear stale (> 600s old), then trigger timeout #1 via non-member GET /state.
	stale := now() - 1000
	if _, err := store.w.Exec(`UPDATE rooms SET updated_at=? WHERE code=?`, stale, room); err != nil {
		t.Fatal(err)
	}
	nowT = nowT.Add(61 * time.Second)
	if code, _ := outsider.get(base + "/state?v=0"); code != http.StatusForbidden {
		t.Fatalf("outsider state = %d, want 403", code)
	}
	_, updated, _, _ := store.RoomMeta(context.Background(), room, "")
	if updated != stale {
		t.Fatalf("non-member expiry bumped updated_at: got %d, want %d", updated, stale)
	}
	snap, _ := store.Snapshot(context.Background(), room)
	if snap.Room.IdleTimeouts != 1 || snap.Room.Deadline == 0 {
		t.Fatalf("after timeout 1: idle=%d deadline=%d", snap.Room.IdleTimeouts, snap.Room.Deadline)
	}

	// Timeout #2: still running.
	nowT = nowT.Add(61 * time.Second)
	a.get(base + "/state?v=0")
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.IdleTimeouts != 2 || snap.Room.Deadline == 0 {
		t.Fatalf("after timeout 2: idle=%d deadline=%d", snap.Room.IdleTimeouts, snap.Room.Deadline)
	}

	// Timeout #3: pauses timer (deadline=0) and logs once.
	nowT = nowT.Add(61 * time.Second)
	_, body := a.get(base + "/state?v=0")
	if !strings.Contains(body, "Timer paused — nobody&#39;s moving") && !strings.Contains(body, "Timer paused — nobody's moving") {
		t.Fatalf("expected pause log entry in response")
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.Deadline != 0 {
		t.Fatalf("after timeout 3: deadline = %d, want 0", snap.Room.Deadline)
	}

	// Further polls after more time must not add duplicate pause logs.
	nowT = nowT.Add(120 * time.Second)
	a.get(base + "/state?v=0")
	snap, _ = store.Snapshot(context.Background(), room)
	pauseCount := 0
	for _, e := range snap.Log {
		if e.Text == "Timer paused — nobody's moving" {
			pauseCount++
		}
	}
	if pauseCount != 1 {
		t.Fatalf("pause log count = %d, want 1", pauseCount)
	}

	// Next clue resumes timer and resets IdleTimeouts.
	active := a
	if snap.Room.Game.Turn == Tide {
		active = b
	}
	if code, _ := active.post(base+"/clue", url.Values{"clue": {"zzzzzz"}, "number": {"1"}}); code != 200 {
		t.Fatalf("clue to resume = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.Deadline != nowT.UnixMilli()+60_000 || snap.Room.IdleTimeouts != 0 {
		t.Fatalf("after clue resume: deadline=%d idle=%d", snap.Room.Deadline, snap.Room.IdleTimeouts)
	}
}

func TestLogCappedAt200PerRoom(t *testing.T) {
	store, _, _ := newTestApp(t)
	ctx := context.Background()
	if err := store.CreateRoom(ctx, "LOGS", newGame(), Player{ID: "p1", Nickname: "Alice", Team: "ember", Role: "handler"}, false); err != nil {
		t.Fatal(err)
	}
	if err := store.Mutate(ctx, "LOGS", func(tx *sql.Tx, rm *Room) error {
		for i := range 230 {
			if err := addLog(ctx, tx, rm, LogEntry{Kind: "settings", Text: fmt.Sprintf("entry-%03d", i)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.r.QueryRow(`SELECT COUNT(*) FROM log WHERE room='LOGS'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != maxLogPerRoom {
		t.Fatalf("log count = %d, want %d", count, maxLogPerRoom)
	}
	snap, err := store.Snapshot(ctx, "LOGS")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Log[0].Text != "entry-229" || snap.Log[len(snap.Log)-1].Text != "entry-030" {
		t.Fatalf("unexpected log bounds: newest=%q oldest=%q", snap.Log[0].Text, snap.Log[len(snap.Log)-1].Text)
	}
}

func TestTimerScaleValidation(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "scale.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	bad := []string{"NaN", "Inf", "+Inf", "-Inf", "0", "-1", "-0.5", "100.1", "999", "abc"}
	for _, raw := range bad {
		t.Setenv("DEADDROP_TIMER_SCALE", raw)
		app, err := NewApp(store)
		if err != nil {
			t.Fatal(err)
		}
		if app.timerScale != 1.0 {
			t.Errorf("DEADDROP_TIMER_SCALE=%q gave %v, want 1.0", raw, app.timerScale)
		}
	}
	good := []struct {
		raw  string
		want float64
	}{
		{"0.05", 0.05},
		{"1", 1},
		{"100", 100},
	}
	for _, tc := range good {
		t.Setenv("DEADDROP_TIMER_SCALE", tc.raw)
		app, err := NewApp(store)
		if err != nil {
			t.Fatal(err)
		}
		if app.timerScale != tc.want {
			t.Errorf("DEADDROP_TIMER_SCALE=%q gave %v, want %v", tc.raw, app.timerScale, tc.want)
		}
	}
}

func TestWordListsNoDuplicatesOrOverlap(t *testing.T) {
	classic := make(map[string]bool, len(wordList))
	for _, w := range wordList {
		if classic[w] {
			t.Errorf("duplicate in classic wordList: %q", w)
		}
		classic[w] = true
	}
	hard := make(map[string]bool, len(hardWordList))
	for _, w := range hardWordList {
		if hard[w] {
			t.Errorf("duplicate in hardWordList: %q", w)
		}
		if classic[w] {
			t.Errorf("overlap between classic and hard word lists: %q", w)
		}
		hard[w] = true
	}
	mixed := wordsFor(PackMixed, "")
	seen := make(map[string]bool, len(mixed))
	for _, w := range mixed {
		if seen[w] {
			t.Errorf("duplicate in mixed pack: %q", w)
		}
		seen[w] = true
	}
	if len(mixed) != len(classic)+len(hard) {
		t.Errorf("mixed pack len = %d, want %d", len(mixed), len(classic)+len(hard))
	}
	for seed := uint64(0); seed < 20; seed++ {
		g := NewGame(testRNG(seed), mixed)
		board := make(map[string]bool, BoardSize)
		for _, c := range g.Cards {
			if board[c.Word] {
				t.Fatalf("seed %d: duplicate card %q in mixed game", seed, c.Word)
			}
			board[c.Word] = true
		}
	}
}
