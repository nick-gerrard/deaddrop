package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode"
)

// Room settings: turn timer and word pack. Any team member may change them.

const (
	PackClassic = "classic"
	PackHard    = "hard"
	PackMixed   = "mixed"
	PackCustom  = "custom"

	minCustomWords   = 25
	maxCustomWords   = 400
	maxCustomWordLen = 20
	maxSettingsBody  = 16 << 10
	maxIdleTimeouts  = 3
)

var timerChoices = []int{0, 60, 90, 120, 180}

type packChoice struct{ Value, Label string }

var packChoices = []packChoice{
	{PackClassic, "Classic"}, {PackHard, "Hard"}, {PackMixed, "Mixed"}, {PackCustom, "Custom"},
}

func packLabel(p string) string {
	for _, c := range packChoices {
		if c.Value == p {
			return c.Label
		}
	}
	return "Classic"
}

func validPack(p string) bool {
	for _, c := range packChoices {
		if c.Value == p {
			return true
		}
	}
	return false
}

func validTimer(n int) bool {
	for _, c := range timerChoices {
		if c == n {
			return true
		}
	}
	return false
}

func timerLabel(secs int) string {
	if secs == 0 {
		return "off"
	}
	return strconv.Itoa(secs) + "s"
}

// parseCustomWords turns the free-text custom pack into a clean, de-duplicated,
// lower-case list. Words are separated by commas or newlines.
func parseCustomWords(raw string) ([]string, error) {
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
	seen := map[string]bool{}
	var out []string
	for _, p := range parts {
		w := strings.ToLower(strings.Join(strings.Fields(p), " "))
		if w == "" {
			continue
		}
		if n := len([]rune(w)); n < 1 || n > maxCustomWordLen {
			return nil, fmt.Errorf("Custom words must be 1-%d characters: %q is too long", maxCustomWordLen, shorten(w))
		}
		hasLetter := false
		for _, r := range w {
			switch {
			case unicode.IsLetter(r):
				hasLetter = true
			case r == ' ' || r == '-':
			default:
				return nil, fmt.Errorf("Custom words may only use letters, spaces and hyphens: %q", shorten(w))
			}
		}
		if !hasLetter {
			return nil, fmt.Errorf("Custom words need at least one letter: %q", shorten(w))
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	if len(out) > maxCustomWords {
		return nil, fmt.Errorf("Too many custom words (%d); the maximum is %d", len(out), maxCustomWords)
	}
	if len(out) < minCustomWords {
		return nil, fmt.Errorf("Need at least %d different custom words (got %d)", minCustomWords, len(out))
	}
	return out, nil
}

func shorten(s string) string {
	if r := []rune(s); len(r) > 24 {
		return string(r[:24]) + "…"
	}
	return s
}

// wordsFor returns the word pool for a pack. Unknown packs and unusable custom
// lists fall back to the classic list so a board can always be dealt.
func wordsFor(pack, custom string) []string {
	switch pack {
	case PackHard:
		return hardWordList
	case PackMixed:
		seen := make(map[string]bool, len(wordList)+len(hardWordList))
		out := make([]string, 0, len(wordList)+len(hardWordList))
		for _, list := range [][]string{wordList, hardWordList} {
			for _, w := range list {
				if !seen[w] {
					seen[w] = true
					out = append(out, w)
				}
			}
		}
		return out
	case PackCustom:
		if words, err := parseCustomWords(custom); err == nil {
			return words
		}
	}
	return wordList
}

func (a *App) newGameFor(rm *Room) *Game { return NewGame(newRNG(), wordsFor(rm.Pack, rm.Custom)) }

func (a *App) doSettings(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error {
	if me.Team == "none" {
		return forbidden("Join a team to change mission settings")
	}
	_, hasCustom := r.PostForm["custom"]
	if rm.Phase == PhasePlaying {
		if r.PostFormValue("timer") != "" || r.PostFormValue("pack") != "" || hasCustom {
			return &httpError{http.StatusConflict, "Settings are locked during a mission"}
		}
	}
	var changes []string

	if v := r.PostFormValue("timer"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || !validTimer(n) {
			return badRequest("Pick a turn timer from the list")
		}
		if n != rm.TimerSecs {
			wasOff := rm.TimerSecs == 0
			rm.TimerSecs = n
			switch {
			case n == 0:
				rm.Deadline = 0
				rm.IdleTimeouts = 0
			case wasOff:
				rm.IdleTimeouts = 0
				a.restartTimer(rm)
			}
			changes = append(changes, "turn timer "+timerLabel(n))
		}
	}

	if pack := r.PostFormValue("pack"); pack != "" {
		if !validPack(pack) {
			return badRequest("Pick a word pack from the list")
		}
		custom := rm.Custom
		if hasCustom {
			raw := r.PostFormValue("custom")
			words, err := parseCustomWords(raw)
			switch {
			case err == nil:
				custom = strings.Join(words, "\n")
			case pack == PackCustom:
				return badRequest(err.Error())
			}
		}
		if pack == PackCustom {
			if _, err := parseCustomWords(custom); err != nil {
				return badRequest(err.Error())
			}
		}
		if pack != rm.Pack || (pack == PackCustom && custom != rm.Custom) {
			changes = append(changes, "word pack "+packLabel(pack))
		}
		rm.Pack, rm.Custom = pack, custom
	}

	if vals, ok := r.PostForm["public"]; ok {
		pub := false
		for _, v := range vals {
			switch v {
			case "1", "true", "on":
				pub = true
			case "0", "false", "":
			default:
				return badRequest("Invalid lobby visibility")
			}
		}
		if pub != rm.Public {
			rm.Public = pub
			if pub {
				changes = append(changes, "room listed in public lobby")
			} else {
				changes = append(changes, "room set to private")
			}
		}
	}

	if len(changes) == 0 {
		return errNoChange
	}
	return addLog(ctx, tx, rm, LogEntry{Kind: "settings", Nickname: me.Nickname, Text: "changed settings: " + strings.Join(changes, "; ")})
}

func (a *App) doSeriesReset(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error {
	if me.Team == "none" {
		return forbidden("Join a team to reset the series score")
	}
	if rm.EmberWins == 0 && rm.TideWins == 0 {
		return errNoChange
	}
	rm.EmberWins, rm.TideWins = 0, 0
	return addLog(ctx, tx, rm, LogEntry{Kind: "settings", Nickname: me.Nickname, Text: "reset the series score to 0 – 0"})
}

// doMark toggles the caller's suggestion mark on an unrevealed card. Only
// operatives on the guessing team may mark.
func (a *App) doMark(ctx context.Context, tx *sql.Tx, rm *Room, me *Player, r *http.Request) error {
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

// ---------- turn timer ----------

// restartTimer (re)starts the turn deadline for the room's current phase, or
// clears it when the timer is off, in setup, or the round is over.
func (a *App) restartTimer(rm *Room) {
	if rm.Phase != PhasePlaying || rm.TimerSecs <= 0 || rm.Game.Over() {
		rm.Deadline = 0
		return
	}
	ms := float64(rm.TimerSecs) * 1000 * a.timerScale
	rm.Deadline = a.nowMs() + int64(ms)
}

type turnKey struct {
	phase   string
	round   int
	turn    Team
	hasClue bool
	over    bool
}

func keyOf(rm *Room) turnKey {
	return turnKey{rm.Phase, rm.Round, rm.Game.Turn, rm.Game.HasClue, rm.Game.Over()}
}

// afterAction runs inside the action's transaction. Whenever the turn phase
// changed (clue given, turn passed, round dealt, game over) it restarts the
// timer and clears operative marks.
func (a *App) afterAction(ctx context.Context, tx *sql.Tx, rm *Room, before turnKey) error {
	if keyOf(rm) == before {
		return nil
	}
	a.restartTimer(rm)
	return clearMarks(ctx, tx, rm.Code)
}

// expireTurn ends the turn if the room's deadline has passed. It is called on
// every state fetch and action, so enforcement is server-side only.
func (a *App) expireTurn(ctx context.Context, code string) {
	dl, err := a.store.Deadline(ctx, code)
	if err != nil || dl == 0 || a.nowMs() <= dl {
		return
	}
	err = a.store.MutateNoTouch(ctx, code, func(tx *sql.Tx, rm *Room) error {
		if rm.Phase != PhasePlaying || rm.Deadline == 0 || a.nowMs() <= rm.Deadline || rm.Game.Over() {
			return errNoChange
		}
		team := rm.Game.Turn
		if err := rm.Game.TimeOut(); err != nil {
			return errNoChange
		}
		if err := addLog(ctx, tx, rm, LogEntry{Kind: "end", Team: team, Text: "time ran out — turn passes"}); err != nil {
			return err
		}
		rm.IdleTimeouts++
		if rm.IdleTimeouts >= maxIdleTimeouts {
			rm.Deadline = 0
			if err := addLog(ctx, tx, rm, LogEntry{Kind: "settings", Text: "Timer paused — nobody's moving"}); err != nil {
				return err
			}
		} else {
			a.restartTimer(rm)
		}
		return clearMarks(ctx, tx, rm.Code)
	})
	if err != nil && err != ErrRoomNotFound {
		log.Printf("expire turn %s: %v", code, err)
	}
}
