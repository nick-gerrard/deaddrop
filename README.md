# Dead Drop

Dead Drop is a browser-based team word-association spy game (Codenames-style) built with Go, htmx, and SQLite.
Two teams — **EMBER** and **TIDE** — compete on a 5×5 grid of code words: each team has one **Handler** who sees the colour key and gives a single-word clue plus a number (1–9), and one or more **Operatives** who guess up to `number + 1` cards per turn.
Revealing your own agent lets you keep guessing; revealing a bystander or enemy agent ends your turn; revealing the single **BURNED** card immediately loses the round.
First team to uncover all of its agents (9 for the starting team, 8 for the other) wins.

## Run & Test

```sh
go run .
```

Environment variables:
- `DEADDROP_ADDR` — listen address (default `127.0.0.1:8080`)
- `DEADDROP_DB` — SQLite database path (default `deaddrop.db`)
- `DEADDROP_TIMER_SCALE` — optional timer multiplier in `(0, 100]` for testing (default `1`)

Run tests and vet:

```sh
go vet ./... && go test ./...
```

## Request Flow

1. **Poll:** `templates/game.html` polls `GET /r/{code}/state?v={version}` every 1.5s via htmx (`hx-swap="outerHTML"`).
2. **Version check:** `handlers_room.go` (`state`) expires any overdue turn, then reads `RoomMeta` from SQLite. If the viewer is a room member and the room version matches `v`, it returns `204 No Content` with no HTML work.
3. **Per-player view:** When the version changed (or after any `POST` action via `act`), `fragment` loads a read-only `Snapshot` and calls `view.go` (`buildView`) to build a viewer-specific `RoomView` — only Handlers (or finished games) receive unrevealed card colours, and operative marks are filtered to the viewer's own team.
4. **Render:** `templates/game.html` renders the `#game` fragment from `RoomView`. Every `POST` action runs inside `Store.Mutate` (a single serialised `BEGIN IMMEDIATE` write transaction) and bumps the room version so all other clients pick up the change on their next poll.

## File Map

- `main.go` — server startup, env config, graceful shutdown, hourly idle-room cleanup.
- `routes.go` — `App` setup, embedded assets, route table, CSRF/security/cache middleware, create rate limiter.
- `http_helpers.go` — HTTP errors, player/nickname cookies, room codes, HTML rendering, `act` transaction wrapper.
- `handlers_room.go` — home page, public lobby polling (`/lobby`), room create/join, room page, `/state` polling.
- `handlers_setup.go` — setup-phase actions: team/role assignment, start mission, abort mission, next round.
- `handlers_play.go` — play-phase actions: submit clue, reveal card, toggle operative mark, end turn.
- `settings.go` — word pack & timer settings, custom word validation, series reset, server-side turn timer expiry.
- `game.go` — pure game rules (deal, clue validation, reveal, guess limit, end turn, timeout); no DB or HTTP.
- `view.go` — builds per-viewer `RoomView` (role permissions, key visibility, marks, recap) and `LobbyView`.
- `db.go` — SQLite schema, auto-migrations, serialised writer + reader pool, snapshots, lobby queries.
- `words.go`, `words_hard.go` — built-in Classic and Hard word lists (Mixed combines both).
- `templates/` — `base.html`, `home.html`, `join.html`, `room.html`, `game.html` (setup + play fragment), `lobby.html`.
- `static/` — `style.css`, `app.js` (card confirm, countdown tick, sound, copy link), `htmx.min.js`, `favicon.svg`.
- `deploy.toml` — Roost deployment config.

Tests: `game_rules_test.go`, `db_test.go`, `room_security_test.go`, `setup_phase_test.go`, `gameplay_test.go`, `settings_timer_test.go`, `lobby_test.go`, `testutil_test.go`.

## Where the Main Rules Live

- **Core turn & win rules:** `game.go` (`ValidateClue`, `SubmitClue`, `Reveal`, `GuessesLeft`, `EndTurn`, `TimeOut`).
- **Room phases & role locks:** `handlers_setup.go` (`doTeam`, `doStart`, `doAbort`, `doNewRound`) and `settings.go` (`doSettings`).
- **Play-phase permissions:** `handlers_play.go` (`doClue`, `doReveal`, `doMark`, `doEnd`).
- **Secret key & mark visibility:** `view.go` (`buildView` — `ShowKey` and `marksByCard`).
- **UI gating:** `view.go` (`CanStart`, `CanClue`, `CanGuess`, `CanEnd`, `CanSettings`) + `templates/game.html`.

## Migrations

`db.go` runs `CREATE TABLE IF NOT EXISTS` followed by `migrate()` on every `OpenStore` startup.
Columns added after the initial release live in `roomColumns`: `migrate` inspects `PRAGMA table_info(rooms)` and `ALTER TABLE rooms ADD COLUMN`s any missing columns idempotently. When adding the `phase` column to a pre-existing DB, `backfillPhase` sets `phase='playing'` for rooms that already have a dealt board.

## Deploy

Deployed via **Roost** (`deploy.toml`, `on = "push"`): pushing to `main` builds the `deaddrop` binary, runs `go test ./...`, and restarts the service with health check `GET /healthz`.
