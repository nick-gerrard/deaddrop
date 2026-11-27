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
	"math"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

//go:embed templates/*.html static
var assets embed.FS

const (
	cookiePlayer       = "dd_pid"
	cookieNick         = "dd_nick"
	codeAlphabet       = "ABCDEFGHJKMNPQRSTUVWXYZ" // no I, L, O: easily confused
	codeLen            = 4
	maxNickLen         = 20
	maxHandlersPerTeam = 2
	createRateLimit    = 10
	createRateWindow   = 10 * time.Minute
	cspHeader          = "default-src 'self'; style-src 'self' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; script-src 'self'; img-src 'self' data:"
)

type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
	now    func() time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		hits:   make(map[string][]time.Time),
		limit:  limit,
		window: window,
		now:    time.Now,
	}
}

func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	cutoff := now.Add(-rl.window)
	ts := rl.hits[key]
	kept := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rl.limit {
		rl.hits[key] = kept
		return false
	}
	rl.hits[key] = append(kept, now)
	return true
}

func (rl *rateLimiter) Prune() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := rl.now().Add(-rl.window)
	for k, ts := range rl.hits {
		kept := ts[:0]
		for _, t := range ts {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(rl.hits, k)
		} else {
			rl.hits[k] = kept
		}
	}
}

func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if ip := strings.TrimSpace(strings.Split(xff, ",")[0]); ip != "" {
			return ip
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

type App struct {
	store         *Store
	tmpl          *template.Template
	assetV        string
	createLimiter *rateLimiter
	now           func() time.Time
	timerScale    float64
}

func (a *App) nowMs() int64 { return a.now().UnixMilli() }

func NewApp(store *Store) (*App, error) {
	t, err := template.ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	for _, f := range []string{"static/style.css", "static/htmx.min.js", "static/app.js", "static/favicon.svg"} {
		b, err := fs.ReadFile(assets, f)
		if err != nil {
			return nil, err
		}
		h.Write(b)
	}
	scale := 1.0
	if raw := os.Getenv("DEADDROP_TIMER_SCALE"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 && v <= 100 {
			scale = v
		}
	}
	return &App{
		store:         store,
		tmpl:          t,
		assetV:        hex.EncodeToString(h.Sum(nil))[:10],
		createLimiter: newRateLimiter(createRateLimit, createRateWindow),
		now:           time.Now,
		timerScale:    scale,
	}, nil
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "ok")
	})
	mux.Handle("GET /static/", cacheFor(http.FileServerFS(assets)))
	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("GET /lobby", a.lobby)
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
	mux.HandleFunc("POST /r/{code}/settings", a.actWithLimit(maxSettingsBody, a.doSettings))
	mux.HandleFunc("POST /r/{code}/series-reset", a.act(a.doSeriesReset))
	mux.HandleFunc("POST /r/{code}/mark", a.act(a.doMark))
	return secureHeaders(csrfProtect(mux))
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

func csrfProtect(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && !sameOriginPOST(r) {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func sameOriginPOST(r *http.Request) bool {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" {
		return sfs == "same-origin" || sfs == "none"
	}
	orig := r.Header.Get("Origin")
	if orig == "" {
		return false
	}
	u, err := url.Parse(orig)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
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
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
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
	w.Header().Set("Content-Security-Policy", cspHeader)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	a.render(w, http.StatusOK, "home.html", pageData{AssetV: a.assetV, Nick: savedNick(r)})
}

const (
	lobbyMaxAge = 30 * time.Minute
	lobbyLimit  = 20
)

func (a *App) lobby(w http.ResponseWriter, r *http.Request) {
	nowSec := a.now().Unix()
	since := nowSec - int64(lobbyMaxAge/time.Second)
	rooms, err := a.store.ListPublicRooms(r.Context(), since, lobbyLimit)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v := buildLobbyView(rooms, nowSec)
	if have := r.URL.Query().Get("v"); have != "" && have == v.Version {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	a.render(w, http.StatusOK, "lobby", v)
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
	if !a.createLimiter.Allow(clientIP(r)) {
		a.homeError(w, r, http.StatusTooManyRequests, nick, "", "Too many rooms created recently. Please wait a few minutes.")
		return
	}
	pubVal := r.FormValue("public")
	public := pubVal == "1" || pubVal == "on" || pubVal == "true"
	id := ensurePlayerID(w, r)
	for range 10 {
		code := newCode()
		err := a.store.CreateRoom(r.Context(), code, newGame(), Player{ID: id, Nickname: nick, Team: "none", Role: "operative"}, public)
		if isUniqueViolation(err) {
			continue
		}
		if errors.Is(err, ErrTooManyRooms) {
			a.homeError(w, r, http.StatusServiceUnavailable, nick, "", "Too many active rooms right now. Please try again later.")
			return
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
	a.expireTurn(r.Context(), code)
	snap, err := a.store.Snapshot(r.Context(), code)
	if errors.Is(err, ErrRoomNotFound) {
		a.homeError(w, r, http.StatusNotFound, savedNick(r), "", "No room with that code")
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v := buildView(snap, playerID(r), a.assetV, a.nowMs())
	if v == nil {
		a.render(w, http.StatusOK, "join.html", pageData{AssetV: a.assetV, Code: code, Nick: savedNick(r)})
		return
	}
	a.render(w, http.StatusOK, "room.html", v)
}

func (a *App) state(w http.ResponseWriter, r *http.Request) {
	code := normCode(r.PathValue("code"))
	a.expireTurn(r.Context(), code)
	pid := playerID(r)
	have, _ := strconv.ParseInt(r.URL.Query().Get("v"), 10, 64)
	ver, updated, isMember, err := a.store.RoomMeta(r.Context(), code, pid)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if isMember && now()-updated > 600 { // keep rooms with watchers alive
		_ = a.store.Touch(r.Context(), code)
	}
	if ver == have && isMember {
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
	v := buildView(snap, pid, a.assetV, a.nowMs())
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
	w.Header().Set("Content-Security-Policy", cspHeader)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf.Bytes())
}

// ---------- actions ----------

type actionFunc func(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error

// act wraps an action: authenticates the player, runs fn in a write
// transaction (rules enforced here, server-side) and returns the new fragment.
func (a *App) act(fn actionFunc) http.HandlerFunc {
	return a.actWithLimit(4096, fn)
}

func (a *App) actWithLimit(maxBody int64, fn actionFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := normCode(r.PathValue("code"))
		pid := playerID(r)
		if pid == "" {
			a.fail(w, r, forbidden("Join the room first"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		if err := r.ParseForm(); err != nil {
			a.fail(w, r, badRequest("bad form"))
			return
		}
		a.expireTurn(r.Context(), code)
		err := a.store.Mutate(r.Context(), code, func(tx *sql.Tx, rm *Room) error {
			me, err := getPlayer(r.Context(), tx, code, pid)
			if err != nil {
				return err
			}
			if me == nil {
				return forbidden("Join the room first")
			}
			before := keyOf(rm)
			if err := fn(r.Context(), tx, rm, me, r); err != nil {
				return err
			}
			return a.afterAction(r.Context(), tx, rm, before)
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
	if role == "handler" && !(me.Team == team && me.Role == "handler") {
		handlers, err := countTeamHandlers(ctx, tx, rm.Code, team)
		if err != nil {
			return err
		}
		if handlers >= maxHandlersPerTeam {
			return &httpError{http.StatusConflict, "That team already has 2 handlers"}
		}
		if rm.Game.InProgress() && handlers > 0 {
			return &httpError{http.StatusConflict, "Can't become handler mid-round"}
		}
	}
	if me.Team != team || me.Role != role {
		if err := clearPlayerMarks(ctx, tx, rm.Code, me.ID); err != nil {
			return err
		}
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
	rm.IdleTimeouts = 0
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
	rm.IdleTimeouts = 0
	if rm.TimerSecs > 0 && rm.Deadline == 0 && !g.Over() {
		a.restartTimer(rm)
	}
	if err := clearCardMarks(ctx, tx, rm.Code, idx); err != nil {
		return err
	}
	if err := addLog(ctx, tx, rm, LogEntry{Kind: "reveal", Team: team, Color: res.Card.Color, Nickname: me.Nickname, Text: strings.ToUpper(res.Card.Word)}); err != nil {
		return err
	}
	if res.GameOver {
		if g.Winner == Tide {
			rm.TideWins++
		} else {
			rm.EmberWins++
		}
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
	if me.Team == "none" {
		return forbidden("Join a team to deal a new round")
	}
	rm.Game = a.newGameFor(rm)
	rm.Round++
	rm.IdleTimeouts = 0
	if err := clearOldLog(ctx, tx, rm); err != nil {
		return err
	}
	return addLog(ctx, tx, rm, LogEntry{Kind: "round", Team: rm.Game.Starter, Nickname: me.Nickname,
		Text: fmt.Sprintf("dealt round %d — %s moves first", rm.Round, rm.Game.Starter.Label())})
}
