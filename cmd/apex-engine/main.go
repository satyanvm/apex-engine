// Command apex-engine serves the API and runs Temporal workers.
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
	"strconv"
	"syscall"
	"time"

	"github.com/satyanvm/apex-engine/internal/compiler"
	"github.com/satyanvm/apex-engine/internal/httpapi"
	"github.com/satyanvm/apex-engine/internal/runtime"
	"github.com/satyanvm/apex-engine/internal/secret"
	"github.com/satyanvm/apex-engine/internal/store"
	"go.temporal.io/sdk/client"
	sdklog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

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

	role := env("APEX_ROLE", "all")
	if role != "all" && role != "api" && role != "worker" {
		return errors.New("APEX_ROLE must be all, api or worker")
	}
	// Fail at boot on anything every compile or run would otherwise fail on.
	if err := compiler.RequirePlannerKeys(); err != nil {
		return err
	}
	box, err := secret.New(os.Getenv("APEX_SECRET_KEY"))
	if err != nil {
		return err
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	st, err := store.Connect(connectCtx, dbURL, box)
	if err != nil {
		return err
	}
	defer st.Pool.Close()

	tc, err := dialTemporal(ctx, log)
	if err != nil {
		return err
	}
	defer tc.Close()

	errs := make(chan error, 2)
	if role != "api" {
		w := worker.New(tc, runtime.TaskQueue, worker.Options{})
		w.RegisterWorkflow(runtime.RunWorkflow)
		w.RegisterWorkflow(runtime.ScheduledWorkflow)
		w.RegisterActivity(&runtime.Activities{Store: st, Complete: runtime.DefaultComplete})
		if err := w.Start(); err != nil {
			return err
		}
		defer w.Stop()
		log.Info("worker started", "queue", runtime.TaskQueue)
	}
	if role == "worker" {
		<-ctx.Done()
		return nil
	}

	keys, err := httpapi.ParseKeys(os.Getenv("APEX_API_KEYS"))
	if err != nil {
		return err
	}
	rpm, err := strconv.Atoi(env("APEX_COMPILE_RPM", "30"))
	if err != nil || rpm < 1 {
		return errors.New("APEX_COMPILE_RPM must be a positive integer")
	}
	api := &httpapi.Server{Store: st, Temporal: tc, Keys: keys, Compile: compiler.Compile, Log: log}
	srv := &http.Server{Addr: env("APEX_ADDR", ":8700"), Handler: api.Handler(rpm), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("api listening", "addr", srv.Addr)
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

// dialTemporal retries, so the engine can start alongside Temporal in Compose.
func dialTemporal(ctx context.Context, log *slog.Logger) (client.Client, error) {
	opts := client.Options{
		HostPort:  env("TEMPORAL_HOSTPORT", "localhost:7233"),
		Namespace: env("TEMPORAL_NAMESPACE", "default"),
		Logger:    sdklog.NewStructuredLogger(log),
	}
	var last error
	for i := 0; i < 40; i++ {
		c, err := client.Dial(opts)
		if err == nil {
			// Compose starts the engine before Temporal has created its namespace.
			if err = namespaceReady(ctx, opts); err == nil {
				return c, nil
			}
			c.Close()
		}
		last = err
		log.Info("waiting for Temporal", "error", err.Error())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return nil, last
}

func namespaceReady(ctx context.Context, opts client.Options) error {
	nc, err := client.NewNamespaceClient(opts)
	if err != nil {
		return err
	}
	defer nc.Close()
	_, err = nc.Describe(ctx, opts.Namespace)
	return err
}
