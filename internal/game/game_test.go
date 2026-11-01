package game

import (
	"math/rand/v2"
	"testing"
)

func newPlaying(t *testing.T) *Game {
	t.Helper()
	g := New(rand.New(rand.NewPCG(1, 2)), 0)
	g.Start()
	return g
}

// find returns two indices with the same rank and one with a different rank.
func find(g *Game) (a, b, c int) {
	a = 0
	for i := 1; i < Cards; i++ {
		if g.Deck[i].Rank == g.Deck[a].Rank && b == 0 {
			b = i
		} else if g.Deck[i].Rank != g.Deck[a].Rank && c == 0 {
			c = i
		}
	}
	return
}

func TestDeckIsFullPack(t *testing.T) {
	g := newPlaying(t)
	seen := map[Card]bool{}
	for _, c := range g.Deck {
		if c.Rank < 1 || c.Rank > 13 || c.Suit < 0 || c.Suit > 3 || seen[c] {
			t.Fatalf("bad or duplicate card %+v", c)
		}
		seen[c] = true
	}
	if len(seen) != Cards {
		t.Fatalf("got %d distinct cards", len(seen))
	}
}

func TestMatchKeepsTurn(t *testing.T) {
	g := newPlaying(t)
	a, b, _ := find(g)
	if _, err := g.Flip(0, a); err != nil {
		t.Fatal(err)
	}
	res, err := g.Flip(0, b)
	if err != nil || !res.Second || !res.Match {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !g.Pending || g.Taken[a] || g.Scores[0] != 0 {
		t.Fatalf("match should wait for Resolve: %+v", g)
	}
	g.Resolve()
	if !g.Taken[a] || !g.Taken[b] || g.Scores[0] != 1 || g.Turn != 0 || len(g.Open) != 0 || g.Pending {
		t.Fatalf("state after match: %+v", g)
	}
}

func TestMismatchPassesTurnAfterResolve(t *testing.T) {
	g := newPlaying(t)
	a, _, c := find(g)
	g.Flip(0, a)
	res, err := g.Flip(0, c)
	if err != nil || !res.Second || res.Match {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !g.Pending || g.Turn != 0 {
		t.Fatalf("should be pending on seat 0: %+v", g)
	}
	if _, err := g.Flip(0, 5); err != ErrTwoOpen {
		t.Fatalf("third flip while pending: %v", err)
	}
	v := g.View()
	if v.Cards[a].State != "up" || v.Cards[c].State != "up" {
		t.Fatalf("both cards should be shown: %+v %+v", v.Cards[a], v.Cards[c])
	}
	g.Resolve()
	if g.Pending || g.Turn != 1 || len(g.Open) != 0 {
		t.Fatalf("after resolve: %+v", g)
	}
	if _, err := g.Flip(0, a); err != ErrNotYourTurn {
		t.Fatalf("seat 0 after turn passed: %v", err)
	}
}

func TestViewHidesDeck(t *testing.T) {
	g := newPlaying(t)
	v := g.View()
	for i, c := range v.Cards {
		if c.State != "down" || c.Rank != 0 || c.Suit != 0 {
			t.Fatalf("card %d leaked: %+v", i, c)
		}
	}
	g.Flip(0, 7)
	v = g.View()
	if v.Cards[7].State != "up" || v.Cards[7].Rank != g.Deck[7].Rank || v.Region != 7/PerRegion {
		t.Fatalf("open card view: %+v region=%d", v.Cards[7], v.Region)
	}
}

func TestErrors(t *testing.T) {
	g := New(rand.New(rand.NewPCG(3, 4)), 1)
	if _, err := g.Flip(1, 0); err != ErrNotPlaying {
		t.Fatalf("flip while waiting: %v", err)
	}
	g.Start()
	if _, err := g.Flip(0, 0); err != ErrNotYourTurn {
		t.Fatalf("wrong seat: %v", err)
	}
	if err := g.Move(1, 4); err != ErrBadRegion {
		t.Fatalf("bad region: %v", err)
	}
	if err := g.Move(1, 3); err != nil || g.Region != 3 {
		t.Fatalf("move: %v region=%d", err, g.Region)
	}
	if _, err := g.Flip(1, 52); err != ErrBadIndex {
		t.Fatalf("bad index: %v", err)
	}
	g.Flip(1, 0)
	if _, err := g.Flip(1, 0); err != ErrCardOpen {
		t.Fatalf("same card twice: %v", err)
	}
}

func TestFullGameFinishes(t *testing.T) {
	g := newPlaying(t)
	// Play perfectly: always pair the first untaken card with its mate.
	for g.Phase == Playing {
		seat := g.Turn
		a := -1
		for i := range g.Deck {
			if !g.Taken[i] {
				a = i
				break
			}
		}
		b := -1
		for i := a + 1; i < Cards; i++ {
			if !g.Taken[i] && g.Deck[i].Rank == g.Deck[a].Rank {
				b = i
				break
			}
		}
		if _, err := g.Flip(seat, a); err != nil {
			t.Fatal(err)
		}
		res, err := g.Flip(seat, b)
		if err != nil || !res.Match {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		g.Resolve()
	}
	if g.Scores[0] != 26 || g.Scores[1] != 0 {
		t.Fatalf("scores %v", g.Scores)
	}
}
