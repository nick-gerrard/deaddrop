package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

const (
	cookiePlayer = "dd_pid"
	cookieNick   = "dd_nick"
	codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ" // no I, L, O: easily confused
	codeLen      = 4
	maxNickLen   = 20
	cspHeader    = "default-src 'self'; style-src 'self' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; script-src 'self'; img-src 'self' data:"
)

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

// ---------- rendering & actions ----------

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
