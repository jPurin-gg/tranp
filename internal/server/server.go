// Package server serves the embedded frontend and the room API.
//
//	POST /api/rooms                  create a room -> {"id": "..."}
//	POST /api/rooms/{id}/join        take a seat (sets the seat cookie) -> {"seat": 0|1}
//	GET  /api/rooms/{id}/events      SSE stream of snapshots
//	POST /api/rooms/{id}/flip        {"seq": n, "index": n}
//	POST /api/rooms/{id}/region      {"seq": n, "region": n}
//	POST /api/rooms/{id}/rematch     {"seq": n}
//
// Every snapshot carries "seq"; an action must echo the seq it was decided
// on, otherwise it is rejected with 409 (the client simply waits for the
// next snapshot).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jPurin-gg/tranp/internal/game"
	"github.com/jPurin-gg/tranp/web"
)

// Options configures a Server.
type Options struct {
	SecureCookie bool // false only for plain-http local development
	Log          *slog.Logger
}

// Server is the HTTP handler.
type Server struct {
	rooms *rooms
	opt   Options
	mux   *http.ServeMux
}

// New builds the handler.
func New(opt Options) *Server {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	s := &Server{rooms: newRooms(maxRooms), opt: opt, mux: http.NewServeMux()}
	static, err := fs.Sub(web.Files, ".")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(static)

	s.mux.HandleFunc("GET /{$}", s.page("index.html"))
	s.mux.HandleFunc("GET /r/{id}", s.page("room.html"))
	s.mux.Handle("GET /static/", files)
	s.mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		http.ServeFileFS(w, r, static, "manifest.webmanifest")
	})

	s.mux.HandleFunc("POST /api/rooms", s.handleCreate)
	s.mux.HandleFunc("POST /api/rooms/{id}/join", s.handleJoin)
	s.mux.HandleFunc("GET /api/rooms/{id}/events", s.handleEvents)
	s.mux.HandleFunc("POST /api/rooms/{id}/flip", s.handleFlip)
	s.mux.HandleFunc("POST /api/rooms/{id}/region", s.handleRegion)
	s.mux.HandleFunc("POST /api/rooms/{id}/rematch", s.handleRematch)
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	return s
}

// ServeHTTP adds security headers and logging around the mux.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	start := time.Now()
	lw := &statusWriter{ResponseWriter: w, status: 200}
	s.mux.ServeHTTP(lw, r)
	if !strings.HasSuffix(r.URL.Path, "/events") {
		s.opt.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", lw.status, "ms", time.Since(start).Milliseconds())
	}
}

// Sweep runs the idle-room collector until ctx ends.
func (s *Server) Sweep(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n := s.rooms.sweep(now); n > 0 {
				s.opt.Log.Info("rooms swept", "removed", n, "left", s.rooms.count())
			}
		}
	}
}

func (s *Server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, web.Files, name)
	}
}

// ---------------------------------------------------------------- rooms

const maxRooms = 1000

// cookieTTL outlives any game; the cookie is refreshed on every join.
const cookieTTL = 24 * time.Hour

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	rm, err := s.rooms.create(time.Now())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	s.opt.Log.Info("room created", "room", rm.id)
	writeJSON(w, http.StatusCreated, map[string]string{"id": rm.id})
}

func cookieName(id string) string { return "tranp_" + id }

func (s *Server) room(w http.ResponseWriter, r *http.Request) (*room, bool) {
	id := r.PathValue("id")
	rm := s.rooms.get(id)
	if rm == nil {
		writeError(w, http.StatusNotFound, "no such room")
		return nil, false
	}
	return rm, true
}

func (s *Server) token(r *http.Request, rm *room) string {
	c, err := r.Cookie(cookieName(rm.id))
	if err != nil {
		return ""
	}
	return c.Value
}

// seat resolves the caller's seat from the cookie; -1 if not seated.
func (s *Server) seat(r *http.Request, rm *room) int {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return rm.seatFor(s.token(r, rm))
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	rm, ok := s.room(w, r)
	if !ok {
		return
	}
	seat, token, err := rm.join(s.token(r, rm))
	if err != nil {
		writeGameError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName(rm.id),
		Value:    token,
		Path:     "/",
		MaxAge:   int(cookieTTL / time.Second),
		HttpOnly: true,
		Secure:   s.opt.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]int{"seat": seat})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	rm, ok := s.room(w, r)
	if !ok {
		return
	}
	seat := s.seat(r, rm)
	if seat < 0 {
		writeError(w, http.StatusForbidden, "not seated")
		return
	}
	ch, first, err := rm.subscribe()
	if err != nil {
		writeGameError(w, err)
		return
	}
	defer rm.unsubscribe(ch)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	// A stalled connection must not hold the subscription forever: every
	// write gets its own deadline.
	write := func(s string) bool {
		rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !write("data: " + string(first) + "\n\n") {
		return
	}
	// Heartbeat keeps the connection alive through proxies (Cloudflare drops
	// idle streams after ~100 s).
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b, open := <-ch:
			if !open || !write("data: "+string(b)+"\n\n") {
				return
			}
		case <-ping.C:
			if !write(": ping\n\n") {
				return
			}
		}
	}
}

func (s *Server) handleFlip(w http.ResponseWriter, r *http.Request) {
	rm, ok := s.room(w, r)
	if !ok {
		return
	}
	seat := s.seat(r, rm)
	if seat < 0 {
		writeError(w, http.StatusForbidden, "not seated")
		return
	}
	var body struct {
		Seq   uint64 `json:"seq"`
		Index int    `json:"index"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := rm.flip(seat, body.Index, body.Seq); err != nil {
		writeGameError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRegion(w http.ResponseWriter, r *http.Request) {
	rm, ok := s.room(w, r)
	if !ok {
		return
	}
	seat := s.seat(r, rm)
	if seat < 0 {
		writeError(w, http.StatusForbidden, "not seated")
		return
	}
	var body struct {
		Seq    uint64 `json:"seq"`
		Region int    `json:"region"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := rm.move(seat, body.Region, body.Seq); err != nil {
		writeGameError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRematch(w http.ResponseWriter, r *http.Request) {
	rm, ok := s.room(w, r)
	if !ok {
		return
	}
	seat := s.seat(r, rm)
	if seat < 0 {
		writeError(w, http.StatusForbidden, "not seated")
		return
	}
	var body struct {
		Seq uint64 `json:"seq"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := rm.rematch(body.Seq); err != nil {
		writeGameError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- helpers

func writeGameError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, game.ErrNotYourTurn), errors.Is(err, game.ErrNotPlaying), errors.Is(err, game.ErrNotFinished),
		errors.Is(err, errStale), errors.Is(err, errRoomFull):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errRoomClosed):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errTooMany):
		writeError(w, http.StatusTooManyRequests, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	// Bodies are tiny; a client that trickles one must not hold the handler.
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Second))
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad json")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (sw *statusWriter) Unwrap() http.ResponseWriter { return sw.ResponseWriter }
