package main

import (
	"context"
	"database/sql"
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

func TestTimerExpiryWithInjectedClock(t *testing.T) {
	store, app, h := newTestApp(t)
	nowT := time.Unix(1_700_000_000, 0)
	app.now = func() time.Time { return nowT }

	a, b := newClient(t, h), newClient(t, h)
	_, loc := a.post("/create", url.Values{"nickname": {"Alice"}})
	room := strings.TrimPrefix(loc, "/r/")
	base := "/r/" + room
	b.post("/join", url.Values{"nickname": {"Bob"}, "code": {room}})

	snap, _ := store.Snapshot(context.Background(), room)
	firstTeam := snap.Room.Game.Turn
	a.post(base+"/team", url.Values{"team": {string(firstTeam)}, "role": {"handler"}})
	b.post(base+"/team", url.Values{"team": {string(firstTeam)}, "role": {"operative"}})

	// Enable 60s turn timer.
	if code, body := a.post(base+"/settings", url.Values{"timer": {"60"}}); code != 200 || !strings.Contains(body, `data-deadline="`) {
		t.Fatalf("enable timer = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	dl1 := snap.Room.Deadline
	if dl1 != nowT.UnixMilli()+60_000 {
		t.Fatalf("deadline = %d, want %d", dl1, nowT.UnixMilli()+60_000)
	}

	// Advance 30s and give a clue: deadline must restart for the guess phase.
	nowT = nowT.Add(30 * time.Second)
	if code, _ := a.post(base+"/clue", url.Values{"clue": {"zzzzzz"}, "number": {"2"}}); code != 200 {
		t.Fatalf("clue = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	dl2 := snap.Room.Deadline
	if dl2 != nowT.UnixMilli()+60_000 {
		t.Fatalf("deadline after clue = %d, want %d", dl2, nowT.UnixMilli()+60_000)
	}

	// Place a mark before timeout to ensure timeout clears marks too.
	if code, _ := b.post(base+"/mark", url.Values{"card": {"0"}}); code != 200 {
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
	a, spec := newClient(t, h), newClient(t, h)
	_, loc := a.post("/create", url.Values{"nickname": {"Alice"}})
	room := strings.TrimPrefix(loc, "/r/")
	base := "/r/" + room
	spec.post("/join", url.Values{"nickname": {"Spec"}, "code": {room}})
	a.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}})

	// Spectator cannot change settings.
	if code, _ := spec.post(base+"/settings", url.Values{"pack": {"hard"}}); code != http.StatusForbidden {
		t.Fatalf("spectator settings = %d, want 403", code)
	}

	// Select Hard pack; verify next round uses hardWordList.
	if code, _ := a.post(base+"/settings", url.Values{"pack": {"hard"}}); code != 200 {
		t.Fatalf("set hard pack = %d", code)
	}
	if code, _ := a.post(base+"/new", nil); code != 200 {
		t.Fatalf("new round hard = %d", code)
	}
	hardSet := map[string]bool{}
	for _, w := range hardWordList {
		hardSet[w] = true
	}
	snap, _ := store.Snapshot(context.Background(), room)
	for _, c := range snap.Room.Game.Cards {
		if !hardSet[c.Word] {
			t.Errorf("word %q not from hardWordList", c.Word)
		}
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
	if code, _ := a.post(base+"/new", nil); code != 200 {
		t.Fatalf("new round custom = %d", code)
	}
	customSet := map[string]bool{}
	for _, w := range custom25 {
		customSet[strings.ToLower(w)] = true
	}
	snap, _ = store.Snapshot(context.Background(), room)
	for _, c := range snap.Room.Game.Cards {
		if !customSet[c.Word] {
			t.Errorf("word %q not from custom pack", c.Word)
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

	// Install deterministic board: ember starts, idx 24 is burned.
	if err := store.Mutate(context.Background(), room, func(tx *sql.Tx, rm *Room) error {
		rm.Game = fixedGame()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.post(base+"/team", url.Values{"team": {"ember"}, "role": {"handler"}})
	b.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}})
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
	handler, op1, op2, enemy, spec := newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h), newClient(t, h)
	_, loc := handler.post("/create", url.Values{"nickname": {"Hannah"}})
	room := strings.TrimPrefix(loc, "/r/")
	base := "/r/" + room
	op1.post("/join", url.Values{"nickname": {"Jane Doe"}, "code": {room}})
	op2.post("/join", url.Values{"nickname": {"Bob"}, "code": {room}})
	enemy.post("/join", url.Values{"nickname": {"Eve"}, "code": {room}})
	spec.post("/join", url.Values{"nickname": {"Spec"}, "code": {room}})

	if err := store.Mutate(context.Background(), room, func(tx *sql.Tx, rm *Room) error {
		rm.Game = fixedGame()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	handler.post(base+"/team", url.Values{"team": {"ember"}, "role": {"handler"}})
	op1.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}})
	op2.post(base+"/team", url.Values{"team": {"ember"}, "role": {"operative"}})
	enemy.post(base+"/team", url.Values{"team": {"tide"}, "role": {"operative"}})

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
	enemyHandler := newClient(t, h)
	enemyHandler.post("/join", url.Values{"nickname": {"EnemyH"}, "code": {room}})
	enemyHandler.post(base+"/team", url.Values{"team": {"tide"}, "role": {"handler"}})
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

	a := newClient(t, h)
	_, loc := a.post("/create", url.Values{"nickname": {"Alice"}})
	room := strings.TrimPrefix(loc, "/r/")
	base := "/r/" + room

	snap, _ := store.Snapshot(context.Background(), room)
	firstTeam := snap.Room.Game.Turn
	a.post(base+"/team", url.Values{"team": {string(firstTeam)}, "role": {"handler"}})

	// Enable 60s timer -> starts deadline.
	if code, _ := a.post(base+"/settings", url.Values{"timer": {"60"}}); code != 200 {
		t.Fatalf("enable timer = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	origDeadline := snap.Room.Deadline
	if origDeadline != nowT.UnixMilli()+60_000 {
		t.Fatalf("origDeadline = %d, want %d", origDeadline, nowT.UnixMilli()+60_000)
	}

	// Advance 20s and re-save settings (change pack, same timer): deadline must not move.
	nowT = nowT.Add(20 * time.Second)
	if code, _ := a.post(base+"/settings", url.Values{"timer": {"60"}, "pack": {"hard"}}); code != 200 {
		t.Fatalf("resave settings = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.Deadline != origDeadline {
		t.Fatalf("resaving settings moved deadline: got %d, want %d", snap.Room.Deadline, origDeadline)
	}

	// Change timer duration from 60s to 120s: current turn deadline stays as is.
	nowT = nowT.Add(10 * time.Second)
	if code, _ := a.post(base+"/settings", url.Values{"timer": {"120"}}); code != 200 {
		t.Fatalf("change timer = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.Deadline != origDeadline {
		t.Fatalf("changing duration restarted running deadline: got %d, want %d", snap.Room.Deadline, origDeadline)
	}
	if snap.Room.TimerSecs != 120 {
		t.Fatalf("TimerSecs = %d, want 120", snap.Room.TimerSecs)
	}

	// Next turn phase (giving a clue) uses the new 120s duration.
	if code, _ := a.post(base+"/clue", url.Values{"clue": {"zzzzzz"}, "number": {"1"}}); code != 200 {
		t.Fatalf("clue = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.Deadline != nowT.UnixMilli()+120_000 {
		t.Fatalf("next turn deadline = %d, want %d", snap.Room.Deadline, nowT.UnixMilli()+120_000)
	}

	// Turning timer off clears the deadline immediately.
	if code, _ := a.post(base+"/settings", url.Values{"timer": {"0"}}); code != 200 {
		t.Fatalf("disable timer = %d", code)
	}
	snap, _ = store.Snapshot(context.Background(), room)
	if snap.Room.Deadline != 0 {
		t.Fatalf("turning timer off did not clear deadline: got %d", snap.Room.Deadline)
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

	if err := store.Mutate(context.Background(), room, func(tx *sql.Tx, rm *Room) error {
		rm.Game = fixedGame()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.post(base+"/team", url.Values{"team": {"ember"}, "role": {"handler"}})
	b.post(base+"/team", url.Values{"team": {"tide"}, "role": {"handler"}})

	// Enable 60s timer.
	if code, _ := a.post(base+"/settings", url.Values{"timer": {"60"}}); code != 200 {
		t.Fatalf("enable timer = %d", code)
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
	if err := store.CreateRoom(ctx, "LOGS", newGame(), Player{ID: "p1", Nickname: "Alice", Team: "ember", Role: "handler"}); err != nil {
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
