package main

import (
	"bytes"
	"context"
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

type fake struct {
	status  string
	started map[string]any
	decided map[string]any
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{}
	_ = json.NewDecoder(r.Body).Decode(&body)
	run := func() string {
		finished := "null"
		if f.status == "succeeded" || f.status == "failed" {
			finished = `"2026-11-09T00:00:00Z"`
		}
		node := `{"key":"ok","status":"waiting_approval","output":{"message":"Refund?"}}`
		return fmt.Sprintf(`{"id":"run_1","status":%q,"finishedAt":%s,"nodes":[%s]}`, f.status, finished, node)
	}
	switch {
	case r.URL.Path == "/v1/catalogs":
		fmt.Fprint(w, `{"catalogs":[{"id":"cat_1","name":"Orders"}]}`)
	case r.URL.Path == "/v1/compile/stream":
		fmt.Fprint(w, "event: stage\ndata: {\"stage\":\"route\",\"status\":\"done\",\"detail\":\"d\"}\n\n")
		fmt.Fprint(w, "event: result\ndata: {\"plan\":{\"name\":\"P\",\"nodes\":[{\"key\":\"t\",\"type\":\"trigger.manual\"}],\"defaultInput\":{}},\"trace\":{}}\n\n")
	case r.URL.Path == "/v1/validate":
		fmt.Fprint(w, `{"valid":false,"problems":["bad reference"]}`)
	case r.URL.Path == "/v1/runs" && r.Method == "POST":
		f.started = body
		w.WriteHeader(202)
		fmt.Fprint(w, run())
	case r.URL.Path == "/v1/runs/run_1":
		fmt.Fprint(w, run())
	case strings.HasSuffix(r.URL.Path, "/approve"):
		f.decided = body
		f.status = "succeeded"
		fmt.Fprint(w, run())
	default:
		w.WriteHeader(404)
		fmt.Fprint(w, `{"error":{"code":"not_found","message":"no such thing"}}`)
	}
}

func setup(t *testing.T, f *fake) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("APEX_URL", srv.URL)
	t.Setenv("APEX_API_KEY", "k")
	pollEvery = time.Millisecond
}

func cli(args ...string) (int, string, string) {
	var out, log bytes.Buffer
	code := run(context.Background(), args, &out, &log)
	return code, out.String(), log.String()
}

func TestCompileWritesAPlanThatRunReads(t *testing.T) {
	f := &fake{status: "succeeded"}
	setup(t, f)
	path := filepath.Join(t.TempDir(), "plan.json")
	if code, _, log := cli("compile", "do a thing", "-o", path); code != exitOK {
		t.Fatalf("compile exit %d: %s", code, log)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"cat_1"`) {
		t.Errorf("the plan should record its tools: %s", raw)
	}
	code, _, log := cli("run", path, "--wait", "--input", "count=3", "--input", "who=ada")
	if code != exitOK {
		t.Fatalf("run exit %d: %s", code, log)
	}
	in := f.started["input"].(map[string]any)
	if in["count"] != float64(3) || in["who"] != "ada" {
		t.Errorf("input = %v", in)
	}
	if ids := f.started["catalogIds"].([]any); len(ids) != 1 || ids[0] != "cat_1" {
		t.Errorf("catalogIds = %v", ids)
	}
}

func plan(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(path, []byte(`{"name":"P","nodes":[{"key":"t","type":"trigger.manual"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExitCodes(t *testing.T) {
	f := &fake{status: "failed"}
	setup(t, f)
	if code, _, _ := cli("run", plan(t), "--wait"); code != exitFailed {
		t.Errorf("failed run: exit %d", code)
	}
	f.status = "waiting_approval"
	code, _, log := cli("run", plan(t), "--stop-at-approval")
	if code != exitWaiting || !strings.Contains(log, "apexctl approve run_1 ok") {
		t.Errorf("waiting run: exit %d, log %q", code, log)
	}
	if code, out, _ := cli("validate", plan(t)); code != exitFailed || !strings.Contains(out, "bad reference") {
		t.Errorf("invalid plan: exit %d, out %q", code, out)
	}
	if code, _, _ := cli("nonsense"); code != exitUsage {
		t.Errorf("unknown command: exit %d", code)
	}
	if code, _, log := cli("get", "run_missing"); code != exitFailed || !strings.Contains(log, "no such thing") {
		t.Errorf("engine error: exit %d, log %q", code, log)
	}
	t.Setenv("APEX_API_KEY", "")
	if code, _, _ := cli("status"); code != exitUsage {
		t.Errorf("no key: exit %d", code)
	}
}

func TestApprove(t *testing.T) {
	f := &fake{status: "waiting_approval"}
	setup(t, f)
	if code, _, log := cli("approve", "run_1", "ok", "--reject", "--note", "no"); code != exitOK {
		t.Fatalf("exit %d: %s", code, log)
	}
	if f.decided["approved"] != false || f.decided["note"] != "no" {
		t.Errorf("decision = %v", f.decided)
	}
}

func TestToolsResolveByName(t *testing.T) {
	setup(t, &fake{status: "succeeded"})
	if code, _, log := cli("compile", "x", "--tools", "nope"); code != exitFailed || !strings.Contains(log, `no tool named "nope"`) {
		t.Errorf("exit %d, log %q", code, log)
	}
}
