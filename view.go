package main

import (
	"fmt"
	"strconv"
	"strings"
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

type MarkView struct {
	Initials string
	Nickname string
	Mine     bool
}

type CardView struct {
	Index     int
	Word      string
	Class     string
	Label     string // accessible description
	Clickable bool
	By        string
	Marks     []MarkView
	MyMark    bool
}

type Banner struct {
	Class    string // team css class or "over"
	Head     string
	ClueWord string
	ClueNum  string
	Detail   string
}

type TimerChoiceView struct {
	Secs     int
	Label    string
	Selected bool
}

type PackChoiceView struct {
	Value    string
	Label    string
	Selected bool
}

type RecapGuess struct {
	Word  string
	Color Color
}

type RecapTurn struct {
	Clue    string
	Guesses []RecapGuess
}

type TeamRecap struct {
	Team       Team
	Label      string
	Turns      []RecapTurn
	Unrevealed []string
}

type RoomView struct {
	Code    string
	Version int64
	Round   int
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

	CanClue     bool
	CanGuess    bool
	CanEnd      bool
	CanSettings bool
	YourTurn    bool
	TurnPhase   string
	Hint        string
	Winner      TeamView

	// Turn timer
	Deadline     int64
	NowMs        int64
	TimerSecs    int
	TimerText    string
	TimerLow     bool
	TimerChoices []TimerChoiceView

	// Word pack & settings
	Pack        string
	PackLabel   string
	PackChoices []PackChoiceView
	CustomWords string

	// Series scoreboard
	EmberWins int
	TideWins  int

	Public bool

	// Round recap (populated when Over is true)
	Recap [2]TeamRecap
}

func plural(n int, one, many string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + one
	}
	return strconv.Itoa(n) + " " + many
}

func initials(nick string) string {
	fields := strings.Fields(nick)
	if len(fields) >= 2 {
		r1 := []rune(fields[0])
		r2 := []rune(fields[1])
		return strings.ToUpper(string([]rune{r1[0], r2[0]}))
	}
	if len(fields) == 1 {
		r := []rune(fields[0])
		if len(r) >= 2 {
			return strings.ToUpper(string(r[:2]))
		}
		return strings.ToUpper(string(r))
	}
	return "?"
}

func formatCountdown(remMs int64) (text string, low bool) {
	secs := int((remMs + 999) / 1000)
	if secs < 0 {
		secs = 0
	}
	return fmt.Sprintf("%d:%02d", secs/60, secs%60), secs <= 10
}

func buildRecap(g *Game, logEntries []LogEntry) [2]TeamRecap {
	recap := [2]TeamRecap{
		{Team: Ember, Label: Ember.Label()},
		{Team: Tide, Label: Tide.Label()},
	}
	idxOf := func(t Team) int {
		if t == Tide {
			return 1
		}
		return 0
	}
	// logEntries is newest-first; iterate backwards for chronological order.
	for i := len(logEntries) - 1; i >= 0; i-- {
		e := logEntries[i]
		if !e.Team.Valid() {
			continue
		}
		tr := &recap[idxOf(e.Team)]
		switch e.Kind {
		case "clue":
			tr.Turns = append(tr.Turns, RecapTurn{Clue: e.Text})
		case "reveal":
			if len(tr.Turns) == 0 {
				tr.Turns = append(tr.Turns, RecapTurn{Clue: "—"})
			}
			last := &tr.Turns[len(tr.Turns)-1]
			last.Guesses = append(last.Guesses, RecapGuess{Word: e.Text, Color: e.Color})
		}
	}
	for _, c := range g.Cards {
		if c.Revealed {
			continue
		}
		switch c.Color {
		case ColorEmber:
			recap[0].Unrevealed = append(recap[0].Unrevealed, strings.ToUpper(c.Word))
		case ColorTide:
			recap[1].Unrevealed = append(recap[1].Unrevealed, strings.ToUpper(c.Word))
		}
	}
	return recap
}

// buildView renders the per-viewer state. It returns nil if pid is not a
// player in the room. Unrevealed colours are only included for handlers and
// finished games.
func buildView(s *Snapshot, pid, assetV string, nowMs int64) *RoomView {
	var me *Player
	playersByID := make(map[string]Player, len(s.Players))
	for i := range s.Players {
		playersByID[s.Players[i].ID] = s.Players[i]
		if pid != "" && s.Players[i].ID == pid {
			me = &s.Players[i]
		}
	}
	if me == nil {
		return nil
	}
	g := s.Room.Game
	pack := s.Room.Pack
	if !validPack(pack) {
		pack = PackClassic
	}
	v := &RoomView{
		Code: s.Room.Code, Version: s.Room.Version, Round: s.Room.Round, AssetV: assetV,
		MeName: me.Nickname, MeTeam: me.Team, MeRole: me.Role,
		Game: g, Over: g.Over(), Log: s.Log,
		NowMs:       nowMs,
		TimerSecs:   s.Room.TimerSecs,
		Pack:        pack,
		PackLabel:   packLabel(pack),
		CustomWords: s.Room.Custom,
		EmberWins:   s.Room.EmberWins,
		TideWins:    s.Room.TideWins,
		Public:      s.Room.Public,
	}
	if !g.Over() && s.Room.Deadline > 0 {
		v.Deadline = s.Room.Deadline
		v.TimerText, v.TimerLow = formatCountdown(s.Room.Deadline - nowMs)
	}
	for _, secs := range timerChoices {
		lbl := "Off"
		if secs > 0 {
			lbl = strconv.Itoa(secs) + "s"
		}
		v.TimerChoices = append(v.TimerChoices, TimerChoiceView{
			Secs:     secs,
			Label:    lbl,
			Selected: secs == s.Room.TimerSecs,
		})
	}
	for _, pc := range packChoices {
		v.PackChoices = append(v.PackChoices, PackChoiceView{
			Value:    pc.Value,
			Label:    pc.Label,
			Selected: pc.Value == pack,
		})
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
	v.CanSettings = me.Team != "none"
	v.YourTurn = v.CanClue || v.CanGuess
	switch {
	case g.Over():
		v.TurnPhase = "over"
	case g.HasClue:
		v.TurnPhase = string(g.Turn) + "-guess"
	default:
		v.TurnPhase = string(g.Turn) + "-clue"
	}

	marksByCard := make(map[int][]MarkView)
	myMarks := make(map[int]bool)
	if me.Team != "none" {
		for _, m := range s.Marks {
			p, ok := playersByID[m.PlayerID]
			if !ok || p.Team != me.Team {
				continue
			}
			mine := m.PlayerID == pid
			if mine {
				myMarks[m.Card] = true
			}
			marksByCard[m.Card] = append(marksByCard[m.Card], MarkView{
				Initials: initials(p.Nickname),
				Nickname: p.Nickname,
				Mine:     mine,
			})
		}
	}

	for i, c := range g.Cards {
		cv := CardView{Index: i, Word: c.Word, Class: "card", By: c.By}
		switch {
		case c.Revealed:
			cv.Class += " revealed c-" + string(c.Color)
			cv.Label = fmt.Sprintf("%s, revealed: %s", c.Word, c.Color.Label())
		case v.ShowKey:
			cv.Class += " key k-" + string(c.Color)
			cv.Label = fmt.Sprintf("%s: %s", c.Word, c.Color.Label())
			cv.Marks = marksByCard[i]
		default:
			cv.Label = c.Word
			cv.Clickable = v.CanGuess
			cv.Marks = marksByCard[i]
			cv.MyMark = myMarks[i]
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
		v.Recap = buildRecap(g, s.Log)
	case !g.HasClue:
		v.Banner = Banner{Class: string(g.Turn), Head: fmt.Sprintf("%s handlers are preparing a clue", g.Turn.Label())}
		switch {
		case v.CanClue:
			// Clue form below the board is self-explanatory.
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
			v.Hint = "Tap a card to reveal or mark it for your team."
		case myTeam == g.Turn && me.IsHandler():
			v.Hint = "Your operatives are guessing."
		}
	}
	return v
}

type LobbyItemView struct {
	Code       string
	EmberCount int
	TideCount  int
	Status     string
	Ago        string
}

type LobbyView struct {
	Rooms []LobbyItemView
}

func lobbyStatus(round int, g *Game) string {
	if g.Over() {
		return fmt.Sprintf("Round %d finished", round)
	}
	if g.InProgress() || round > 1 {
		return fmt.Sprintf("In progress round %d", round)
	}
	return "Waiting for players"
}

func formatAgo(nowSec, updatedSec int64) string {
	d := nowSec - updatedSec
	if d < 10 {
		return "just now"
	}
	if d < 60 {
		return fmt.Sprintf("%ds ago", d)
	}
	return fmt.Sprintf("%dm ago", d/60)
}

func buildLobbyView(rooms []LobbyRoom, nowSec int64) LobbyView {
	v := LobbyView{Rooms: make([]LobbyItemView, 0, len(rooms))}
	for _, r := range rooms {
		v.Rooms = append(v.Rooms, LobbyItemView{
			Code:       r.Code,
			EmberCount: r.EmberCount,
			TideCount:  r.TideCount,
			Status:     lobbyStatus(r.Round, r.Game),
			Ago:        formatAgo(nowSec, r.UpdatedAt),
		})
	}
	return v
}
