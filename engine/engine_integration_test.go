//go:build integration

package engine_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/satyanvm/apex-engine/client"
	"github.com/satyanvm/apex-engine/engine"
)

// TestEmbedded uses the engine exactly as a host program would, through its
// public packages only.
func TestEmbedded(t *testing.T) {
	db, hp := os.Getenv("APEX_TEST_DATABASE_URL"), os.Getenv("TEMPORAL_TEST_HOSTPORT")
	if db == "" || hp == "" {
		t.Skip("APEX_TEST_DATABASE_URL and TEMPORAL_TEST_HOSTPORT are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	key := "embedded-host-key-0123456789"
	e, err := engine.New(ctx, engine.Config{
		DatabaseURL: db, TemporalHostPort: hp,
		SecretKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)),
		APIKeys:   map[string]string{key: "embedded"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	srv := httptest.NewServer(e.Handler())
	defer srv.Close()

	c := client.New(srv.URL, key)
	run, err := c.StartRun(ctx, client.RunRequest{DAG: client.DAG{Nodes: []client.Node{
		{Key: "start", Type: "trigger.manual", DependsOn: []string{}, Config: map[string]any{}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	run, err = c.WaitRun(ctx, run.ID, client.WaitOptions{Poll: 50 * time.Millisecond})
	if err != nil || run.Status != client.StatusSucceeded {
		t.Fatalf("run = %+v, err = %v", run, err)
	}
}

func TestConfigErrorsAreNamed(t *testing.T) {
	if _, err := engine.New(context.Background(), engine.Config{}); err == nil {
		t.Fatal("an empty config should be refused")
	}
}
