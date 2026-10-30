// Command tenet serves GitHub commit insights, an LLM narrative over them, and a
// repository reconstruction from diffs, from one process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/swizzley/tenet/internal/api"
	"github.com/swizzley/tenet/internal/collect"
	"github.com/swizzley/tenet/internal/config"
	"github.com/swizzley/tenet/internal/dbuf"
	"github.com/swizzley/tenet/internal/github"
	"github.com/swizzley/tenet/internal/narrate"
	"github.com/swizzley/tenet/internal/reconstruct"
	"github.com/swizzley/tenet/internal/store"
	"github.com/swizzley/tenet/web"
)

func main() {
	cfgPath := flag.String("config", "config.json", "settings file; GITHUB_TOKEN and ANTHROPIC_API_KEY in the environment override its credentials")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*cfgPath, log); err != nil {
		fmt.Fprintln(os.Stderr, "tenet:", err)
		os.Exit(1)
	}
}

func run(cfgPath string, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath, os.Getenv)
	if err != nil {
		return err // names the missing credential and where to put it
	}
	log.Info("config", "settings", cfg.String())

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// The double buffer: collectors fill one half while the flusher commits the
	// other to SQLite as one transaction.
	sink := dbuf.New(db.UpsertCommits, cfg.BufferCap)
	gh := github.New(github.Options{Token: cfg.GitHubToken, Logger: log})

	svc := &api.Service{
		Store:     db,
		Collector: &collect.Collector{GH: gh, Sink: sink, MaxCommits: cfg.MaxPeriodCommits},
		Narrator:  narrate.New(cfg.AnthropicAPIKey, cfg.AnthropicModel),
		TTL:       time.Duration(cfg.CacheTTLSeconds) * time.Second,
	}
	recon := &reconstruct.Manager{
		Runner:  &reconstruct.Runner{GH: gh, Cache: db, Chunk: gh.Core.Chunk, BufferCap: cfg.BufferCap},
		MaxLive: cfg.MaxSessions, Keep: 30 * time.Minute, Timeout: 10 * time.Minute, Log: log,
	}
	srv := &api.Server{
		Svc: svc, Recon: recon, GH: gh, Sink: sink, Static: web.FS(), Log: log,
		MaxPeriod: time.Duration(cfg.MaxPeriodDays) * 24 * time.Hour, MaxReconstruct: cfg.MaxReconstruct, Now: time.Now,
	}
	hs := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       90 * time.Second,
		// No WriteTimeout: the event stream and the narrative are long-lived;
		// each handler sets its own deadline.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("listening", "addr", "http://"+cfg.Addr)
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	hs.Shutdown(sctx)
	return sink.Close() // commit whatever is still buffered
}
