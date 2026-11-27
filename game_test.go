package main

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode"
)

func testRNG(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed+1)) }

// fixedGame builds a deterministic board: ember starts.
// idx 0-8 ember, 9-16 tide, 17-23 bystander, 24 burned.
func fixedGame() *Game {
	g := &Game{Starter: Ember, Turn: Ember, Cards: make([]Card, BoardSize)}
	for i := range g.Cards {
		c := ColorBystander
		switch {
		case i < 9:
			c = ColorEmber
		case i < 17:
			c = ColorTide
		case i == 24:
			c = ColorBurned
		}
		g.Cards[i] = Card{Word: "word" + string(rune('a'+i)), Color: c}
	}
	return g
}

func TestWordList(t *testing.T) {
	seen := map[string]bool{}
	for _, w := range wordList {
		if w == "" || w != strings.ToLower(w) {
			t.Errorf("bad word %q", w)
		}
		for _, r := range w {
			if !unicode.IsLetter(r) {
				t.Errorf("non-letter in %q", w)
			}
		}
		if seen[w] {
			t.Errorf("duplicate %q", w)
		}
		seen[w] = true
	}
	if len(wordList) < 300 {
		t.Errorf("word list too short: %d", len(wordList))
	}
}

func TestNewGameCounts(t *testing.T) {
	for seed := uint64(0); seed < 50; seed++ {
		g := NewGame(testRNG(seed), wordList)
		counts := map[Color]int{}
		words := map[string]bool{}
		for _, c := range g.Cards {
			counts[c.Color]++
			words[c.Word] = true
			if c.Revealed {
				t.Fatal("card revealed at deal")
			}
		}
		if len(g.Cards) != 25 || len(words) != 25 {
			t.Fatalf("seed %d: want 25 distinct cards, got %d/%d", seed, len(g.Cards), len(words))
		}
		other := g.Starter.Other()
		if counts[g.Starter.Color()] != 9 || counts[other.Color()] != 8 ||
			counts[ColorBystander] != 7 || counts[ColorBurned] != 1 {
			t.Fatalf("seed %d: bad counts %v", seed, counts)
		}
		if g.Turn != g.Starter || g.HasClue || g.Over() {
			t.Fatalf("seed %d: bad initial state %+v", seed, g)
		}
	}
}

func TestNewGameStartersVary(t *testing.T) {
	seen := map[Team]bool{}
	for seed := uint64(0); seed < 40; seed++ {
		seen[NewGame(testRNG(seed), wordList).Starter] = true
	}
	if len(seen) != 2 {
		t.Errorf("starter never varied: %v", seen)
	}
}

func TestValidateClue(t *testing.T) {
	cards := []Card{{Word: "ocean"}, {Word: "apple"}}
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"river", "RIVER", false},
		{"  river ", "RIVER", false},
		{"Well-Known", "WELL-KNOWN", false},
		{"ocean", "", true},
		{"OCEAN", "", true},
		{"Apple", "", true},
		{"two words", "", true},
		{"", "", true},
		{"   ", "", true},
		{"abc123", "", true},
		{"don't", "", true},
		{"-", "", true},
		{"---", "", true},
		{strings.Repeat("a", 40), "", true},
	}
	for _, tc := range tests {
		got, err := ValidateClue(tc.in, cards)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("ValidateClue(%q) = %q, %v; want %q err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
		if err != nil && !errors.Is(err, ErrBadClue) {
			t.Errorf("ValidateClue(%q) error not ErrBadClue: %v", tc.in, err)
		}
	}
}

func TestSubmitClue(t *testing.T) {
	tests := []struct {
		name    string
		team    Team
		clue    string
		number  int
		prep    func(g *Game)
		wantErr error
	}{
		{"ok", Ember, "sea", 2, nil, nil},
		{"number zero", Ember, "sea", 0, nil, ErrBadClue},
		{"wrong team", Tide, "sea", 2, nil, ErrNotYourTurn},
		{"already clued", Ember, "sea", 2, func(g *Game) { g.HasClue = true }, ErrHasClue},
		{"game over", Ember, "sea", 2, func(g *Game) { g.Winner = Tide }, ErrGameOver},
		{"number too big", Ember, "sea", 10, nil, ErrBadClue},
		{"number negative", Ember, "sea", -1, nil, ErrBadClue},
		{"board word", Ember, "worda", 1, nil, ErrBadClue},
		{"two words", Ember, "a b", 1, nil, ErrBadClue},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := fixedGame()
			if tc.prep != nil {
				tc.prep(g)
			}
			err := g.SubmitClue(tc.team, tc.clue, tc.number)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if err == nil && (g.Clue != "SEA" || g.Number != tc.number || !g.HasClue) {
				t.Errorf("clue not stored: %+v", g)
			}
		})
	}
}

func TestRevealRules(t *testing.T) {
	tests := []struct {
		name      string
		prep      func(g *Game)
		team      Team
		idx       int
		wantErr   error
		wantTurn  Team
		wantWin   Team
		wantEnded bool
	}{
		{"own card continues", nil, Ember, 0, nil, Ember, "", false},
		{"bystander ends turn", nil, Ember, 17, nil, Tide, "", true},
		{"enemy card ends turn", nil, Ember, 9, nil, Tide, "", true},
		{"burned loses", nil, Ember, 24, nil, Ember, Tide, false},
		{"no clue", func(g *Game) { g.HasClue = false }, Ember, 0, ErrNoClue, Ember, "", false},
		{"wrong team", nil, Tide, 9, ErrNotYourTurn, Ember, "", false},
		{"already revealed", func(g *Game) { g.Cards[0].Revealed = true }, Ember, 0, ErrAlreadyRevealed, Ember, "", false},
		{"bad index", nil, Ember, 25, ErrBadIndex, Ember, "", false},
		{"negative index", nil, Ember, -1, ErrBadIndex, Ember, "", false},
		{"game over", func(g *Game) { g.Winner = Tide }, Ember, 0, ErrGameOver, Ember, Tide, false},
		{"last own agent wins", func(g *Game) {
			for i := 1; i < 9; i++ {
				g.Cards[i].Revealed = true
			}
		}, Ember, 0, nil, Ember, Ember, false},
		{"revealing last enemy agent gives enemy the win", func(g *Game) {
			for i := 10; i < 17; i++ {
				g.Cards[i].Revealed = true
			}
		}, Ember, 9, nil, Ember, Tide, false},
		{"burned beats last agent order", func(g *Game) {
			for i := 0; i < 9; i++ {
				g.Cards[i].Revealed = true
			}
			g.Winner = ""
		}, Ember, 24, nil, Ember, Tide, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := fixedGame()
			g.HasClue, g.Clue, g.Number = true, "SEA", 3
			if tc.prep != nil {
				tc.prep(g)
			}
			res, err := g.Reveal(tc.team, tc.idx, "alex")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if g.Turn != tc.wantTurn || g.Winner != tc.wantWin {
				t.Errorf("turn=%s winner=%q, want turn=%s winner=%q", g.Turn, g.Winner, tc.wantTurn, tc.wantWin)
			}
			if err == nil {
				if res.TurnEnded != tc.wantEnded || res.GameOver != (tc.wantWin != "") {
					t.Errorf("result = %+v, want ended=%v over=%v", res, tc.wantEnded, tc.wantWin != "")
				}
				if !g.Cards[tc.idx].Revealed || g.Cards[tc.idx].By != "alex" {
					t.Errorf("card not marked revealed by alex: %+v", g.Cards[tc.idx])
				}
			} else if tc.idx >= 0 && tc.idx < BoardSize && tc.name != "already revealed" && g.Cards[tc.idx].Revealed {
				t.Error("failed reveal still revealed the card")
			}
		})
	}
}

func TestGuessLimit(t *testing.T) {
	tests := []struct {
		name       string
		number     int
		correct    int // consecutive own-colour guesses to make
		wantTurn   Team
		wantGuesss int
	}{
		{"number 1 allows 2 guesses, still ours after 1", 1, 1, Ember, 1},
		{"number 1 turn ends after 2", 1, 2, Tide, 0},
		{"number 2 still ours after 2", 2, 2, Ember, 2},
		{"number 2 ends after 3", 2, 3, Tide, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := fixedGame()
			if err := g.SubmitClue(Ember, "sea", tc.number); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < tc.correct; i++ {
				if _, err := g.Reveal(Ember, i, "a"); err != nil {
					t.Fatalf("guess %d: %v", i, err)
				}
			}
			if g.Turn != tc.wantTurn || g.Guesses != tc.wantGuesss {
				t.Errorf("turn=%s guesses=%d, want %s/%d", g.Turn, g.Guesses, tc.wantTurn, tc.wantGuesss)
			}
			if tc.wantTurn == Ember {
				if left, unlim := g.GuessesLeft(); unlim || left != tc.number+1-tc.correct {
					t.Errorf("GuessesLeft = %d,%v; want %d,false", left, unlim, tc.number+1-tc.correct)
				}
			}
		})
	}
}

func TestLegacyZeroClueUnlimited(t *testing.T) {
	g := fixedGame()
	// New clues with number 0 are rejected.
	if err := g.SubmitClue(Ember, "sea", 0); !errors.Is(err, ErrBadClue) {
		t.Fatalf("SubmitClue(0) = %v, want ErrBadClue", err)
	}
	// Stored legacy mid-turn clue with Number=0 still behaves as unlimited.
	g.HasClue, g.Clue, g.Number = true, "SEA", 0
	for i := range 8 {
		if _, err := g.Reveal(Ember, i, "a"); err != nil {
			t.Fatalf("legacy unlimited guess %d: %v", i, err)
		}
	}
	if g.Turn != Ember || g.Guesses != 8 {
		t.Fatalf("turn=%s guesses=%d, want ember/8", g.Turn, g.Guesses)
	}
	if _, unlim := g.GuessesLeft(); !unlim {
		t.Errorf("GuessesLeft() unlimited = false, want true")
	}
}

func TestEndTurn(t *testing.T) {
	g := fixedGame()
	if err := g.EndTurn(Ember); !errors.Is(err, ErrNoClue) {
		t.Errorf("end before clue: %v", err)
	}
	_ = g.SubmitClue(Ember, "sea", 2)
	if err := g.EndTurn(Ember); !errors.Is(err, ErrNeedGuess) {
		t.Errorf("end with no guess: %v", err)
	}
	if err := g.EndTurn(Tide); !errors.Is(err, ErrNotYourTurn) {
		t.Errorf("end by wrong team: %v", err)
	}
	if _, err := g.Reveal(Ember, 0, "a"); err != nil {
		t.Fatal(err)
	}
	if err := g.EndTurn(Ember); err != nil {
		t.Fatalf("end after guess: %v", err)
	}
	if g.Turn != Tide || g.HasClue || g.Clue != "" || g.Guesses != 0 {
		t.Errorf("turn not reset: %+v", g)
	}
	g.Winner = Tide
	if err := g.EndTurn(Tide); !errors.Is(err, ErrGameOver) {
		t.Errorf("end after game over: %v", err)
	}
}

func TestFullTurnCycle(t *testing.T) {
	g := fixedGame()
	_ = g.SubmitClue(Ember, "sea", 1)
	_, _ = g.Reveal(Ember, 17, "a") // bystander -> tide's turn
	if err := g.SubmitClue(Ember, "again", 1); !errors.Is(err, ErrNotYourTurn) {
		t.Errorf("ember clued out of turn: %v", err)
	}
	if _, err := g.Reveal(Tide, 9, "b"); !errors.Is(err, ErrNoClue) {
		t.Errorf("tide guessed without clue: %v", err)
	}
	if err := g.SubmitClue(Tide, "wave", 2); err != nil {
		t.Fatal(err)
	}
	if g.Remaining(Tide) != 8 || g.Remaining(Ember) != 9 {
		t.Errorf("remaining wrong: %d/%d", g.Remaining(Ember), g.Remaining(Tide))
	}
	if _, err := g.Reveal(Tide, 9, "b"); err != nil {
		t.Fatal(err)
	}
	if g.Remaining(Tide) != 7 {
		t.Errorf("tide remaining = %d, want 7", g.Remaining(Tide))
	}
}
