package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
)

const maxHandlersPerTeam = 1

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
	// Phase lock also reflected in templates/game.html (team controls shown only in setup, except spectator join).
	if rm.Phase == PhasePlaying {
		if me.Team != "none" {
			return &httpError{http.StatusConflict, "Roles are locked during a mission"}
		}
		if team == "none" {
			return errNoChange
		}
		if role == "handler" {
			return &httpError{http.StatusConflict, "Can't become Handler during a mission"}
		}
	} else {
		if me.Team == team && me.Role == role {
			return errNoChange
		}
		if role == "handler" {
			handlers, err := countTeamHandlers(ctx, tx, rm.Code, team)
			if err != nil {
				return err
			}
			// Handler cap also checked in doStart, view.go (buildView), and templates/game.html ("Be Handler" disabled).
			if handlers >= maxHandlersPerTeam {
				return &httpError{http.StatusConflict, "That team already has a Handler"}
			}
		}
	}
	if me.Team != team || me.Role != role {
		if err := clearPlayerMarks(ctx, tx, rm.Code, me.ID); err != nil {
			return err
		}
	}
	return setPlayerTeam(ctx, tx, rm.Code, me.ID, team, role)
}

func (a *App) doStart(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error {
	if me.Team == "none" {
		return forbidden("Join a team to start the mission")
	}
	if rm.Phase != PhaseSetup {
		return &httpError{http.StatusConflict, "Mission is already in progress"}
	}
	// Mirror of start-readiness check in view.go (buildView setup block).
	for _, t := range []Team{Ember, Tide} {
		handlers, err := countTeamHandlers(ctx, tx, rm.Code, string(t))
		if err != nil {
			return err
		}
		if handlers == 0 {
			return &httpError{http.StatusConflict, t.Label() + " needs a Handler"}
		}
		if handlers > maxHandlersPerTeam {
			return &httpError{http.StatusConflict, t.Label() + " can only have one Handler"}
		}
		operatives, err := countTeamOperatives(ctx, tx, rm.Code, string(t))
		if err != nil {
			return err
		}
		if operatives < 1 {
			return &httpError{http.StatusConflict, t.Label() + " needs an operative"}
		}
	}
	rm.Game = a.newGameFor(rm)
	rm.Phase = PhasePlaying
	rm.IdleTimeouts = 0
	if err := clearOldLog(ctx, tx, rm); err != nil {
		return err
	}
	return addLog(ctx, tx, rm, LogEntry{Kind: "round", Team: rm.Game.Starter, Nickname: me.Nickname,
		Text: fmt.Sprintf("started round %d — %s moves first", rm.Round, rm.Game.Starter.Label())})
}

func (a *App) doAbort(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error {
	// Abort button gated by MeTeam and Over in templates/game.html.
	if me.Team == "none" {
		return forbidden("Join a team to abort the mission")
	}
	if rm.Phase != PhasePlaying || rm.Game.Over() {
		return &httpError{http.StatusConflict, "No mission in progress"}
	}
	rm.Phase = PhaseSetup
	rm.Deadline = 0
	rm.IdleTimeouts = 0
	return clearRoundLog(ctx, tx, rm)
}

func (a *App) doNewRound(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error {
	// Next round button gated by MeTeam and Over in templates/game.html.
	if me.Team == "none" {
		return forbidden("Join a team to start the next round")
	}
	if rm.Phase != PhasePlaying {
		return &httpError{http.StatusConflict, "Already in setup"}
	}
	if !rm.Game.Over() {
		return &httpError{http.StatusConflict, "Mission in progress — abort it first"}
	}
	rm.Phase = PhaseSetup
	rm.Round++
	rm.Deadline = 0
	rm.IdleTimeouts = 0
	return clearOldLog(ctx, tx, rm)
}
