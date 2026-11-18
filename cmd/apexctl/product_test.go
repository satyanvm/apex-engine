package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recorder answers the newer routes and keeps what it was sent.
type recorder struct {
	bodies map[string]map[string]any
	paths  []string
	run    string
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{}
	_ = json.NewDecoder(r.Body).Decode(&body)
	key := r.Method + " " + r.URL.Path
	rec.paths = append(rec.paths, key+"?"+r.URL.RawQuery)
	rec.bodies[key] = body
	switch {
	case key == "POST /v1/runs/run_1/retry":
		w.WriteHeader(202)
		fmt.Fprint(w, `{"id":"run_2","status":"queued","resumeOf":"run_1"}`)
	case key == "GET /v1/runs/run_2":
		fmt.Fprint(w, rec.run)
	case strings.HasSuffix(r.URL.Path, "/items"):
		fmt.Fprint(w, `{"items":[{"index":1,"status":"failed","error":"card declined"}]}`)
	case key == "POST /v1/keys":
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":"key_1","name":"asha","role":"approver","key":"apx_secret"}`)
	case key == "GET /v1/audit":
		fmt.Fprint(w, `{"entries":[{"id":7,"actor":"asha","action":"run.approve","target":"run_1"}]}`)
	case key == "POST /v1/catalogs":
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":"cat_9","name":"Slack","kind":"piece"}`)
	case strings.HasSuffix(r.URL.Path, "/credentials"):
		fmt.Fprint(w, `{"id":"cat_9","auth":"oauth2"}`)
	case strings.HasSuffix(r.URL.Path, "/oauth/authorize"):
		fmt.Fprint(w, `{"authorizeUrl":"https://idp/authorize?state=s","redirectUrl":"https://engine/oauth/callback","expiresInSeconds":900}`)
	default:
		w.WriteHeader(403)
		fmt.Fprint(w, `{"error":{"code":"forbidden","message":"this key's role is viewer; this needs admin"}}`)
	}
}

func recording(t *testing.T) *recorder {
	t.Helper()
	rec := &recorder{bodies: map[string]map[string]any{}}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	t.Setenv("APEX_URL", srv.URL)
	t.Setenv("APEX_API_KEY", "k")
	pollEvery = time.Millisecond
	return rec
}

func TestRetryWaitsAndReportsFailedItemsWithItsOwnExitCode(t *testing.T) {
	rec := recording(t)
	rec.run = `{"id":"run_2","status":"succeeded","finishedAt":"2026-11-11T00:00:00Z","failedItems":2,"nodes":[{"key":"refund","status":"failed","itemsFailed":2}]}`
	code, out, log := cli("retry", "run_1", "--wait")
	if code != exitPartial {
		t.Fatalf("exit = %d, want %d; log: %s", code, exitPartial, log)
	}
	if !strings.Contains(log, "2 loop item(s) failed") || !strings.Contains(log, "apexctl items run_2 refund --status failed") {
		t.Errorf("log = %q", log)
	}
	if !strings.Contains(out, `"id": "run_2"`) {
		t.Errorf("out = %q", out)
	}
	rec.run = `{"id":"run_2","status":"failed","finishedAt":"2026-11-11T00:00:00Z","error":"boom","nodes":[]}`
	code, _, log = cli("retry", "run_1", "--wait")
	if code != exitFailed || !strings.Contains(log, "apexctl retry run_2") {
		t.Errorf("a failed run should say how to continue it: %d %q", code, log)
	}
	if code, _, _ := cli("retry"); code != exitUsage {
		t.Errorf("retry without an id = %d", code)
	}
}

func TestItemsKeysAndAudit(t *testing.T) {
	rec := recording(t)
	code, out, _ := cli("items", "run_1", "refund", "--status", "failed")
	if code != exitOK || !strings.Contains(out, "card declined") {
		t.Errorf("items: %d %q", code, out)
	}
	if last := rec.paths[len(rec.paths)-1]; !strings.Contains(last, "status=failed") {
		t.Errorf("items request = %s", last)
	}
	code, out, log := cli("keys", "add", "asha", "--role", "approver")
	if code != exitOK || !strings.Contains(out, "apx_secret") || !strings.Contains(log, "only time") {
		t.Errorf("keys add: %d %q %q", code, out, log)
	}
	if got := rec.bodies["POST /v1/keys"]; got["name"] != "asha" || got["role"] != "approver" {
		t.Errorf("keys body = %v", got)
	}
	if code, _, _ := cli("keys", "add", "asha"); code != exitUsage {
		t.Errorf("keys add without a role = %d", code)
	}
	code, out, _ = cli("audit", "--action", "run.", "--actor", "asha")
	if code != exitOK || !strings.Contains(out, "run.approve") {
		t.Errorf("audit: %d %q", code, out)
	}
	if last := rec.paths[len(rec.paths)-1]; !strings.Contains(last, "action=run.") || !strings.Contains(last, "actor=asha") {
		t.Errorf("audit request = %s", last)
	}
	// A refusal is shown with the engine's own reason.
	code, _, log = cli("keys")
	if code != exitFailed || !strings.Contains(log, "needs admin") {
		t.Errorf("forbidden: %d %q", code, log)
	}
}

func TestSecretsCanComeFromAFileOrTheEnvironment(t *testing.T) {
	rec := recording(t)
	file := filepath.Join(t.TempDir(), "conn.json")
	if err := os.WriteFile(file, []byte("{\"apiKey\":\"from-file\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, log := cli("tools", "add", "Slack", "--piece", "slack", "--auth", "@"+file); code != exitOK {
		t.Fatalf("tools add: %d %s", code, log)
	}
	source := rec.bodies["POST /v1/catalogs"]["source"].(map[string]any)
	if source["type"] != "piece" || source["piece"] != "slack" || source["auth"].(map[string]any)["apiKey"] != "from-file" {
		t.Errorf("source = %v", source)
	}
	t.Setenv("CLIENT_SECRET", "from-env")
	code, _, log := cli("tools", "auth", "cat_9", "--oauth", "client_credentials", "--token-url", "https://idp/token", "--client-id", "cid",
		"--client-secret", "env:CLIENT_SECRET", "--header", "X-Team: env:CLIENT_SECRET", "--query", "api_key=plain")
	if code != exitOK {
		t.Fatalf("tools auth: %d %s", code, log)
	}
	sent := rec.bodies["PUT /v1/catalogs/cat_9/credentials"]
	oauth := sent["oauth"].(map[string]any)
	if oauth["clientSecret"] != "from-env" || oauth["grant"] != "client_credentials" || sent["headers"].(map[string]any)["X-Team"] != "from-env" ||
		sent["query"].(map[string]any)["api_key"] != "plain" {
		t.Errorf("credentials = %v", sent)
	}
	if code, _, log := cli("tools", "auth", "cat_9", "--oauth", "client_credentials", "--client-secret", "env:NOT_SET_ANYWHERE"); code != exitUsage || !strings.Contains(log, "NOT_SET_ANYWHERE") {
		t.Errorf("a missing variable should be named: %d %q", code, log)
	}
	code, out, log := cli("tools", "oauth", "cat_9", "--auth-url", "https://idp/authorize", "--token-url", "https://idp/token", "--client-id", "cid", "--param", "access_type=offline")
	if code != exitOK || !strings.Contains(out, "authorizeUrl") || !strings.Contains(log, "https://engine/oauth/callback") {
		t.Errorf("tools oauth: %d %q %q", code, out, log)
	}
	if got := rec.bodies["POST /v1/catalogs/cat_9/oauth/authorize"]["authParams"].(map[string]any)["access_type"]; got != "offline" {
		t.Errorf("authParams = %v", got)
	}
	if code, _, _ := cli("tools", "add", "X", "--piece", "slack", "--mcp", "https://x"); code != exitUsage {
		t.Errorf("two sources at once = %d", code)
	}
}
