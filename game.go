package main

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"unicode"
)

// Pure game logic: no DB, no HTTP.

const (
	BoardSize  = 25
	MaxClueNum = 9
	maxClueLen = 24
)

type Team string

const (
	Ember Team = "ember"
	Tide  Team = "tide"
)

func (t Team) Other() Team {
	if t == Ember {
		return Tide
	}
	return Ember
}

func (t Team) Valid() bool { return t == Ember || t == Tide }

func (t Team) Label() string { return strings.ToUpper(string(t)) }

func (t Team) Color() Color { return Color(t) }

type Color string

const (
	ColorEmber     Color = "ember"
	ColorTide      Color = "tide"
	ColorBystander Color = "bystander"
	ColorBurned    Color = "burned"
)

// Label is a human readable name for a card colour.
func (c Color) Label() string {
	switch c {
	case ColorEmber:
		return "EMBER agent"
	case ColorTide:
		return "TIDE agent"
	case ColorBystander:
		return "bystander"
	case ColorBurned:
		return "BURNED"
	}
	return string(c)
}

type Card struct {
	Word     string `json:"w"`
	Color    Color  `json:"c"`
	Revealed bool   `json:"r,omitempty"`
	By       string `json:"by,omitempty"`
}

type Game struct {
	Cards     []Card `json:"cards"`
	Starter   Team   `json:"starter"`
	Turn      Team   `json:"turn"`
	Clue      string `json:"clue,omitempty"`
	Number    int    `json:"number,omitempty"` // 0 with HasClue = unlimited
	HasClue   bool   `json:"has_clue,omitempty"`
	Guesses   int    `json:"guesses,omitempty"` // guesses made this turn
	Winner    Team   `json:"winner,omitempty"`
	EndReason string `json:"end_reason,omitempty"`
}

var (
	ErrGameOver        = errors.New("the game is over")
	ErrNotYourTurn     = errors.New("it is not that team's turn")
	ErrNoClue          = errors.New("wait for the handler to give a clue first")
	ErrHasClue         = errors.New("a clue has already been given this turn")
	ErrAlreadyRevealed = errors.New("that card is already revealed")
	ErrBadIndex        = errors.New("no such card")
	ErrNeedGuess       = errors.New("make at least one guess before ending the turn")
	ErrBadClue         = errors.New("invalid clue")
)

// NewGame deals a fresh board. The starting team is random and gets 9 agents,
// the other team 8, plus 7 bystanders and 1 burned card.
func NewGame(rng *rand.Rand, words []string) *Game {
	pool := append([]string(nil), words...)
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	starter := Ember
	if rng.IntN(2) == 1 {
		starter = Tide
	}
	colors := make([]Color, 0, BoardSize)
	for range 9 {
		colors = append(colors, starter.Color())
	}
	for range 8 {
		colors = append(colors, starter.Other().Color())
	}
	for range 7 {
		colors = append(colors, ColorBystander)
	}
	colors = append(colors, ColorBurned)
	rng.Shuffle(len(colors), func(i, j int) { colors[i], colors[j] = colors[j], colors[i] })

	g := &Game{Starter: starter, Turn: starter, Cards: make([]Card, BoardSize)}
	for i := range g.Cards {
		g.Cards[i] = Card{Word: pool[i], Color: colors[i]}
	}
	return g
}

func (g *Game) Over() bool { return g.Winner != "" }

// Remaining counts the unrevealed agents of a team.
func (g *Game) Remaining(t Team) int {
	n := 0
	for _, c := range g.Cards {
		if c.Color == t.Color() && !c.Revealed {
			n++
		}
	}
	return n
}

// GuessesLeft reports how many guesses the current clue allows.
func (g *Game) GuessesLeft() (n int, unlimited bool) {
	if g.Number == 0 {
		return 0, true
	}
	return g.Number + 1 - g.Guesses, false
}

// ValidateClue normalises a clue (upper-case) and checks it against the board.
func ValidateClue(raw string, cards []Card) (string, error) {
	w := strings.TrimSpace(raw)
	if w == "" {
		return "", fmt.Errorf("%w: enter a clue word", ErrBadClue)
	}
	if strings.IndexFunc(w, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("%w: a clue must be a single word", ErrBadClue)
	}
	if len([]rune(w)) > maxClueLen {
		return "", fmt.Errorf("%w: clue is too long", ErrBadClue)
	}
	for _, r := range w {
		if !unicode.IsLetter(r) && r != '-' {
			return "", fmt.Errorf("%w: use letters only", ErrBadClue)
		}
	}
	for _, c := range cards {
		if strings.EqualFold(c.Word, w) {
			return "", fmt.Errorf("%w: %s is on the board", ErrBadClue, strings.ToUpper(w))
		}
	}
	return strings.ToUpper(w), nil
}

// SubmitClue records the current team's clue. number 0 means unlimited.
func (g *Game) SubmitClue(team Team, raw string, number int) error {
	if g.Over() {
		return ErrGameOver
	}
	if team != g.Turn {
		return ErrNotYourTurn
	}
	if g.HasClue {
		return ErrHasClue
	}
	if number < 0 || number > MaxClueNum {
		return fmt.Errorf("%w: number must be 0-%d", ErrBadClue, MaxClueNum)
	}
	clue, err := ValidateClue(raw, g.Cards)
	if err != nil {
		return err
	}
	g.Clue, g.Number, g.HasClue, g.Guesses = clue, number, true, 0
	return nil
}

// RevealResult describes what a guess did.
type RevealResult struct {
	Card      Card
	TurnEnded bool // the turn passed to the other team
	GameOver  bool
}

// Reveal handles a guess by an operative of team on card idx.
func (g *Game) Reveal(team Team, idx int, by string) (RevealResult, error) {
	if g.Over() {
		return RevealResult{}, ErrGameOver
	}
	if team != g.Turn {
		return RevealResult{}, ErrNotYourTurn
	}
	if !g.HasClue {
		return RevealResult{}, ErrNoClue
	}
	if idx < 0 || idx >= len(g.Cards) {
		return RevealResult{}, ErrBadIndex
	}
	c := &g.Cards[idx]
	if c.Revealed {
		return RevealResult{}, ErrAlreadyRevealed
	}
	c.Revealed, c.By = true, by
	g.Guesses++
	res := RevealResult{Card: *c}

	switch {
	case c.Color == ColorBurned:
		g.finish(team.Other(), team.Label()+" found the BURNED card")
	case g.Remaining(Ember) == 0:
		g.finish(Ember, "all EMBER agents found")
	case g.Remaining(Tide) == 0:
		g.finish(Tide, "all TIDE agents found")
	case c.Color != team.Color():
		g.passTurn()
		res.TurnEnded = true
	default:
		if left, unlimited := g.GuessesLeft(); !unlimited && left <= 0 {
			g.passTurn()
			res.TurnEnded = true
		}
	}
	res.GameOver = g.Over()
	return res, nil
}

// EndTurn lets the team stop guessing; at least one guess is required.
func (g *Game) EndTurn(team Team) error {
	if g.Over() {
		return ErrGameOver
	}
	if team != g.Turn {
		return ErrNotYourTurn
	}
	if !g.HasClue {
		return ErrNoClue
	}
	if g.Guesses < 1 {
		return ErrNeedGuess
	}
	g.passTurn()
	return nil
}

func (g *Game) passTurn() {
	g.Turn = g.Turn.Other()
	g.Clue, g.Number, g.HasClue, g.Guesses = "", 0, false, 0
}

func (g *Game) finish(winner Team, reason string) {
	g.Winner, g.EndReason = winner, reason
	g.Clue, g.Number, g.HasClue, g.Guesses = "", 0, false, 0
}
