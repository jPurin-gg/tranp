// Command tranp serves the 神経衰弱 game.
//
// Environment:
//
//	TRANP_ADDR             listen address (default 127.0.0.1:8080)
//	TRANP_INSECURE_COOKIE  set to 1 for plain-http local development only
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jPurin-gg/tranp/internal/server"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	addr := os.Getenv("TRANP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	secure := os.Getenv("TRANP_INSECURE_COOKIE") == ""

	s := server.New(server.Options{SecureCookie: secure, Log: log})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go s.Sweep(ctx)

	srv := &http.Server{
		Addr:              addr,
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE streams stay open.
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Info("listening", "addr", addr, "secure_cookie", secure)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
