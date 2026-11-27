package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

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
