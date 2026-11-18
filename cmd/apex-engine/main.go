// Command apex-engine serves the API and runs Temporal workers. It is a thin
// wrapper over package engine, which you can embed instead.
//
// APEX_ROLE selects what this process does: all (default), api, or worker.
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

	"github.com/satyanvm/apex-engine/engine"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("exit", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := engine.ConfigFromEnv()
	if err != nil {
		return err
	}
	cfg.Logger = log
	e, err := engine.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer e.Close()
	if e.Handler() == nil { // worker only
		<-ctx.Done()
		return nil
	}

	addr := os.Getenv("APEX_ADDR")
	if addr == "" {
		addr = ":8700"
	}
	srv := &http.Server{Addr: addr, Handler: e.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdown, done := context.WithTimeout(context.Background(), 20*time.Second)
	defer done()
	return srv.Shutdown(shutdown)
}
