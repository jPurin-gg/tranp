package server

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	mrand "math/rand/v2"
	"sync"
	"time"

	"github.com/jPurin-gg/tranp/internal/game"
)

// How long both players see the second card before the pair is taken
// (match) or turned back down with the turn passing (mismatch).
const (
	matchDelay    = 900 * time.Millisecond
	mismatchDelay = 1500 * time.Millisecond
)

// room is one game plus its two seats and the browsers watching it.
type room struct {
	id      string
	mu      sync.Mutex
	g       *game.Game
	tokens  [game.Players]string // per-seat secret; "" = empty seat
	subs    map[chan []byte]struct{}
	resolve *time.Timer
	touched time.Time
	rng     *mrand.Rand
}

func newRoom() *room {
	r := &room{
		id:      randomID(8),
		subs:    map[chan []byte]struct{}{},
		touched: time.Now(),
		rng:     mrand.New(mrand.NewPCG(rand64(), rand64())),
	}
	r.g = game.New(r.rng, game.SeatCreator)
	return r
}

// seatFor returns the seat owning token, or -1.
func (r *room) seatFor(token string) int {
	if token == "" {
		return -1
	}
	for s, t := range r.tokens {
		if t == token {
			return s
		}
	}
	return -1
}

// join seats the holder of token (rejoin) or the next free seat (new token).
// ok is false when the room is full.
func (r *room) join(token string) (seat int, newToken string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.touched = time.Now()
	if s := r.seatFor(token); s >= 0 {
		return s, token, true
	}
	for s := range r.tokens {
		if r.tokens[s] == "" {
			r.tokens[s] = randomID(32)
			if s == game.Players-1 {
				r.g.Start()
				r.broadcastLocked()
			}
			return s, r.tokens[s], true
		}
	}
	return -1, "", false
}

// wire is the JSON sent on the SSE stream.
type wire struct {
	game.View
	Seated [game.Players]bool `json:"seated"`
}

func (r *room) snapshotLocked() []byte {
	w := wire{View: r.g.View()}
	for s, t := range r.tokens {
		w.Seated[s] = t != ""
	}
	b, _ := json.Marshal(w)
	return b
}

func (r *room) broadcastLocked() {
	b := r.snapshotLocked()
	for ch := range r.subs {
		// Latest snapshot wins: drop a stale one rather than block.
		select {
		case ch <- b:
		default:
			select {
			case <-ch:
			default:
			}
			ch <- b
		}
	}
}

func (r *room) subscribe() (ch chan []byte, first []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch = make(chan []byte, 1)
	r.subs[ch] = struct{}{}
	return ch, r.snapshotLocked()
}

func (r *room) unsubscribe(ch chan []byte) {
	r.mu.Lock()
	delete(r.subs, ch)
	r.mu.Unlock()
}

func (r *room) flip(seat, idx int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.touched = time.Now()
	res, err := r.g.Flip(seat, idx)
	if err != nil {
		return err
	}
	if res.Second {
		delay := mismatchDelay
		if res.Match {
			delay = matchDelay
		}
		r.resolve = time.AfterFunc(delay, func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.g.Resolve()
			r.broadcastLocked()
		})
	}
	r.broadcastLocked()
	return nil
}

func (r *room) move(seat, region int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.touched = time.Now()
	if err := r.g.Move(seat, region); err != nil {
		return err
	}
	r.broadcastLocked()
	return nil
}

// rematch deals a new game; the player who did not start last time starts.
func (r *room) rematch(seat int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.touched = time.Now()
	if r.g.Phase != game.Finished {
		return game.ErrNotFinished
	}
	r.g = game.New(r.rng, 1-r.g.Starter)
	r.g.Start()
	r.broadcastLocked()
	return nil
}

func (r *room) idle(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subs) == 0 && now.Sub(r.touched) > roomTTL
}

func (r *room) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resolve != nil {
		r.resolve.Stop()
	}
	for ch := range r.subs {
		close(ch)
		delete(r.subs, ch)
	}
}

// roomTTL is how long an empty room is kept.
const roomTTL = 2 * time.Hour

// rooms is the in-memory registry.
type rooms struct {
	mu sync.Mutex
	m  map[string]*room
}

func newRooms() *rooms { return &rooms{m: map[string]*room{}} }

func (rs *rooms) create() *room {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for {
		r := newRoom()
		if _, dup := rs.m[r.id]; !dup {
			rs.m[r.id] = r
			return r
		}
	}
}

func (rs *rooms) get(id string) *room {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.m[id]
}

// sweep drops rooms nobody has touched or watched for roomTTL.
func (rs *rooms) sweep(now time.Time) int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	n := 0
	for id, r := range rs.m {
		if r.idle(now) {
			r.close()
			delete(rs.m, id)
			n++
		}
	}
	return n
}

func (rs *rooms) count() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.m)
}

var idEnc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return idEnc.EncodeToString(b)[:n]
}

func rand64() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	var u uint64
	for _, x := range b {
		u = u<<8 | uint64(x)
	}
	return u
}
