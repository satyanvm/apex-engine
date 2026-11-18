// Command apex-eval measures how often the compiler turns a sentence into a
// graph that does the right thing.
//
// For each case it compiles the prompt, checks the graph's shape, and (with
// --execute) runs it against a mock backend that records every write. A case
// passes only when the writes that happened are exactly the writes the sentence
// asked for. Structure alone is not enough: a graph can look right and refund
// the wrong orders.
//
//	APEX_URL=... APEX_API_KEY=... apex-eval --execute
//
// The engine must be able to reach the mock: run it with
// APEX_ALLOW_PRIVATE_NETWORK=1, and pass --mock-url when it is in Docker.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/satyanvm/apex-engine/client"
)

type evalCase struct {
	ID      string   `json:"id"`
	Tools   []string `json:"tools"`
	Prompt  string   `json:"prompt"`
	Approve *bool    `json:"approve"`
	Expect  struct {
		Uses           []string `json:"uses"`
		NotUses        []string `json:"notUses"`
		Kinds          []string `json:"kinds"`
		NotKinds       []string `json:"notKinds"`
		ApprovalBefore []string `json:"approvalBefore"`
		MaxNodes       int      `json:"maxNodes"`
		Calls          []string `json:"calls"`
	} `json:"expect"`
}

type result struct {
	ID         string   `json:"id"`
	Prompt     string   `json:"prompt"`
	Compiled   bool     `json:"compiled"`
	CompileMs  int64    `json:"compileMs"`
	Repairs    int      `json:"repairs"`
	Nodes      int      `json:"nodes"`
	CompileErr string   `json:"compileError,omitempty"`
	Structure  []string `json:"structureProblems"`
	Executed   bool     `json:"executed"`
	RunStatus  string   `json:"runStatus,omitempty"`
	RunError   string   `json:"runError,omitempty"`
	Calls      []string `json:"calls"`
	Missing    []string `json:"missingCalls"`
	Unexpected []string `json:"unexpectedCalls"`
	Passed     bool     `json:"passed"`
	Plan       any      `json:"plan,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "apex-eval:", err)
		os.Exit(2)
	}
}

func run() error {
	dir := flag.String("dir", "evals", "directory holding cases.json, fixtures.json and specs/")
	only := flag.String("only", "", "comma-separated case ids, or a prefix ending in *")
	execute := flag.Bool("execute", false, "run each compiled graph against the mock backend and check what it did")
	listen := flag.String("mock-listen", "127.0.0.1:8799", "where the mock backend listens")
	mockURL := flag.String("mock-url", "", "the mock backend's address as the engine sees it (default http://<mock-listen>)")
	report := flag.String("report", "", "write the full results as JSON to this file")
	perCase := flag.Duration("timeout", 8*time.Minute, "time allowed for one case to run")
	minPass := flag.Float64("min-pass", 0, "exit 1 when the pass rate is below this fraction")
	flag.Parse()

	apiKey := os.Getenv("APEX_API_KEY")
	if apiKey == "" {
		return errors.New("APEX_API_KEY is required")
	}
	base := os.Getenv("APEX_URL")
	if base == "" {
		base = "http://localhost:8700"
	}
	c := client.New(base, apiKey)
	c.HTTP = &http.Client{Timeout: 12 * time.Minute}

	var cases []evalCase
	if err := readJSON(filepath.Join(*dir, "cases.json"), &cases); err != nil {
		return err
	}
	cases = filter(cases, *only)
	if len(cases) == 0 {
		return errors.New("no cases selected")
	}
	fixtures := map[string]map[string][]map[string]any{}
	if err := readJSON(filepath.Join(*dir, "fixtures.json"), &fixtures); err != nil {
		return err
	}
	mock := &mockBackend{data: fixtures}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("mock backend: %w", err)
	}
	defer ln.Close()
	go func() { _ = http.Serve(ln, mock) }()
	if *mockURL == "" {
		*mockURL = "http://" + *listen
	}

	ctx := context.Background()
	st, err := c.Status(ctx)
	if err != nil {
		return fmt.Errorf("engine at %s: %w", base, err)
	}
	fmt.Fprintf(os.Stderr, "engine %s, models: map=%s reduce=%s\n", base, st.Models.Map, st.Models.Reduce)
	catalogs, err := installTools(ctx, c, *dir, *mockURL, cases)
	if err != nil {
		return err
	}

	var results []result
	for i, ec := range cases {
		fmt.Fprintf(os.Stderr, "[%d/%d] %s ... ", i+1, len(cases), ec.ID)
		r := evaluate(ctx, c, mock, catalogs, ec, *execute, *perCase)
		results = append(results, r)
		verdict := "PASS"
		if !r.Passed {
			verdict = "FAIL: " + reason(r)
		}
		fmt.Fprintf(os.Stderr, "%s (%.0fs)\n", verdict, float64(r.CompileMs)/1000)
	}
	summary := summarise(results, *execute, st.Models.Map, st.Models.Reduce)
	fmt.Println(summary.text)
	if *report != "" {
		raw, _ := json.MarshalIndent(map[string]any{"summary": summary.data, "results": results}, "", "  ")
		if err := os.WriteFile(*report, raw, 0o644); err != nil {
			return err
		}
	}
	if summary.rate < *minPass {
		os.Exit(1)
	}
	return nil
}

func readJSON(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func filter(cases []evalCase, only string) []evalCase {
	if only == "" {
		return cases
	}
	var out []evalCase
	for _, c := range cases {
		for _, want := range strings.Split(only, ",") {
			want = strings.TrimSpace(want)
			if c.ID == want || (strings.HasSuffix(want, "*") && strings.HasPrefix(c.ID, strings.TrimSuffix(want, "*"))) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// installTools imports each spec the cases need, pointed at the mock backend.
// Catalogs from an earlier evaluation are replaced, so the run measures the
// importer as it is now.
func installTools(ctx context.Context, c *client.Client, dir, mockURL string, cases []evalCase) (map[string]string, error) {
	need := map[string]bool{}
	for _, ec := range cases {
		for _, t := range ec.Tools {
			need[t] = true
		}
	}
	existing, err := c.ListCatalogs(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for tool := range need {
		name := "Eval " + tool
		for _, cat := range existing {
			if cat.Name == name {
				if err := c.DeleteCatalog(ctx, cat.ID); err != nil {
					return nil, err
				}
			}
		}
		spec, err := os.ReadFile(filepath.Join(dir, "specs", tool+".yaml"))
		if err != nil {
			return nil, err
		}
		cat, err := c.CreateCatalog(ctx, client.CatalogRequest{Name: name, BaseURL: strings.TrimSuffix(mockURL, "/") + "/" + tool,
			Source: client.CatalogSource{Type: "openapi", Spec: string(spec)}})
		if err != nil {
			return nil, fmt.Errorf("importing %s: %w", tool, err)
		}
		out[tool] = cat.ID
	}
	return out, nil
}

func evaluate(ctx context.Context, c *client.Client, mock *mockBackend, catalogs map[string]string, ec evalCase, execute bool, timeout time.Duration) result {
	r := result{ID: ec.ID, Prompt: ec.Prompt, Structure: []string{}, Calls: []string{}, Missing: []string{}, Unexpected: []string{}}
	var ids []string
	for _, t := range ec.Tools {
		ids = append(ids, catalogs[t])
	}
	began := time.Now()
	res, err := c.Compile(ctx, client.CompileRequest{Prompt: ec.Prompt, CatalogIDs: ids})
	r.CompileMs = time.Since(began).Milliseconds()
	if err != nil {
		r.CompileErr = err.Error()
		return r
	}
	r.Compiled, r.Repairs, r.Nodes = true, res.Trace.Repairs, len(res.Plan.Nodes)
	r.Structure = checkStructure(ec, res.Plan.Nodes)
	r.Passed = len(r.Structure) == 0
	if !execute {
		if !r.Passed {
			r.Plan = res.Plan
		}
		return r
	}
	mock.reset()
	r.Executed = true
	status, runErr := drive(ctx, c, res.Plan, ids, ec, timeout)
	r.RunStatus, r.RunError = status, runErr
	r.Calls = mock.writes()
	r.Missing, r.Unexpected = diff(ec.Expect.Calls, r.Calls)
	r.Passed = len(r.Structure) == 0 && status == client.StatusSucceeded && len(r.Missing) == 0 && len(r.Unexpected) == 0
	if !r.Passed {
		r.Plan = res.Plan
	}
	return r
}

// drive runs a plan to the end, answering every approval the way the case says.
func drive(ctx context.Context, c *client.Client, plan client.Plan, catalogs []string, ec evalCase, timeout time.Duration) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	run, err := c.StartRun(ctx, client.RunRequest{DAG: plan.DAG(), CatalogIDs: catalogs, Name: "eval " + ec.ID})
	if err != nil {
		return "not_started", err.Error()
	}
	approve := ec.Approve == nil || *ec.Approve
	for {
		run, err = c.GetRun(ctx, run.ID)
		if err != nil {
			return "unknown", err.Error()
		}
		if run.Done() {
			msg := ""
			if run.Error != nil {
				msg = *run.Error
			}
			if run.Status == client.StatusSucceeded && run.FailedItems > 0 {
				return "succeeded_with_failed_items", fmt.Sprintf("%d loop items failed", run.FailedItems)
			}
			return run.Status, msg
		}
		for _, n := range run.Waiting() {
			// A decision can race the run moving on; the next poll sorts it out.
			_ = c.Decide(ctx, run.ID, n.Key, approve, "apex-eval")
		}
		select {
		case <-ctx.Done():
			_ = c.CancelRun(context.Background(), run.ID)
			return "timed_out", "the run did not finish in " + timeout.String()
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func checkStructure(ec evalCase, nodes []client.Node) []string {
	problems := []string{}
	byKey := map[string]client.Node{}
	for _, n := range nodes {
		byKey[n.Key] = n
	}
	uses := func(op string) []client.Node {
		var out []client.Node
		for _, n := range nodes {
			if strings.HasSuffix(n.Type, "."+op) {
				out = append(out, n)
			}
		}
		return out
	}
	hasKind := func(kind string) bool {
		for _, n := range nodes {
			if n.Type == kind {
				return true
			}
		}
		return false
	}
	for _, op := range ec.Expect.Uses {
		if len(uses(op)) == 0 {
			problems = append(problems, "does not use "+op)
		}
	}
	for _, op := range ec.Expect.NotUses {
		if len(uses(op)) > 0 {
			problems = append(problems, "uses "+op+", which the request did not ask for")
		}
	}
	for _, k := range ec.Expect.Kinds {
		if !hasKind(k) {
			problems = append(problems, "has no "+k+" step")
		}
	}
	for _, k := range ec.Expect.NotKinds {
		if hasKind(k) {
			problems = append(problems, "has a "+k+" step it does not need")
		}
	}
	if ec.Expect.MaxNodes > 0 && len(nodes) > ec.Expect.MaxNodes {
		problems = append(problems, fmt.Sprintf("has %d steps, more than %d", len(nodes), ec.Expect.MaxNodes))
	}
	for _, op := range ec.Expect.ApprovalBefore {
		for _, n := range uses(op) {
			if !approvalUpstream(n, byKey, map[string]bool{}) {
				problems = append(problems, n.Key+" ("+op+") can run without an approval before it")
			}
		}
	}
	return problems
}

func approvalUpstream(n client.Node, byKey map[string]client.Node, seen map[string]bool) bool {
	for _, d := range n.DependsOn {
		if seen[d] {
			continue
		}
		seen[d] = true
		dep, ok := byKey[d]
		if !ok {
			continue
		}
		if dep.Type == "logic.approval" || approvalUpstream(dep, byKey, seen) {
			return true
		}
	}
	return false
}

// diff compares two multisets of calls.
func diff(want, got []string) (missing, unexpected []string) {
	missing, unexpected = []string{}, []string{}
	count := map[string]int{}
	for _, w := range want {
		count[w]++
	}
	for _, g := range got {
		if count[g] > 0 {
			count[g]--
		} else {
			unexpected = append(unexpected, g)
		}
	}
	for call, n := range count {
		for ; n > 0; n-- {
			missing = append(missing, call)
		}
	}
	sort.Strings(missing)
	sort.Strings(unexpected)
	return missing, unexpected
}

func reason(r result) string {
	switch {
	case !r.Compiled:
		return "did not compile: " + clip(r.CompileErr, 140)
	case len(r.Structure) > 0:
		return strings.Join(r.Structure, "; ")
	case !r.Executed:
		return ""
	case len(r.Missing) > 0 || len(r.Unexpected) > 0:
		return fmt.Sprintf("missing %v, unexpected %v (run %s)", r.Missing, r.Unexpected, r.RunStatus)
	}
	return "run " + r.RunStatus + ": " + clip(r.RunError, 140)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

type summary struct {
	text string
	data map[string]any
	rate float64
}

func summarise(results []result, executed bool, mapModel, reduceModel string) summary {
	var compiled, structure, passed, repaired int
	var times []int64
	for _, r := range results {
		if r.Compiled {
			compiled++
			times = append(times, r.CompileMs)
			if len(r.Structure) == 0 {
				structure++
			}
			if r.Repairs > 0 {
				repaired++
			}
		}
		if r.Passed {
			passed++
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	median := int64(0)
	if len(times) > 0 {
		median = times[len(times)/2]
	}
	n := len(results)
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-32s %-9s %-7s %-6s %s\n", "case", "compiled", "repairs", "steps", "result")
	for _, r := range results {
		verdict := "pass"
		if !r.Passed {
			verdict = "FAIL  " + reason(r)
		}
		fmt.Fprintf(&b, "%-32s %-9v %-7d %-6d %s\n", r.ID, r.Compiled, r.Repairs, r.Nodes, clip(verdict, 150))
	}
	fmt.Fprintf(&b, "\nmodels: map=%s reduce=%s\n", mapModel, reduceModel)
	fmt.Fprintf(&b, "compiled to a valid graph: %d/%d\n", compiled, n)
	fmt.Fprintf(&b, "right shape:               %d/%d\n", structure, n)
	if executed {
		fmt.Fprintf(&b, "did exactly what was asked: %d/%d\n", passed, n)
	}
	fmt.Fprintf(&b, "needed a repair round:     %d/%d\n", repaired, compiled)
	fmt.Fprintf(&b, "median compile time:       %.1fs\n", float64(median)/1000)
	rate := 0.0
	if n > 0 {
		rate = float64(passed) / float64(n)
	}
	return summary{text: b.String(), rate: rate, data: map[string]any{
		"cases": n, "compiled": compiled, "rightShape": structure, "passed": passed, "executed": executed,
		"neededRepair": repaired, "medianCompileMs": median, "mapModel": mapModel, "reduceModel": reduceModel,
		"at": time.Now().UTC().Format(time.RFC3339),
	}}
}

/* -------------------------------- mock backend ------------------------------ */

// mockBackend serves the fixture data and records every write. Lists hold two
// records a page by default, so a graph that forgets to ask for every page
// misses records and fails its case.
type mockBackend struct {
	mu   sync.Mutex
	data map[string]map[string][]map[string]any
	log  []string
}

func (m *mockBackend) reset() {
	m.mu.Lock()
	m.log = nil
	m.mu.Unlock()
}

func (m *mockBackend) writes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]string{}, m.log...)
	sort.Strings(out)
	return out
}

func (m *mockBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if r.Method != http.MethodGet {
		_, _ = io.Copy(io.Discard, r.Body)
		m.mu.Lock()
		m.log = append(m.log, r.Method+" "+r.URL.Path)
		m.mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	}
	if len(parts) < 2 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	rows, ok := m.data[parts[0]][parts[1]]
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if len(parts) == 3 {
		for _, row := range rows {
			if fmt.Sprint(row["id"]) == parts[2] {
				_ = json.NewEncoder(w).Encode(row)
				return
			}
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if len(parts) > 3 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	matched := []map[string]any{}
	for _, row := range rows {
		keep := true
		for name := range q {
			if name == "page" || name == "per_page" {
				continue
			}
			if v, has := row[name]; has && fmt.Sprint(v) != q.Get(name) {
				keep = false
			}
		}
		if keep {
			matched = append(matched, row)
		}
	}
	page, per := 1, 2
	if n, err := strconv.Atoi(q.Get("page")); err == nil && n > 0 {
		page = n
	}
	if n, err := strconv.Atoi(q.Get("per_page")); err == nil && n > 0 && n <= 100 {
		per = n
	}
	from := (page - 1) * per
	if from > len(matched) {
		from = len(matched)
	}
	to := from + per
	if to > len(matched) {
		to = len(matched)
	}
	_ = json.NewEncoder(w).Encode(matched[from:to])
}
