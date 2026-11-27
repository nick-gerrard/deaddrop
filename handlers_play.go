package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func (a *App) doClue(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error {
	// Turn and role checks mirror CanClue in view.go and SubmitClue in game.go.
	if rm.Phase != PhasePlaying {
		return &httpError{http.StatusConflict, "Mission hasn't started yet"}
	}
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
		return badRequest("Pick a number from 1 to 9")
	}
	if err := g.SubmitClue(team, r.PostFormValue("clue"), n); err != nil {
		return gameError(err)
	}
	rm.IdleTimeouts = 0
	return addLog(ctx, tx, rm, LogEntry{Kind: "clue", Team: team, Nickname: me.Nickname, Text: fmt.Sprintf("%s %d", g.Clue, g.Number)})
}

func (a *App) doReveal(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error {
	// Turn and role checks mirror CanGuess in view.go and Reveal in game.go.
	if rm.Phase != PhasePlaying {
		return &httpError{http.StatusConflict, "Mission hasn't started yet"}
	}
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

// doMark toggles the caller's suggestion mark on an unrevealed card. Only
// operatives on the guessing team may mark (visibility filtered in view.go).
func (a *App) doMark(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error {
	// Turn and role checks mirror CanGuess in view.go.
	if rm.Phase != PhasePlaying {
		return &httpError{http.StatusConflict, "Mission hasn't started yet"}
	}
	g := rm.Game
	if me.Team == "none" || me.IsHandler() {
		return forbidden("Only operatives on the guessing team can mark cards")
	}
	if Team(me.Team) != g.Turn && !g.Over() {
		return forbidden("It is not your team's turn")
	}
	if g.Over() {
		return gameError(ErrGameOver)
	}
	if !g.HasClue {
		return gameError(ErrNoClue)
	}
	idx, err := strconv.Atoi(r.PostFormValue("card"))
	if err != nil || idx < 0 || idx >= len(g.Cards) {
		return badRequest("no such card")
	}
	if g.Cards[idx].Revealed {
		return gameError(ErrAlreadyRevealed)
	}
	_, err = toggleMark(ctx, tx, rm.Code, idx, me.ID)
	return err
}

func (a *App) doEnd(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error {
	// Turn and role checks mirror CanEnd in view.go and EndTurn in game.go.
	if rm.Phase != PhasePlaying {
		return &httpError{http.StatusConflict, "Mission hasn't started yet"}
	}
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
