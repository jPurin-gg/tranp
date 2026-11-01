// Package game holds the rules of 神経衰弱 for two players. It knows nothing
// about HTTP; the server wraps a Game in a room and broadcasts View().
package game

import (
	"errors"
	"math/rand/v2"
)

// Layout constants shared with the frontend.
const (
	Cards       = 52
	Regions     = 4
	PerRegion   = Cards / Regions // 13
	Players     = 2
	SeatCreator = 0
)

// Card is rank 1..13 (A..K) and suit 0..3 (♠ ♥ ♦ ♣).
type Card struct {
	Rank int `json:"rank"`
	Suit int `json:"suit"`
}

// Phase of a game.
type Phase string

const (
	Waiting  Phase = "waiting"  // one player seated
	Playing  Phase = "playing"  // both seated
	Finished Phase = "finished" // every pair taken
)

var (
	ErrNotYourTurn = errors.New("not your turn")
	ErrBadIndex    = errors.New("bad card index")
	ErrBadRegion   = errors.New("bad region")
	ErrCardGone    = errors.New("card already taken")
	ErrCardOpen    = errors.New("card already open")
	ErrTwoOpen     = errors.New("two cards already open")
	ErrNotPlaying  = errors.New("game is not in progress")
	ErrNotFinished = errors.New("game is not finished")
)

// Game is the full, server-side state. Deck is never sent to clients.
type Game struct {
	Phase   Phase
	Deck    [Cards]Card
	Taken   [Cards]bool
	Open    []int // cards face up in the current turn (0..2)
	Turn    int   // seat whose turn it is
	Region  int   // region the active player is looking at (drives both screens)
	Scores  [Players]int
	Starter int // seat that opened this game
	// Pending is true after the second flip until Resolve() is called, so
	// both players get to see the second card before it is taken or hidden.
	Pending bool
}

// New deals a shuffled deck. The game starts in Waiting until Start().
func New(rng *rand.Rand, starter int) *Game {
	g := &Game{Phase: Waiting, Turn: starter, Starter: starter}
	i := 0
	for s := 0; s < 4; s++ {
		for r := 1; r <= 13; r++ {
			g.Deck[i] = Card{Rank: r, Suit: s}
			i++
		}
	}
	rng.Shuffle(Cards, func(a, b int) { g.Deck[a], g.Deck[b] = g.Deck[b], g.Deck[a] })
	return g
}

// Start moves Waiting -> Playing once the second player has joined.
func (g *Game) Start() {
	if g.Phase == Waiting {
		g.Phase = Playing
	}
}

// Move changes the region the active player (and so both screens) looks at.
func (g *Game) Move(seat, region int) error {
	if g.Phase != Playing {
		return ErrNotPlaying
	}
	if seat != g.Turn {
		return ErrNotYourTurn
	}
	if region < 0 || region >= Regions {
		return ErrBadRegion
	}
	g.Region = region
	return nil
}

// FlipResult tells the caller what happened so it can schedule Resolve().
type FlipResult struct {
	Second bool // this was the second card of the turn; Resolve() is due
	Match  bool // the two cards have the same rank
}

// Flip turns card idx face up for seat.
func (g *Game) Flip(seat, idx int) (FlipResult, error) {
	var res FlipResult
	if g.Phase != Playing {
		return res, ErrNotPlaying
	}
	if seat != g.Turn {
		return res, ErrNotYourTurn
	}
	if g.Pending || len(g.Open) >= 2 {
		return res, ErrTwoOpen
	}
	if idx < 0 || idx >= Cards {
		return res, ErrBadIndex
	}
	if g.Taken[idx] {
		return res, ErrCardGone
	}
	for _, o := range g.Open {
		if o == idx {
			return res, ErrCardOpen
		}
	}
	g.Open = append(g.Open, idx)
	// Looking at a card moves the shared view to its region.
	g.Region = idx / PerRegion
	if len(g.Open) < 2 {
		return res, nil
	}
	res.Second = true
	res.Match = g.Deck[g.Open[0]].Rank == g.Deck[g.Open[1]].Rank
	g.Pending = true
	return res, nil
}

// Resolve settles the open pair: a match is taken and the turn continues, a
// mismatch turns back down and the turn passes. The server calls it after a
// short delay so both players see the second card.
func (g *Game) Resolve() {
	if !g.Pending {
		return
	}
	g.Pending = false
	a, b := g.Open[0], g.Open[1]
	g.Open = g.Open[:0]
	if g.Deck[a].Rank != g.Deck[b].Rank {
		g.Turn = 1 - g.Turn
		return
	}
	g.Taken[a], g.Taken[b] = true, true
	g.Scores[g.Turn]++
	if g.Scores[0]+g.Scores[1] == Cards/2 {
		g.Phase = Finished
	}
}

// CardView is what a client may know about one slot.
type CardView struct {
	State string `json:"state"` // "down", "up", "gone"
	Rank  int    `json:"rank"`
	Suit  int    `json:"suit"`
}

// View is the snapshot broadcast to clients. It reveals only open cards.
type View struct {
	Phase   Phase           `json:"phase"`
	Turn    int             `json:"turn"`
	Region  int             `json:"region"`
	Scores  [Players]int    `json:"scores"`
	Starter int             `json:"starter"`
	Pending bool            `json:"pending"`
	Cards   [Cards]CardView `json:"cards"`
}

// View builds the client snapshot.
func (g *Game) View() View {
	v := View{Phase: g.Phase, Turn: g.Turn, Region: g.Region, Scores: g.Scores, Starter: g.Starter, Pending: g.Pending}
	for i := range v.Cards {
		v.Cards[i].State = "down"
		if g.Taken[i] {
			v.Cards[i].State = "gone"
		}
	}
	for _, o := range g.Open {
		v.Cards[o] = CardView{State: "up", Rank: g.Deck[o].Rank, Suit: g.Deck[o].Suit}
	}
	return v
}
