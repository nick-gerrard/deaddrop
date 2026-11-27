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
CREATE TABLE IF NOT EXISTS card_marks (
	room      TEXT NOT NULL REFERENCES rooms(code) ON DELETE CASCADE,
	card_idx  INTEGER NOT NULL,
	player_id TEXT NOT NULL,
	PRIMARY KEY (room, card_idx, player_id)
);
`

const (
	PhaseSetup   = "setup"
	PhasePlaying = "playing"
)

// roomColumns are columns added after the first release. migrate adds any that
// are missing, so it is safe to run on every startup (fresh or live DB).
var roomColumns = []struct{ name, def string }{
	{"deadline", "INTEGER NOT NULL DEFAULT 0"}, // turn deadline, unix ms; 0 = no timer running
	{"timer_secs", "INTEGER NOT NULL DEFAULT 0"},
	{"word_pack", "TEXT NOT NULL DEFAULT 'classic'"},
	{"custom_words", "TEXT NOT NULL DEFAULT ''"}, // newline separated
	{"ember_wins", "INTEGER NOT NULL DEFAULT 0"},
	{"tide_wins", "INTEGER NOT NULL DEFAULT 0"},
	{"idle_timeouts", "INTEGER NOT NULL DEFAULT 0"},
	{"public", "INTEGER NOT NULL DEFAULT 0"},
	{"phase", "TEXT NOT NULL DEFAULT 'setup'"},
}

const roomSelect = `SELECT code, version, round, game, updated_at, deadline, timer_secs, word_pack, custom_words, ember_wins, tide_wins, idle_timeouts, public, phase FROM rooms`

func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(rooms)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range roomColumns {
		if have[c.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE rooms ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
		if c.name == "phase" {
			if err := backfillPhase(db); err != nil {
				return fmt.Errorf("backfill phase: %w", err)
			}
		}
	}
	return nil
}

// backfillPhase sets phase='playing' for pre-migration rooms that already have
// a dealt board so live rounds survive the upgrade; rooms without a board stay
// in 'setup'.
func backfillPhase(db *sql.DB) error {
	rows, err := db.Query(`SELECT code, game FROM rooms`)
	if err != nil {
		return err
	}
	var playing []string
	for rows.Next() {
		var code, blob string
		if err := rows.Scan(&code, &blob); err != nil {
			rows.Close()
			return err
		}
		var g Game
		if err := json.Unmarshal([]byte(blob), &g); err == nil && len(g.Cards) > 0 {
			playing = append(playing, code)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, code := range playing {
		if _, err := db.Exec(`UPDATE rooms SET phase=? WHERE code=?`, PhasePlaying, code); err != nil {
			return err
		}
	}
	return nil
}

const (
	maxPlayersPerRoom = 40
	maxLiveRooms      = 1000
	maxLogPerRoom     = 200
)

var (
	ErrRoomNotFound = errors.New("room not found")
	ErrTooManyRooms = errors.New("too many active rooms")

	// errNoChange can be returned from a Mutate callback to commit nothing
	// (no write, no version bump) without reporting an error.
	errNoChange = errors.New("no change")
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

	Deadline     int64  // unix ms when the current turn phase times out; 0 = none
	TimerSecs    int    // 0 = timer off
	Pack         string // classic | hard | mixed | custom (applies from the next round)
	Custom       string // newline separated custom words
	EmberWins    int
	TideWins     int
	IdleTimeouts int
	Public       bool
	Phase        string // setup | playing
}

// Wins returns the series score for a team.
func (rm *Room) Wins(t Team) int {
	if t == Tide {
		return rm.TideWins
	}
	return rm.EmberWins
}

// Mark is one operative's suggestion on a card.
type Mark struct {
	Card     int
	PlayerID string
}

type Snapshot struct {
	Room    Room
	Players []Player
	Log     []LogEntry
	Marks   []Mark
}

type rowScanner interface{ Scan(dest ...any) error }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanRoom(row rowScanner, rm *Room) error {
	var blob string
	var pub int
	if err := row.Scan(&rm.Code, &rm.Version, &rm.Round, &blob, &rm.UpdatedAt, &rm.Deadline, &rm.TimerSecs, &rm.Pack, &rm.Custom, &rm.EmberWins, &rm.TideWins, &rm.IdleTimeouts, &pub, &rm.Phase); err != nil {
		return err
	}
	if rm.Phase != PhasePlaying {
		rm.Phase = PhaseSetup
	}
	rm.Public = pub != 0
	rm.Game = new(Game)
	if err := json.Unmarshal([]byte(blob), rm.Game); err != nil {
		return fmt.Errorf("decode game: %w", err)
	}
	return nil
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
	if err := migrate(w); err != nil {
		w.Close()
		return nil, fmt.Errorf("migrate: %w", err)
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
func (s *Store) CreateRoom(ctx context.Context, code string, g *Game, p Player, public bool) error {
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO rooms(code, game, created_at, updated_at, public, phase) VALUES (?,?,?,?,?,?)`, code, string(blob), t, t, boolToInt(public), PhaseSetup); err != nil {
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
	return s.mutate(ctx, code, true, fn)
}

// MutateNoTouch is like Mutate but does not bump updated_at, so background or
// inactivity transitions (such as turn expiry) do not keep idle rooms alive.
func (s *Store) MutateNoTouch(ctx context.Context, code string, fn func(tx *sql.Tx, rm *Room) error) error {
	return s.mutate(ctx, code, false, fn)
}

func (s *Store) mutate(ctx context.Context, code string, touch bool, fn func(tx *sql.Tx, rm *Room) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var rm Room
	err = scanRoom(tx.QueryRowContext(ctx, roomSelect+` WHERE code = ?`, code), &rm)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRoomNotFound
	}
	if err != nil {
		return err
	}
	if err := fn(tx, &rm); err != nil {
		if errors.Is(err, errNoChange) {
			return nil
		}
		return err
	}
	out, err := json.Marshal(rm.Game)
	if err != nil {
		return err
	}
	if touch {
		_, err = tx.ExecContext(ctx, `UPDATE rooms SET game=?, round=?, deadline=?, timer_secs=?, word_pack=?, custom_words=?, ember_wins=?, tide_wins=?, idle_timeouts=?, public=?, phase=?, version=version+1, updated_at=? WHERE code=?`,
			string(out), rm.Round, rm.Deadline, rm.TimerSecs, rm.Pack, rm.Custom, rm.EmberWins, rm.TideWins, rm.IdleTimeouts, boolToInt(rm.Public), rm.Phase, now(), code)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE rooms SET game=?, round=?, deadline=?, timer_secs=?, word_pack=?, custom_words=?, ember_wins=?, tide_wins=?, idle_timeouts=?, public=?, phase=?, version=version+1 WHERE code=?`,
			string(out), rm.Round, rm.Deadline, rm.TimerSecs, rm.Pack, rm.Custom, rm.EmberWins, rm.TideWins, rm.IdleTimeouts, boolToInt(rm.Public), rm.Phase, code)
	}
	if err != nil {
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

func countTeamOperatives(ctx context.Context, tx *sql.Tx, code, team string) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM players WHERE room=? AND team=? AND role='operative'`, code, team).Scan(&n)
	return n, err
}

func setPlayerTeam(ctx context.Context, tx *sql.Tx, code, id, team, role string) error {
	_, err := tx.ExecContext(ctx, `UPDATE players SET team=?, role=? WHERE room=? AND id=?`, team, role, code, id)
	return err
}

// toggleMark adds the player's mark on a card, or removes it if present.
// It reports whether the mark now exists.
func toggleMark(ctx context.Context, tx *sql.Tx, code string, card int, pid string) (bool, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM card_marks WHERE room=? AND card_idx=? AND player_id=?`, code, card, pid)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return false, nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO card_marks(room, card_idx, player_id) VALUES (?,?,?)`, code, card, pid)
	return true, err
}

func clearMarks(ctx context.Context, tx *sql.Tx, code string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM card_marks WHERE room=?`, code)
	return err
}

func clearCardMarks(ctx context.Context, tx *sql.Tx, code string, card int) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM card_marks WHERE room=? AND card_idx=?`, code, card)
	return err
}

func clearPlayerMarks(ctx context.Context, tx *sql.Tx, code, pid string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM card_marks WHERE room=? AND player_id=?`, code, pid)
	return err
}

func addLog(ctx context.Context, tx *sql.Tx, rm *Room, e LogEntry) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO log(room, round, kind, team, color, nickname, text, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		rm.Code, rm.Round, e.Kind, string(e.Team), string(e.Color), e.Nickname, e.Text, now()); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM log WHERE room=? AND id NOT IN (SELECT id FROM log WHERE room=? ORDER BY id DESC LIMIT ?)`,
		rm.Code, rm.Code, maxLogPerRoom)
	return err
}

func clearOldLog(ctx context.Context, tx *sql.Tx, rm *Room) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM log WHERE room=? AND round<?`, rm.Code, rm.Round)
	return err
}

func clearRoundLog(ctx context.Context, tx *sql.Tx, rm *Room) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM log WHERE room=? AND round=?`, rm.Code, rm.Round)
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

// Deadline returns the room's current turn deadline (unix ms, 0 = none).
func (s *Store) Deadline(ctx context.Context, code string) (int64, error) {
	var d int64
	err := s.r.QueryRowContext(ctx, `SELECT deadline FROM rooms WHERE code=?`, code).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrRoomNotFound
	}
	return d, err
}

// Snapshot loads a consistent view of a room (one read transaction).
func (s *Store) Snapshot(ctx context.Context, code string) (*Snapshot, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	snap := &Snapshot{}
	err = scanRoom(tx.QueryRowContext(ctx, roomSelect+` WHERE code=?`, code), &snap.Room)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRoomNotFound
	}
	if err != nil {
		return nil, err
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
	for rows.Next() {
		var e LogEntry
		var team, color string
		if err := rows.Scan(&e.Kind, &team, &color, &e.Nickname, &e.Text); err != nil {
			rows.Close()
			return nil, err
		}
		e.Team, e.Color = Team(team), Color(color)
		snap.Log = append(snap.Log, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx, `SELECT card_idx, player_id FROM card_marks WHERE room=? ORDER BY rowid`, code)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m Mark
		if err := rows.Scan(&m.Card, &m.PlayerID); err != nil {
			return nil, err
		}
		snap.Marks = append(snap.Marks, m)
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
		`DELETE FROM card_marks WHERE room IN ` + idle,
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

// LobbyRoom is a summary of an active public room for the home page lobby.
type LobbyRoom struct {
	Code       string
	Phase      string
	Round      int
	Game       *Game
	UpdatedAt  int64
	EmberCount int
	TideCount  int
}

// ListPublicRooms returns up to limit public rooms updated at or after since
// (unix seconds), ordered by most recent activity first.
func (s *Store) ListPublicRooms(ctx context.Context, since int64, limit int) ([]LobbyRoom, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT r.code, r.phase, r.round, r.game, r.updated_at,
		       COALESCE(SUM(CASE WHEN p.team = 'ember' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN p.team = 'tide' THEN 1 ELSE 0 END), 0)
		FROM rooms r
		LEFT JOIN players p ON p.room = r.code
		WHERE r.public = 1 AND r.updated_at >= ?
		GROUP BY r.code
		ORDER BY r.updated_at DESC, r.code ASC
		LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LobbyRoom
	for rows.Next() {
		var lr LobbyRoom
		var blob string
		if err := rows.Scan(&lr.Code, &lr.Phase, &lr.Round, &blob, &lr.UpdatedAt, &lr.EmberCount, &lr.TideCount); err != nil {
			return nil, err
		}
		if lr.Phase != PhasePlaying {
			lr.Phase = PhaseSetup
		}
		lr.Game = new(Game)
		if err := json.Unmarshal([]byte(blob), lr.Game); err != nil {
			return nil, fmt.Errorf("decode game %s: %w", lr.Code, err)
		}
		out = append(out, lr)
	}
	return out, rows.Err()
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
