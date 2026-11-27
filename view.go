package main

import (
	"fmt"
	"strconv"
)

type PlayerView struct {
	Nickname string
	You      bool
}

type TeamView struct {
	Team       Team
	Label      string
	Remaining  int
	Total      int
	Handlers   []PlayerView
	Operatives []PlayerView
	Mine       bool // the viewer is on this team
}

type CardView struct {
	Index     int
	Word      string
	Class     string
	Label     string // accessible description
	Clickable bool
	By        string
}

type Banner struct {
	Class    string // team css class or "over"
	Head     string
	ClueWord string
	ClueNum  string
	Detail   string
}

type RoomView struct {
	Code    string
	Version int64
	AssetV  string
	MeName  string
	MeTeam  string
	MeRole  string
	Role    string // "ember handler", "spectator", ...
	Game    *Game
	Over    bool
	Banner  Banner
	Cards   []CardView
	Teams   [2]TeamView
	Specs   []PlayerView
	Log     []LogEntry
	ShowKey bool

	CanClue  bool
	CanGuess bool
	CanEnd   bool
	Hint     string
	Winner   TeamView
}

func plural(n int, one, many string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + one
	}
	return strconv.Itoa(n) + " " + many
}

// buildView renders the per-viewer state. It returns nil if pid is not a
// player in the room. Unrevealed colours are only included for handlers and
// finished games.
func buildView(s *Snapshot, pid, assetV string) *RoomView {
	var me *Player
	for i := range s.Players {
		if pid != "" && s.Players[i].ID == pid {
			me = &s.Players[i]
		}
	}
	if me == nil {
		return nil
	}
	g := s.Room.Game
	v := &RoomView{
		Code: s.Room.Code, Version: s.Room.Version, AssetV: assetV,
		MeName: me.Nickname, MeTeam: me.Team, MeRole: me.Role,
		Game: g, Over: g.Over(), Log: s.Log,
	}
	v.ShowKey = me.IsHandler() || g.Over()
	myTeam := Team(me.Team)
	switch {
	case me.Team == "none":
		v.Role = "spectator"
	default:
		v.Role = fmt.Sprintf("%s %s", myTeam.Label(), me.Role)
	}

	v.CanClue = !g.Over() && !g.HasClue && me.IsHandler() && myTeam == g.Turn
	v.CanGuess = !g.Over() && g.HasClue && me.Team != "none" && !me.IsHandler() && myTeam == g.Turn
	v.CanEnd = v.CanGuess && g.Guesses >= 1

	for i, c := range g.Cards {
		cv := CardView{Index: i, Word: c.Word, Class: "card", By: c.By}
		switch {
		case c.Revealed:
			cv.Class += " revealed c-" + string(c.Color)
			cv.Label = fmt.Sprintf("%s, revealed: %s", c.Word, c.Color.Label())
		case v.ShowKey:
			cv.Class += " key k-" + string(c.Color)
			cv.Label = fmt.Sprintf("%s: %s", c.Word, c.Color.Label())
		default:
			cv.Label = c.Word
			cv.Clickable = v.CanGuess
		}
		v.Cards = append(v.Cards, cv)
	}

	for i, t := range []Team{Ember, Tide} {
		tv := TeamView{Team: t, Label: t.Label(), Remaining: g.Remaining(t), Mine: myTeam == t}
		tv.Total = 8
		if g.Starter == t {
			tv.Total = 9
		}
		v.Teams[i] = tv
	}
	for _, p := range s.Players {
		pv := PlayerView{Nickname: p.Nickname, You: p.ID == pid}
		switch {
		case p.Team == "none":
			v.Specs = append(v.Specs, pv)
		default:
			tv := &v.Teams[0]
			if p.Team == "tide" {
				tv = &v.Teams[1]
			}
			if p.Role == "handler" {
				tv.Handlers = append(tv.Handlers, pv)
			} else {
				tv.Operatives = append(tv.Operatives, pv)
			}
		}
	}

	switch {
	case g.Over():
		v.Banner = Banner{Class: "over", Head: fmt.Sprintf("%s WINS — %s", g.Winner.Label(), g.EndReason)}
		if g.Winner == Tide {
			v.Winner = v.Teams[1]
		} else {
			v.Winner = v.Teams[0]
		}
		v.Hint = "Round over. Deal a new round to play again."
	case !g.HasClue:
		v.Banner = Banner{Class: string(g.Turn), Head: fmt.Sprintf("%s handlers are preparing a clue", g.Turn.Label())}
		switch {
		case v.CanClue:
			v.Hint = "You're on the clock: give your operatives a one-word clue and a number."
		case me.IsHandler():
			v.Hint = "Waiting for the other team to move."
		case myTeam == g.Turn:
			v.Hint = "Waiting for your handler's clue."
		}
	default:
		left, unlimited := g.GuessesLeft()
		detail := "unlimited guesses"
		if !unlimited {
			detail = plural(left, "guess left", "guesses left")
		}
		v.Banner = Banner{Class: string(g.Turn), Head: fmt.Sprintf("%s operatives are guessing", g.Turn.Label()),
			ClueWord: g.Clue, ClueNum: strconv.Itoa(g.Number), Detail: detail}
		switch {
		case v.CanGuess:
			v.Hint = "Pick the cards you think match the clue."
		case myTeam == g.Turn && me.IsHandler():
			v.Hint = "Your operatives are guessing."
		}
	}
	return v
}
