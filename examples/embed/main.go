// Embed runs apex-engine inside your own server, under /apex/.
//
//	DATABASE_URL=... APEX_SECRET_KEY=... APEX_API_KEYS=key:tenant go run ./examples/embed
package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"

	"github.com/satyanvm/apex-engine/engine"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := engine.ConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	e, err := engine.New(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer e.Close()

	mux := http.NewServeMux()
	mux.Handle("/apex/", http.StripPrefix("/apex", e.Handler()))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("your application\n")) })

	srv := &http.Server{Addr: ":9000", Handler: mux}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	log.Print("your app on :9000, the engine under /apex/")
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
