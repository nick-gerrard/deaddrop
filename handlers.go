package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

//go:embed templates/*.html static
var assets embed.FS

const (
	cookiePlayer = "dd_pid"
	cookieNick   = "dd_nick"
	codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ" // no I, L, O: easily confused
	codeLen      = 4
	maxNickLen   = 20
)

type App struct {
	store  *Store
	tmpl   *template.Template
	assetV string
}

func NewApp(store *Store) (*App, error) {
	t, err := template.ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	for _, f := range []string{"static/style.css", "static/htmx.min.js"} {
		b, err := fs.ReadFile(assets, f)
		if err != nil {
			return nil, err
		}
		h.Write(b)
	}
	return &App{store: store, tmpl: t, assetV: hex.EncodeToString(h.Sum(nil))[:10]}, nil
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "ok")
	})
	mux.Handle("GET /static/", cacheFor(http.FileServerFS(assets)))
	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("POST /create", a.create)
	mux.HandleFunc("POST /join", a.joinByCode)
	mux.HandleFunc("GET /r/{code}", a.room)
	mux.HandleFunc("POST /r/{code}/join", a.joinRoom)
	mux.HandleFunc("GET /r/{code}/state", a.state)
	mux.HandleFunc("POST /r/{code}/team", a.act(a.doTeam))
	mux.HandleFunc("POST /r/{code}/clue", a.act(a.doClue))
	mux.HandleFunc("POST /r/{code}/reveal", a.act(a.doReveal))
	mux.HandleFunc("POST /r/{code}/end", a.act(a.doEnd))
	mux.HandleFunc("POST /r/{code}/new", a.act(a.doNewRound))
	return secureHeaders(mux)
}

func cacheFor(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

func secureHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		h.ServeHTTP(w, r)
	})
}

// ---------- errors ----------

type httpError struct {
	Status int
	Msg    string
}

func (e *httpError) Error() string { return e.Msg }

func forbidden(msg string) error  { return &httpError{http.StatusForbidden, msg} }
func badRequest(msg string) error { return &httpError{http.StatusBadRequest, msg} }

// gameError maps a game-rule error to an HTTP status.
func gameError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrBadClue), errors.Is(err, ErrBadIndex):
		return badRequest(err.Error())
	default:
		return &httpError{http.StatusConflict, err.Error()}
	}
}

func (a *App) fail(w http.ResponseWriter, r *http.Request, err error) {
	var he *httpError
	switch {
	case errors.As(err, &he):
		http.Error(w, he.Msg, he.Status)
	case errors.Is(err, ErrRoomNotFound):
		w.Header().Set("HX-Redirect", "/")
		http.Error(w, "room not found", http.StatusNotFound)
	default:
		log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
	}
}

// ---------- identity ----------

func playerID(r *http.Request) string {
	c, err := r.Cookie(cookiePlayer)
	if err != nil || len(c.Value) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(c.Value); err != nil {
		return ""
	}
	return c.Value
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func ensurePlayerID(w http.ResponseWriter, r *http.Request) string {
	if id := playerID(r); id != "" {
		return id
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	id := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: cookiePlayer, Value: id, Path: "/", MaxAge: 365 * 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureRequest(r),
	})
	return id
}

func rememberNick(w http.ResponseWriter, r *http.Request, nick string) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieNick, Value: url.QueryEscape(nick), Path: "/", MaxAge: 365 * 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureRequest(r),
	})
}

func savedNick(r *http.Request) string {
	c, err := r.Cookie(cookieNick)
	if err != nil {
		return ""
	}
	v, err := url.QueryUnescape(c.Value)
	if err != nil {
		return ""
	}
	nick, _ := cleanNick(v)
	return nick
}

func cleanNick(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if s == "" {
		return "", errors.New("Enter a nickname")
	}
	if r := []rune(s); len(r) > maxNickLen {
		s = string(r[:maxNickLen])
	}
	return s, nil
}

func normCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if unicode.IsLetter(r) && r < 128 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func newCode() string {
	b := make([]byte, codeLen)
	for i := range b {
		b[i] = codeAlphabet[mrand.IntN(len(codeAlphabet))]
	}
	return string(b)
}

func newRNG() *mrand.Rand {
	var seed [32]byte
	if _, err := rand.Read(seed[:]); err != nil {
		panic(err)
	}
	return mrand.New(mrand.NewChaCha8(seed))
}

func newGame() *Game { return NewGame(newRNG(), wordList) }

// ---------- pages ----------

type pageData struct {
	AssetV string
	Error  string
	Nick   string
	Code   string
}

func (a *App) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := a.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	a.render(w, http.StatusOK, "home.html", pageData{AssetV: a.assetV, Nick: savedNick(r)})
}

func (a *App) homeError(w http.ResponseWriter, r *http.Request, status int, nick, code, msg string) {
	a.render(w, status, "home.html", pageData{AssetV: a.assetV, Nick: nick, Code: code, Error: msg})
}

func (a *App) create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	nick, err := cleanNick(r.FormValue("nickname"))
	if err != nil {
		a.homeError(w, r, http.StatusBadRequest, "", "", err.Error())
		return
	}
	id := ensurePlayerID(w, r)
	for range 10 {
		code := newCode()
		err := a.store.CreateRoom(r.Context(), code, newGame(), Player{ID: id, Nickname: nick, Team: "none", Role: "operative"})
		if isUniqueViolation(err) {
			continue
		}
		if err != nil {
			a.fail(w, r, err)
			return
		}
		rememberNick(w, r, nick)
		http.Redirect(w, r, "/r/"+code, http.StatusSeeOther)
		return
	}
	a.fail(w, r, errors.New("could not allocate a room code"))
}

func (a *App) joinByCode(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	nick, err := cleanNick(r.FormValue("nickname"))
	code := normCode(r.FormValue("code"))
	if err != nil {
		a.homeError(w, r, http.StatusBadRequest, "", code, err.Error())
		return
	}
	if len(code) != codeLen {
		a.homeError(w, r, http.StatusBadRequest, nick, code, "Room codes are 4 letters")
		return
	}
	a.join(w, r, code, nick, func(msg string, status int) { a.homeError(w, r, status, nick, code, msg) })
}

func (a *App) joinRoom(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	code := normCode(r.PathValue("code"))
	nick, err := cleanNick(r.FormValue("nickname"))
	if err != nil {
		a.render(w, http.StatusBadRequest, "join.html", pageData{AssetV: a.assetV, Code: code, Error: err.Error()})
		return
	}
	a.join(w, r, code, nick, func(msg string, status int) {
		a.render(w, status, "join.html", pageData{AssetV: a.assetV, Code: code, Nick: nick, Error: msg})
	})
}

func (a *App) join(w http.ResponseWriter, r *http.Request, code, nick string, showErr func(string, int)) {
	id := ensurePlayerID(w, r)
	err := a.store.Mutate(r.Context(), code, func(tx *sql.Tx, rm *Room) error {
		exists, err := playerExists(r.Context(), tx, code, id)
		if err != nil {
			return err
		}
		if !exists {
			if n, err := countPlayers(r.Context(), tx, code); err != nil {
				return err
			} else if n >= maxPlayersPerRoom {
				return &httpError{http.StatusConflict, "That room is full"}
			}
		}
		return upsertPlayer(r.Context(), tx, code, Player{ID: id, Nickname: nick, Team: "none", Role: "operative"})
	})
	var he *httpError
	switch {
	case errors.Is(err, ErrRoomNotFound):
		showErr("No room with that code", http.StatusNotFound)
	case errors.As(err, &he):
		showErr(he.Msg, he.Status)
	case err != nil:
		a.fail(w, r, err)
	default:
		rememberNick(w, r, nick)
		http.Redirect(w, r, "/r/"+code, http.StatusSeeOther)
	}
}

func (a *App) room(w http.ResponseWriter, r *http.Request) {
	code := normCode(r.PathValue("code"))
	snap, err := a.store.Snapshot(r.Context(), code)
	if errors.Is(err, ErrRoomNotFound) {
		a.homeError(w, r, http.StatusNotFound, savedNick(r), "", "No room with that code")
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v := buildView(snap, playerID(r), a.assetV)
	if v == nil {
		a.render(w, http.StatusOK, "join.html", pageData{AssetV: a.assetV, Code: code, Nick: savedNick(r)})
		return
	}
	a.render(w, http.StatusOK, "room.html", v)
}

func (a *App) state(w http.ResponseWriter, r *http.Request) {
	code := normCode(r.PathValue("code"))
	pid := playerID(r)
	have, _ := strconv.ParseInt(r.URL.Query().Get("v"), 10, 64)
	ver, updated, err := a.store.RoomMeta(r.Context(), code)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if now()-updated > 600 { // keep rooms with watchers alive
		_ = a.store.Touch(r.Context(), code)
	}
	if ver == have && pid != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	a.fragment(w, r, code, pid)
}

// fragment renders the game fragment for this viewer.
func (a *App) fragment(w http.ResponseWriter, r *http.Request, code, pid string) {
	snap, err := a.store.Snapshot(r.Context(), code)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v := buildView(snap, pid, a.assetV)
	if v == nil { // not (or no longer) a member
		w.Header().Set("HX-Redirect", "/r/"+code)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	var buf bytes.Buffer
	if err := a.tmpl.ExecuteTemplate(&buf, "game", v); err != nil {
		log.Printf("render game: %v", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf.Bytes())
}

// ---------- actions ----------

type actionFunc func(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error

// act wraps an action: authenticates the player, runs fn in a write
// transaction (rules enforced here, server-side) and returns the new fragment.
func (a *App) act(fn actionFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := normCode(r.PathValue("code"))
		pid := playerID(r)
		if pid == "" {
			a.fail(w, r, forbidden("Join the room first"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			a.fail(w, r, badRequest("bad form"))
			return
		}
		err := a.store.Mutate(r.Context(), code, func(tx *sql.Tx, rm *Room) error {
			me, err := getPlayer(r.Context(), tx, code, pid)
			if err != nil {
				return err
			}
			if me == nil {
				return forbidden("Join the room first")
			}
			return fn(r.Context(), tx, rm, me, r)
		})
		if err != nil {
			a.fail(w, r, err)
			return
		}
		a.fragment(w, r, code, pid)
	}
}

func (a *App) doTeam(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error {
	team, role := r.PostFormValue("team"), r.PostFormValue("role")
	switch team {
	case "none":
		role = "operative"
	case "ember", "tide":
		if role != "operative" && role != "handler" {
			return badRequest("unknown role")
		}
	default:
		return badRequest("unknown team")
	}
	// The change itself is not a game event, but bump the version via Mutate.
	return setPlayerTeam(ctx, tx, rm.Code, me.ID, team, role)
}

func (a *App) doClue(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error {
	g := rm.Game
	if !me.IsHandler() {
		return forbidden("Only a handler can give clues")
	}
	team := Team(me.Team)
	if team != g.Turn && !g.Over() {
		return forbidden("It is not your team's turn")
	}
	n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("number")))
	if err != nil {
		return badRequest("Pick a number from 0 to 9")
	}
	if err := g.SubmitClue(team, r.PostFormValue("clue"), n); err != nil {
		return gameError(err)
	}
	return addLog(ctx, tx, rm, LogEntry{Kind: "clue", Team: team, Nickname: me.Nickname, Text: fmt.Sprintf("%s %d", g.Clue, g.Number)})
}

func (a *App) doReveal(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error {
	g := rm.Game
	if me.Team == "none" || me.IsHandler() {
		return forbidden("Only an operative on the active team can guess")
	}
	team := Team(me.Team)
	if team != g.Turn && !g.Over() {
		return forbidden("It is not your team's turn")
	}
	idx, err := strconv.Atoi(r.PostFormValue("card"))
	if err != nil {
		return badRequest("no such card")
	}
	res, err := g.Reveal(team, idx, me.Nickname)
	if err != nil {
		return gameError(err)
	}
	if err := addLog(ctx, tx, rm, LogEntry{Kind: "reveal", Team: team, Color: res.Card.Color, Nickname: me.Nickname, Text: strings.ToUpper(res.Card.Word)}); err != nil {
		return err
	}
	if res.GameOver {
		return addLog(ctx, tx, rm, LogEntry{Kind: "win", Team: g.Winner, Text: fmt.Sprintf("%s wins: %s", g.Winner.Label(), g.EndReason)})
	}
	return nil
}

func (a *App) doEnd(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error {
	g := rm.Game
	if me.Team == "none" || me.IsHandler() {
		return forbidden("Only an operative on the active team can end the turn")
	}
	team := Team(me.Team)
	if team != g.Turn && !g.Over() {
		return forbidden("It is not your team's turn")
	}
	if err := g.EndTurn(team); err != nil {
		return gameError(err)
	}
	return addLog(ctx, tx, rm, LogEntry{Kind: "end", Team: team, Nickname: me.Nickname, Text: "ended the turn"})
}

func (a *App) doNewRound(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error {
	rm.Game = newGame()
	rm.Round++
	if err := clearOldLog(ctx, tx, rm); err != nil {
		return err
	}
	return addLog(ctx, tx, rm, LogEntry{Kind: "round", Team: rm.Game.Starter, Nickname: me.Nickname,
		Text: fmt.Sprintf("dealt round %d — %s moves first", rm.Round, rm.Game.Starter.Label())})
}
