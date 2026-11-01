package server

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
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

// Retention: a room nobody watches is dropped after roomTTL; a room whose
// second seat was never taken is dropped after unjoinedTTL.
const (
	roomTTL     = 2 * time.Hour
	unjoinedTTL = 15 * time.Minute
)

// maxStreams caps SSE connections per room (one seat may have a few tabs).
const maxStreams = 6

var (
	errRoomClosed = errors.New("room closed")
	errStale      = errors.New("stale state")
	errTooMany    = errors.New("too many connections")
)

// room is one game plus its two seats and the browsers watching it.
type room struct {
	id      string
	mu      sync.Mutex
	g       *game.Game
	tokens  [game.Players]string // per-seat secret; "" = empty seat
	subs    map[chan []byte]struct{}
	seq     uint64 // bumps on every broadcast; actions must echo the seq they saw
	resolve *time.Timer
	touched time.Time
	closed  bool
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
func (r *room) join(token string) (seat int, newToken string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return -1, "", errRoomClosed
	}
	r.touched = time.Now()
	if s := r.seatFor(token); s >= 0 {
		return s, token, nil
	}
	for s := range r.tokens {
		if r.tokens[s] == "" {
			r.tokens[s] = randomID(32)
			if s == game.Players-1 {
				r.g.Start()
				r.broadcastLocked()
			}
			return s, r.tokens[s], nil
		}
	}
	return -1, "", errRoomFull
}

var errRoomFull = errors.New("room is full")

// wire is the JSON sent on the SSE stream.
type wire struct {
	game.View
	Seq    uint64             `json:"seq"`
	Seated [game.Players]bool `json:"seated"`
}

func (r *room) snapshotLocked() []byte {
	w := wire{View: r.g.View(), Seq: r.seq}
	for s, t := range r.tokens {
		w.Seated[s] = t != ""
	}
	b, _ := json.Marshal(w)
	return b
}

func (r *room) broadcastLocked() {
	r.seq++
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

func (r *room) subscribe() (ch chan []byte, first []byte, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, nil, errRoomClosed
	}
	if len(r.subs) >= maxStreams {
		return nil, nil, errTooMany
	}
	ch = make(chan []byte, 1)
	r.subs[ch] = struct{}{}
	return ch, r.snapshotLocked(), nil
}

func (r *room) unsubscribe(ch chan []byte) {
	r.mu.Lock()
	delete(r.subs, ch)
	r.touched = time.Now()
	r.mu.Unlock()
}

// checkLocked validates an action's preconditions shared by every mutation.
func (r *room) checkLocked(seq uint64) error {
	if r.closed {
		return errRoomClosed
	}
	if seq != r.seq {
		return errStale
	}
	r.touched = time.Now()
	return nil
}

func (r *room) flip(seat, idx int, seq uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkLocked(seq); err != nil {
		return err
	}
	res, err := r.g.Flip(seat, idx)
	if err != nil {
		return err
	}
	if res.Second {
		delay := mismatchDelay
		if res.Match {
			delay = matchDelay
		}
		g := r.g
		r.resolve = time.AfterFunc(delay, func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.closed || r.g != g {
				return
			}
			r.g.Resolve()
			r.broadcastLocked()
		})
	}
	r.broadcastLocked()
	return nil
}

func (r *room) move(seat, region int, seq uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkLocked(seq); err != nil {
		return err
	}
	if err := r.g.Move(seat, region); err != nil {
		return err
	}
	r.broadcastLocked()
	return nil
}

// rematch deals a new game; the player who did not start last time starts.
func (r *room) rematch(seq uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkLocked(seq); err != nil {
		return err
	}
	if r.g.Phase != game.Finished {
		return game.ErrNotFinished
	}
	if r.resolve != nil {
		r.resolve.Stop()
	}
	r.g = game.New(r.rng, 1-r.g.Starter)
	r.g.Start()
	r.broadcastLocked()
	return nil
}

// expire closes the room if it is idle and reports whether it did.
func (r *room) expire(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return true
	}
	ttl := roomTTL
	if r.tokens[game.Players-1] == "" {
		ttl = unjoinedTTL
	}
	if len(r.subs) > 0 || now.Sub(r.touched) < ttl {
		return false
	}
	r.closeLocked()
	return true
}

func (r *room) closeLocked() {
	r.closed = true
	if r.resolve != nil {
		r.resolve.Stop()
	}
	for ch := range r.subs {
		close(ch)
		delete(r.subs, ch)
	}
}

// rooms is the in-memory registry.
type rooms struct {
	mu  sync.Mutex
	m   map[string]*room
	max int
	// Creation is rate limited with a token bucket so a script cannot fill
	// the registry: refill 1 room/s up to burst.
	tokens   float64
	burst    float64
	lastFill time.Time
}

func newRooms(max int) *rooms {
	return &rooms{m: map[string]*room{}, max: max, tokens: 30, burst: 30, lastFill: time.Now()}
}

var errBusy = errors.New("too many rooms")

func (rs *rooms) create(now time.Time) (*room, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.tokens = min(rs.burst, rs.tokens+now.Sub(rs.lastFill).Seconds())
	rs.lastFill = now
	if rs.tokens < 1 || len(rs.m) >= rs.max {
		return nil, errBusy
	}
	rs.tokens--
	for {
		r := newRoom()
		if _, dup := rs.m[r.id]; !dup {
			rs.m[r.id] = r
			return r, nil
		}
	}
}

func (rs *rooms) get(id string) *room {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.m[id]
}

// sweep drops rooms nobody has touched or watched for their TTL.
func (rs *rooms) sweep(now time.Time) int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	n := 0
	for id, r := range rs.m {
		if r.expire(now) {
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
