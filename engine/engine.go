// Package engine runs apex-engine inside your own Go program: the prompt
// compiler, the HTTP API and the durable Temporal worker.
//
//	e, err := engine.New(ctx, engine.Config{
//		DatabaseURL:      "postgres://...",
//		TemporalHostPort: "localhost:7233",
//		SecretKey:        base64Of32Bytes,
//		APIKeys:          map[string]string{"a-long-api-key": "acme"},
//	})
//	if err != nil { ... }
//	defer e.Close()
//	mux.Handle("/", e.Handler())
//
// New applies database migrations and starts the worker. Talk to the API with
// package client, from this process or any other.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	apexclient "github.com/satyanvm/apex-engine/client"
	"github.com/satyanvm/apex-engine/internal/compiler"
	"github.com/satyanvm/apex-engine/internal/httpapi"
	"github.com/satyanvm/apex-engine/internal/runtime"
	"github.com/satyanvm/apex-engine/internal/secret"
	"github.com/satyanvm/apex-engine/internal/store"
	"go.temporal.io/sdk/client"
	sdklog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
)

// Role selects which half of the engine a process runs.
type Role string

const (
	RoleAll    Role = "all"    // the API and a worker (the default)
	RoleAPI    Role = "api"    // only the API
	RoleWorker Role = "worker" // only a worker
)

// Config is everything the engine needs. Model providers are read from the
// environment (GEMINI_API_KEY, ANTHROPIC_API_KEY, OPENAI_API_KEY, PLANNER_MODEL).
type Config struct {
	DatabaseURL       string
	TemporalHostPort  string // default localhost:7233
	TemporalNamespace string // default "default"
	// SecretKey is 32 random bytes in base64. It seals stored credentials and
	// signs run events, so back it up.
	SecretKey string
	// APIKeys maps each API key to its tenant. Use these when the tenants are
	// known up front; such a key is an administrator of its tenant.
	APIKeys map[string]string
	// PlatformKey is for a product that has its own customers. It acts for any
	// tenant: each request names the tenant in X-Apex-Tenant, and may name the
	// end user (X-Apex-Actor) and their role (X-Apex-Role). A tenant needs no
	// setup. At least 32 characters; keep it on your server, never in a browser.
	// In Go, Engine.Client does the same without a key or a network hop.
	PlatformKey string
	Role        Role
	// CompileRPM limits compiles per tenant per minute. Default 30.
	CompileRPM int
	// Console also serves the optional web inspector at /.
	Console bool
	// DemoURL enables POST /v1/demo against the sample tools.
	DemoURL string
	// NotifyURL receives a Slack-style message whenever a run needs a human, and
	// PublicURL makes that message link to the run.
	NotifyURL, PublicURL string
	// PiecesURL is the Activepieces runner (see pieces/). Empty leaves piece
	// tools off. PiecesToken is the shared secret the runner expects.
	PiecesURL, PiecesToken string
	// MetricsToken turns on GET /metrics for whoever presents it.
	MetricsToken string
	// RequireApproval is the rule for steps that change something outside the
	// engine: "deletes" refuses any graph that deletes with no approval step in
	// front of the delete, "changes" refuses any change made that way. A compile
	// is told the rule and adds the approval itself. Empty allows both.
	RequireApproval string
	// MaxOpenRuns caps one tenant's unfinished runs; the next start gets a 429.
	// Zero means no cap. Set it when tenants are your customers.
	MaxOpenRuns int
	// RetentionDays deletes finished runs, with every step's stored output,
	// this many days after they finish. AuditRetentionDays does the same for
	// the audit log. Zero keeps them forever.
	RetentionDays, AuditRetentionDays int
	Logger                            *slog.Logger
}

// ConfigFromEnvWithoutKeys is ConfigFromEnv for a program that reaches the
// engine only through Engine.Client: it does not insist on an API key.
func ConfigFromEnvWithoutKeys() (Config, error) { return configFromEnv(false) }

// ConfigFromEnv reads the settings the apex-engine binary documents.
func ConfigFromEnv() (Config, error) { return configFromEnv(true) }

func configFromEnv(needKey bool) (Config, error) {
	cfg := Config{
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		TemporalHostPort:  os.Getenv("TEMPORAL_HOSTPORT"),
		TemporalNamespace: os.Getenv("TEMPORAL_NAMESPACE"),
		SecretKey:         os.Getenv("APEX_SECRET_KEY"),
		Role:              Role(os.Getenv("APEX_ROLE")),
		Console:           os.Getenv("APEX_CONSOLE") == "1",
		DemoURL:           os.Getenv("APEX_DEMO_URL"),
		NotifyURL:         os.Getenv("APEX_NOTIFY_WEBHOOK"),
		PublicURL:         os.Getenv("APEX_PUBLIC_URL"),
		PiecesURL:         os.Getenv("APEX_PIECES_URL"),
		PiecesToken:       os.Getenv("APEX_PIECES_TOKEN"),
		PlatformKey:       os.Getenv("APEX_PLATFORM_KEY"),
		RequireApproval:   os.Getenv("APEX_REQUIRE_APPROVAL"),
		MetricsToken:      os.Getenv("APEX_METRICS_TOKEN"),
	}
	if raw := os.Getenv("APEX_COMPILE_RPM"); raw != "" {
		rpm, err := strconv.Atoi(raw)
		if err != nil || rpm < 1 {
			return cfg, errors.New("APEX_COMPILE_RPM must be a positive integer")
		}
		cfg.CompileRPM = rpm
	}
	for name, into := range map[string]*int{"APEX_MAX_OPEN_RUNS": &cfg.MaxOpenRuns, "APEX_RETENTION_DAYS": &cfg.RetentionDays,
		"APEX_AUDIT_RETENTION_DAYS": &cfg.AuditRetentionDays} {
		if raw := os.Getenv(name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				return cfg, errors.New(name + " must be a whole number, zero or more")
			}
			*into = n
		}
	}
	if cfg.Role != RoleWorker {
		raw := os.Getenv("APEX_API_KEYS")
		if raw == "" && cfg.PlatformKey == "" && needKey {
			return cfg, errors.New("set APEX_API_KEYS (key:tenant pairs) or APEX_PLATFORM_KEY (one key for every tenant)")
		}
		if raw != "" {
			keys, err := httpapi.ParseKeys(raw)
			if err != nil {
				return cfg, err
			}
			cfg.APIKeys = keys
		}
	}
	return cfg, nil
}

func (c *Config) applyDefaults() error {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.TemporalHostPort == "" {
		c.TemporalHostPort = "localhost:7233"
	}
	if c.TemporalNamespace == "" {
		c.TemporalNamespace = "default"
	}
	if c.CompileRPM == 0 {
		c.CompileRPM = 30
	}
	if c.Role == "" {
		c.Role = RoleAll
	}
	switch c.Role {
	case RoleAll, RoleAPI, RoleWorker:
	default:
		return errors.New("engine: Role must be all, api or worker")
	}
	if c.DatabaseURL == "" {
		return errors.New("engine: DatabaseURL (DATABASE_URL) is required")
	}
	if c.RequireApproval != "" && c.RequireApproval != compiler.ApproveDeletes && c.RequireApproval != compiler.ApproveChanges {
		return errors.New("engine: RequireApproval (APEX_REQUIRE_APPROVAL) must be empty, \"deletes\" or \"changes\"")
	}
	if c.PlatformKey != "" && len(c.PlatformKey) < 32 {
		return errors.New("engine: PlatformKey (APEX_PLATFORM_KEY) must be at least 32 characters")
	}
	return nil
}

// Engine is a running instance.
type Engine struct {
	st       *store.Store
	tc       client.Client
	w        worker.Worker
	handler  http.Handler
	internal string // a platform key that never leaves this process
	stop     context.CancelFunc
}

// New connects to Postgres and Temporal, migrates, and starts the worker. It
// waits for Temporal for up to two minutes, so it can start alongside it.
func New(ctx context.Context, cfg Config) (*Engine, error) {
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	// With no model, compiles fail with this reason but saved graphs still run.
	if err := compiler.RequirePlannerKeys(); err != nil {
		cfg.Logger.Warn("compiling is unavailable", "reason", err.Error())
	}
	box, err := secret.New(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	st, err := store.Connect(connectCtx, cfg.DatabaseURL, box)
	if err != nil {
		return nil, fmt.Errorf("engine: connecting to the database: %w", err)
	}
	tc, err := dialTemporal(ctx, cfg)
	if err != nil {
		st.Pool.Close()
		return nil, fmt.Errorf("engine: connecting to Temporal: %w", err)
	}
	e := &Engine{st: st, tc: tc}
	if cfg.Role != RoleAPI {
		e.w = worker.New(tc, runtime.TaskQueue, worker.Options{})
		e.w.RegisterWorkflow(runtime.RunWorkflow)
		e.w.RegisterWorkflow(runtime.ScheduledWorkflow)
		e.w.RegisterActivity(&runtime.Activities{Store: st, Complete: runtime.DefaultComplete, NotifyURL: cfg.NotifyURL, PublicURL: cfg.PublicURL,
			PiecesURL: cfg.PiecesURL, PiecesToken: cfg.PiecesToken})
		if err := e.w.Start(); err != nil {
			e.Close()
			return nil, err
		}
		cfg.Logger.Info("worker started", "queue", runtime.TaskQueue)
	}
	if cfg.Role != RoleWorker {
		inner := make([]byte, 32)
		if _, err := rand.Read(inner); err != nil {
			e.Close()
			return nil, err
		}
		e.internal = hex.EncodeToString(inner)
		api := &httpapi.Server{Store: st, Temporal: tc, Keys: cfg.APIKeys, PlatformKeys: []string{cfg.PlatformKey, e.internal},
			Compile: compiler.Compile, Log: cfg.Logger, DemoURL: cfg.DemoURL, Console: cfg.Console,
			PublicURL: cfg.PublicURL, PiecesURL: cfg.PiecesURL, PiecesToken: cfg.PiecesToken, MetricsToken: cfg.MetricsToken,
			MaxOpenRuns: cfg.MaxOpenRuns, RequireApproval: cfg.RequireApproval}
		e.handler = api.Handler(cfg.CompileRPM)
	}
	if cfg.RetentionDays > 0 || cfg.AuditRetentionDays > 0 {
		bg, stop := context.WithCancel(context.Background())
		e.stop = stop
		go e.retain(bg, cfg)
	}
	return e, nil
}

// retain deletes what is past its retention, now and then every hour. Every
// process may run it: the deletes are idempotent.
func (e *Engine) retain(ctx context.Context, cfg Config) {
	sweep := func() {
		if cfg.RetentionDays > 0 {
			n, err := e.st.PurgeRuns(ctx, time.Now().AddDate(0, 0, -cfg.RetentionDays))
			if err != nil {
				cfg.Logger.Error("retention: runs", "error", err.Error())
			} else if n > 0 {
				cfg.Logger.Info("retention: deleted finished runs", "runs", n, "olderThanDays", cfg.RetentionDays)
			}
		}
		if cfg.AuditRetentionDays > 0 {
			n, err := e.st.PurgeAudit(ctx, time.Now().AddDate(0, 0, -cfg.AuditRetentionDays))
			if err != nil {
				cfg.Logger.Error("retention: audit log", "error", err.Error())
			} else if n > 0 {
				cfg.Logger.Info("retention: deleted audit entries", "entries", n, "olderThanDays", cfg.AuditRetentionDays)
			}
		}
	}
	sweep()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

// Handler serves the HTTP API. It is nil when Role is RoleWorker.
func (e *Engine) Handler() http.Handler { return e.handler }

// Client returns a client that acts for one tenant, inside this process: no
// API key and no network. A tenant needs no setup; naming it is enough, so a
// product can call this with its own customer id. Use As on the result to act
// as one end user with a narrower role. It is nil when Role is RoleWorker.
func (e *Engine) Client(tenant string) *apexclient.Client {
	if e.handler == nil {
		return nil
	}
	c := apexclient.New("http://apex.inprocess", e.internal)
	c.HTTP = &http.Client{Transport: inProcess{e.handler}}
	c.Tenant = tenant
	return c
}

// inProcess serves a request from the engine's own handler. The body streams,
// so a compile's progress arrives as it happens.
type inProcess struct{ h http.Handler }

func (t inProcess) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	rw := &pipeWriter{header: http.Header{}, pw: pw, ready: make(chan struct{})}
	go func() {
		defer func() {
			if v := recover(); v != nil {
				rw.start(http.StatusInternalServerError)
				_ = pw.CloseWithError(fmt.Errorf("engine: %v", v))
				return
			}
			rw.start(http.StatusOK)
			_ = pw.Close()
		}()
		t.h.ServeHTTP(rw, req)
	}()
	select {
	case <-rw.ready:
	case <-req.Context().Done():
		_ = pr.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
	return &http.Response{StatusCode: rw.status, Status: http.StatusText(rw.status), Header: rw.header, Body: pr,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Request: req, ContentLength: -1}, nil
}

type pipeWriter struct {
	header http.Header
	pw     *io.PipeWriter
	ready  chan struct{}
	once   sync.Once
	status int
}

func (w *pipeWriter) Header() http.Header { return w.header }
func (w *pipeWriter) start(status int) {
	w.once.Do(func() { w.status = status; close(w.ready) })
}
func (w *pipeWriter) WriteHeader(status int)      { w.start(status) }
func (w *pipeWriter) Write(b []byte) (int, error) { w.start(http.StatusOK); return w.pw.Write(b) }
func (w *pipeWriter) Flush()                      {}

// Close stops the worker and releases connections. In-flight runs are not lost:
// another worker, or this one after a restart, picks them up.
func (e *Engine) Close() {
	if e.stop != nil {
		e.stop()
	}
	if e.w != nil {
		e.w.Stop()
	}
	e.tc.Close()
	e.st.Pool.Close()
}

// dialTemporal retries, so the engine can start alongside Temporal.
func dialTemporal(ctx context.Context, cfg Config) (client.Client, error) {
	opts := client.Options{HostPort: cfg.TemporalHostPort, Namespace: cfg.TemporalNamespace, Logger: sdklog.NewStructuredLogger(cfg.Logger)}
	var last error
	for i := 0; i < 40; i++ {
		c, err := client.Dial(opts)
		if err == nil {
			// Temporal can accept connections before it has created the namespace.
			if err = namespaceReady(ctx, opts); err == nil {
				return c, nil
			}
			c.Close()
		}
		last = err
		cfg.Logger.Info("waiting for Temporal", "error", err.Error())
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
