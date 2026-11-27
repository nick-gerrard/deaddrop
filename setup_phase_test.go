package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

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
