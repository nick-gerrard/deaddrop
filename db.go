package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS rooms (
	code       TEXT PRIMARY KEY,
	version    INTEGER NOT NULL DEFAULT 1,
	round      INTEGER NOT NULL DEFAULT 1,
	game       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS players (
	room      TEXT NOT NULL REFERENCES rooms(code) ON DELETE CASCADE,
	id        TEXT NOT NULL,
	nickname  TEXT NOT NULL,
	team      TEXT NOT NULL DEFAULT 'none',
	role      TEXT NOT NULL DEFAULT 'operative',
	joined_at INTEGER NOT NULL,
	PRIMARY KEY (room, id)
);
CREATE TABLE IF NOT EXISTS log (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	room       TEXT NOT NULL REFERENCES rooms(code) ON DELETE CASCADE,
	round      INTEGER NOT NULL,
	kind       TEXT NOT NULL,
	team       TEXT NOT NULL DEFAULT '',
	color      TEXT NOT NULL DEFAULT '',
	nickname   TEXT NOT NULL DEFAULT '',
	text       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS log_room ON log(room, id);
`

const (
	maxPlayersPerRoom = 40
	maxLiveRooms      = 1000
)

var (
	ErrRoomNotFound = errors.New("room not found")
	ErrTooManyRooms = errors.New("too many active rooms")
)

type Player struct {
	ID       string
	Nickname string
	Team     string // none | ember | tide
	Role     string // operative | handler
}

func (p Player) IsHandler() bool { return p.Team != "none" && p.Role == "handler" }

type LogEntry struct {
	Kind     string // clue | reveal | end | win | round
	Team     Team
	Color    Color
	Nickname string
	Text     string
}

func (e LogEntry) TeamLabel() string  { return e.Team.Label() }
func (e LogEntry) ColorLabel() string { return e.Color.Label() }

type Room struct {
	Code      string
	Version   int64
	Round     int
	Game      *Game
	UpdatedAt int64
}

type Snapshot struct {
	Room    Room
	Players []Player
	Log     []LogEntry
}

// Store uses a single serialised writer (BEGIN IMMEDIATE) and a separate
// reader pool so polling never blocks on game actions.
type Store struct {
	w, r     *sql.DB
	maxRooms int
}

func OpenStore(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	w, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if _, err := w.Exec(schema); err != nil {
		w.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	return &Store{w: w, r: r, maxRooms: maxLiveRooms}, nil
}

func (s *Store) Close() {
	s.w.Close()
	s.r.Close()
}

func now() int64 { return time.Now().Unix() }

// CreateRoom inserts a new room and its first player atomically.
func (s *Store) CreateRoom(ctx context.Context, code string, g *Game, p Player) error {
	blob, err := json.Marshal(g)
	if err != nil {
		return err
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.maxRooms > 0 {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rooms`).Scan(&n); err != nil {
			return err
		}
		if n >= s.maxRooms {
			return ErrTooManyRooms
		}
	}
	t := now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO rooms(code, game, created_at, updated_at) VALUES (?,?,?,?)`, code, string(blob), t, t); err != nil {
		return err
	}
	if err := upsertPlayer(ctx, tx, code, p); err != nil {
		return err
	}
	return tx.Commit()
}

// Mutate runs fn on the room inside one write transaction. If fn returns nil
// the (possibly modified) game is saved and the room version is bumped.
func (s *Store) Mutate(ctx context.Context, code string, fn func(tx *sql.Tx, rm *Room) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var rm Room
	var blob string
	err = tx.QueryRowContext(ctx, `SELECT code, version, round, game, updated_at FROM rooms WHERE code = ?`, code).
		Scan(&rm.Code, &rm.Version, &rm.Round, &blob, &rm.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRoomNotFound
	}
	if err != nil {
		return err
	}
	rm.Game = new(Game)
	if err := json.Unmarshal([]byte(blob), rm.Game); err != nil {
		return fmt.Errorf("decode game: %w", err)
	}
	if err := fn(tx, &rm); err != nil {
		return err
	}
	out, err := json.Marshal(rm.Game)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE rooms SET game=?, round=?, version=version+1, updated_at=? WHERE code=?`,
		string(out), rm.Round, now(), code); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertPlayer(ctx context.Context, tx *sql.Tx, code string, p Player) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO players(room, id, nickname, team, role, joined_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(room, id) DO UPDATE SET nickname = excluded.nickname`,
		code, p.ID, p.Nickname, p.Team, p.Role, now())
	return err
}

func countPlayers(ctx context.Context, tx *sql.Tx, code string) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM players WHERE room = ?`, code).Scan(&n)
	return n, err
}

func playerExists(ctx context.Context, tx *sql.Tx, code, id string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM players WHERE room=? AND id=?`, code, id).Scan(&n)
	return n > 0, err
}

func getPlayer(ctx context.Context, tx *sql.Tx, code, id string) (*Player, error) {
	p := Player{ID: id}
	err := tx.QueryRowContext(ctx, `SELECT nickname, team, role FROM players WHERE room=? AND id=?`, code, id).
		Scan(&p.Nickname, &p.Team, &p.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

func countTeamHandlers(ctx context.Context, tx *sql.Tx, code, team string) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM players WHERE room=? AND team=? AND role='handler'`, code, team).Scan(&n)
	return n, err
}

func setPlayerTeam(ctx context.Context, tx *sql.Tx, code, id, team, role string) error {
	_, err := tx.ExecContext(ctx, `UPDATE players SET team=?, role=? WHERE room=? AND id=?`, team, role, code, id)
	return err
}

func addLog(ctx context.Context, tx *sql.Tx, rm *Room, e LogEntry) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO log(room, round, kind, team, color, nickname, text, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		rm.Code, rm.Round, e.Kind, string(e.Team), string(e.Color), e.Nickname, e.Text, now())
	return err
}

func clearOldLog(ctx context.Context, tx *sql.Tx, rm *Room) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM log WHERE room=? AND round<?`, rm.Code, rm.Round)
	return err
}

// RoomMeta returns the cheap polling info for a room and whether pid is a member.
func (s *Store) RoomMeta(ctx context.Context, code, pid string) (version, updatedAt int64, isMember bool, err error) {
	var mem int
	err = s.r.QueryRowContext(ctx, `SELECT version, updated_at, EXISTS(SELECT 1 FROM players WHERE room=? AND id=?) FROM rooms WHERE code=?`, code, pid, code).
		Scan(&version, &updatedAt, &mem)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrRoomNotFound
	}
	isMember = pid != "" && mem != 0
	return
}

// Touch marks a room as recently used without changing its version.
func (s *Store) Touch(ctx context.Context, code string) error {
	_, err := s.w.ExecContext(ctx, `UPDATE rooms SET updated_at=? WHERE code=?`, now(), code)
	return err
}

// Snapshot loads a consistent view of a room (one read transaction).
func (s *Store) Snapshot(ctx context.Context, code string) (*Snapshot, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	snap := &Snapshot{}
	var blob string
	err = tx.QueryRowContext(ctx, `SELECT code, version, round, game, updated_at FROM rooms WHERE code=?`, code).
		Scan(&snap.Room.Code, &snap.Room.Version, &snap.Room.Round, &blob, &snap.Room.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRoomNotFound
	}
	if err != nil {
		return nil, err
	}
	snap.Room.Game = new(Game)
	if err := json.Unmarshal([]byte(blob), snap.Room.Game); err != nil {
		return nil, fmt.Errorf("decode game: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `SELECT id, nickname, team, role FROM players WHERE room=? ORDER BY joined_at, nickname, id`, code)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p Player
		if err := rows.Scan(&p.ID, &p.Nickname, &p.Team, &p.Role); err != nil {
			rows.Close()
			return nil, err
		}
		snap.Players = append(snap.Players, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx, `SELECT kind, team, color, nickname, text FROM log WHERE room=? AND round=? ORDER BY id DESC LIMIT 200`, code, snap.Room.Round)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e LogEntry
		var team, color string
		if err := rows.Scan(&e.Kind, &team, &color, &e.Nickname, &e.Text); err != nil {
			return nil, err
		}
		e.Team, e.Color = Team(team), Color(color)
		snap.Log = append(snap.Log, e)
	}
	return snap, rows.Err()
}

// DeleteIdleRooms removes rooms not touched for longer than maxIdle.
func (s *Store) DeleteIdleRooms(ctx context.Context, maxIdle time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxIdle).Unix()
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	const idle = `(SELECT code FROM rooms WHERE updated_at < ?)`
	for _, q := range []string{
		`DELETE FROM log WHERE room IN ` + idle,
		`DELETE FROM players WHERE room IN ` + idle,
	} {
		if _, err := tx.ExecContext(ctx, q, cutoff); err != nil {
			return 0, err
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM rooms WHERE updated_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
