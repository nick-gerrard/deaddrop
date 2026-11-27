package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"
)

const (
	lobbyMaxAge = 30 * time.Minute
	lobbyLimit  = 20
)

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	a.render(w, http.StatusOK, "home.html", pageData{AssetV: a.assetV, Nick: savedNick(r)})
}

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
