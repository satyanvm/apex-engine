package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/satyanvm/apex-engine/internal/catalog"
	"github.com/satyanvm/apex-engine/internal/compiler"
	"github.com/satyanvm/apex-engine/internal/netguard"
	"github.com/satyanvm/apex-engine/internal/oauth"
	"github.com/satyanvm/apex-engine/internal/store"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

const (
	maxBodyBytes = 512 << 10
	httpTimeout  = 90 * time.Second
)

// Activities holds everything nodes touch. Workflow code never calls these directly.
type Activities struct {
	Store *store.Store
	// Complete runs an agent.llm call; replaced in tests.
	Complete func(ctx context.Context, model, system, user string) (string, error)
	// NotifyURL, when set, receives a Slack-style {"text":...} POST each time a
	// run stops for a human. PublicURL makes that message link to the run.
	NotifyURL, PublicURL string
	// PiecesURL is the Activepieces runner (pieces/server.mjs). Empty means
	// piece nodes fail with that reason. PiecesToken authenticates to it.
	PiecesURL, PiecesToken string
}

// DefaultComplete calls the configured model.
func DefaultComplete(ctx context.Context, model, system, user string) (string, error) {
	return compiler.Complete(ctx, model, system, user, 4*time.Minute)
}

func fatal(format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), "NodeError", nil)
}

// MarkRunStarted moves the run to running.
func (a *Activities) MarkRunStarted(ctx context.Context, runID string) error {
	return a.Store.MarkRunStarted(ctx, runID)
}

// RefreshRunStatus recomputes running/waiting_approval.
func (a *Activities) RefreshRunStatus(ctx context.Context, runID string) error {
	return a.Store.RefreshRunStatus(ctx, runID)
}

// FinishNode records a state the workflow decided.
func (a *Activities) FinishNode(ctx context.Context, in FinishNodeInput) error {
	var out any
	if in.Output != nil {
		out = in.Output
	}
	return a.Store.NodeFinish(ctx, store.NodeRef{RunID: in.RunID, Key: in.Key, HasItem: in.HasItem, Index: in.Index}, in.Status, out, in.Error)
}

// CopyPrior carries a finished loop's body rows over from the run being resumed.
func (a *Activities) CopyPrior(ctx context.Context, in CopyPriorInput) error {
	return a.Store.CopyPrior(ctx, in.RunID, in.Prior, in.Keys)
}

// FinishRun closes the run.
func (a *Activities) FinishRun(ctx context.Context, in FinishRunInput) error {
	return a.Store.FinishRun(ctx, in.RunID, in.Status, in.Error)
}

// CreateScheduledRun inserts the row for a scheduled run; a retry finds it present.
func (a *Activities) CreateScheduledRun(ctx context.Context, in ScheduledInput) error {
	meta := store.RunMeta{Name: in.Name, SavedWorkflow: in.WorkflowRef, Source: "schedule", StartedBy: "schedule"}
	_, _, err := a.Store.CreateRun(ctx, in.Tenant, in.RunID, in.WorkflowID, "sched:"+in.RunID, in.Nodes, in.Input, in.CatalogIDs, meta)
	return err
}

// LoadSavedWorkflow reads the graph a scheduled tick should run. A deleted
// workflow fails the tick with the reason instead of retrying.
func (a *Activities) LoadSavedWorkflow(ctx context.Context, tenant, id string) (SavedGraph, error) {
	wf, err := a.Store.GetWorkflow(ctx, tenant, id)
	if errors.Is(err, store.ErrNotFound) {
		return SavedGraph{}, fatal("workflow %s no longer exists", id)
	}
	if err != nil {
		return SavedGraph{}, err
	}
	return SavedGraph{Name: wf.Name, Nodes: wf.Nodes, Input: wf.DefaultInput, CatalogIDs: wf.CatalogIDs, CallbackURL: wf.CallbackURL}, nil
}

// notifyApproval tells a person a run is waiting. It is best effort: a chat
// outage must not fail the run.
func (a *Activities) notifyApproval(ctx context.Context, in NodeInput, message string) {
	if a.NotifyURL == "" {
		return
	}
	if len(message) > 500 {
		message = message[:500] + "..."
	}
	text := fmt.Sprintf("Approval needed on %s: %s", in.Node.Name, message)
	if a.PublicURL != "" {
		text += "\n" + strings.TrimSuffix(a.PublicURL, "/") + "/#/runs/" + in.RunID
	} else {
		text += "\nRun " + in.RunID
	}
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.NotifyURL, bytes.NewReader(body))
	if err != nil {
		activity.GetLogger(ctx).Warn("approval notification", "error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		activity.GetLogger(ctx).Warn("approval notification", "error", err.Error())
		return
	}
	_ = resp.Body.Close()
}

// ExecuteNode runs one node. Credentials are read here, so they never enter
// workflow history.
func (a *Activities) ExecuteNode(ctx context.Context, in NodeInput) (NodeResult, error) {
	stop := startHeartbeat(ctx)
	defer stop()
	ref := store.NodeRef{RunID: in.RunID, Key: in.Node.Key, HasItem: in.HasItem, Index: in.Index}
	if in.ResumeOf != "" {
		out, ok, err := a.Store.PriorOutput(ctx, in.Tenant, in.ResumeOf, ref)
		if err != nil {
			return NodeResult{}, err
		}
		if ok {
			// It succeeded in the run being resumed: carry the result over
			// instead of doing it again.
			if err := a.Store.NodeReuse(ctx, ref, out); err != nil {
				return NodeResult{}, err
			}
			return NodeResult{Output: out, Passed: priorPassed(in.Node.Type, out), Reused: true}, nil
		}
	}
	if err := a.Store.NodeStart(ctx, ref); err != nil {
		return NodeResult{}, err
	}
	scope := Scope{Input: in.Input, Outputs: in.Upstream, Item: in.Item, HasItem: in.HasItem, Loop: in.Loop}
	if in.Node.Type == "logic.approval" {
		message, err := scope.RenderString(str(in.Node.Config["message"]))
		if err != nil {
			return NodeResult{}, fatal("%s", err)
		}
		out := map[string]any{"message": message, "token": in.ApprovalToken}
		if err := a.Store.NodeFinish(ctx, ref, store.StatusWaiting, out, ""); err != nil {
			return NodeResult{}, err
		}
		if !in.Preview {
			a.notifyApproval(ctx, in, message)
		}
		return NodeResult{Output: out}, nil
	}
	res, err := a.dispatch(ctx, in, scope)
	if err != nil && in.Preview && nonRetryable(err) && usesPlanned(in.Upstream) {
		// The step needs something a skipped write would have returned, so the
		// preview cannot say what it would do. It says that instead of failing.
		res, err = planned("unknown", map[string]any{
			"reason": "this step uses the result of a step the preview did not run", "detail": rootMessage(err)}), nil
		if in.Node.Type == "logic.loop" {
			res.Output["items"], res.Output["count"] = []any{}, float64(0)
		}
	}
	if err != nil {
		return NodeResult{}, err
	}
	// A loop finishes after its fan-out and a wait after its timer; the
	// workflow records those.
	if in.Node.Type != "logic.loop" && in.Node.Type != "logic.wait" {
		if err := a.Store.NodeFinish(ctx, ref, store.StatusSuccess, res.Output, ""); err != nil {
			return NodeResult{}, err
		}
	}
	return res, nil
}

// planned is what a preview records in place of doing something.
func planned(kind string, detail map[string]any) NodeResult {
	detail["kind"] = kind
	return NodeResult{Output: map[string]any{"preview": true, "planned": detail}, Passed: true}
}

// isRead reports whether an HTTP method only reads. A preview sends those.
func isRead(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func nonRetryable(err error) bool {
	var app *temporal.ApplicationError
	return errors.As(err, &app) && app.NonRetryable()
}

func usesPlanned(upstream map[string]map[string]any) bool {
	for _, out := range upstream {
		if on, _ := out["preview"].(bool); on {
			return true
		}
	}
	return false
}

// bodyOf returns a request's body as JSON when it is JSON, for showing a person.
func bodyOf(req *http.Request) any {
	if req.GetBody == nil {
		return nil
	}
	rc, err := req.GetBody()
	if err != nil {
		return nil
	}
	defer rc.Close()
	raw, _ := io.ReadAll(io.LimitReader(rc, maxBodyBytes))
	if len(raw) == 0 {
		return nil
	}
	var parsed any
	if json.Unmarshal(raw, &parsed) == nil {
		return parsed
	}
	return string(raw)
}

// priorPassed recovers a gate's decision from its stored output.
func priorPassed(nodeType string, out map[string]any) bool {
	switch nodeType {
	case "logic.branch", "logic.filter":
		passed, _ := out["passed"].(bool)
		return passed
	case "logic.approval":
		approved, _ := out["approved"].(bool)
		return approved
	}
	return true
}

// startHeartbeat reports liveness until stopped. The SDK throttles the calls.
func startHeartbeat(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				activity.RecordHeartbeat(ctx)
			}
		}
	}()
	return func() { close(done) }
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

func (a *Activities) dispatch(ctx context.Context, in NodeInput, scope Scope) (NodeResult, error) {
	t := in.Node.Type
	switch {
	case strings.HasPrefix(t, "trigger."):
		return NodeResult{Output: map[string]any{"input": in.Input}}, nil
	case t == "agent.llm" || t == "agent.chat":
		return a.agent(ctx, in, scope)
	case t == "tool.http":
		return a.httpNode(ctx, in, scope)
	case t == "logic.branch" || t == "logic.filter":
		expr := str(in.Node.Config["condition"])
		ok, err := scope.EvalCondition(expr)
		if err != nil {
			return NodeResult{}, fatal("%s", err)
		}
		return NodeResult{Output: map[string]any{"passed": ok, "condition": expr}, Passed: ok}, nil
	case t == "logic.loop":
		return a.loop(in, scope)
	case t == "logic.wait":
		return waitNode(in, scope)
	case t == "data.transform":
		return transformNode(ctx, in, scope)
	case strings.HasPrefix(t, "api."):
		return a.apiNode(ctx, in, scope)
	case strings.HasPrefix(t, "mcp."):
		return a.mcpNode(ctx, in, scope)
	case strings.HasPrefix(t, "piece."):
		return a.pieceNode(ctx, in, scope)
	}
	return NodeResult{}, fatal("unsupported node type %q", t)
}

func (a *Activities) agent(ctx context.Context, in NodeInput, scope Scope) (NodeResult, error) {
	system, err := scope.RenderString(str(in.Node.Config["system"]))
	if err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	prompt, err := scope.RenderString(str(in.Node.Config["prompt"]))
	if err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	model := compiler.AgentModel()
	text, err := a.Complete(ctx, model, system, prompt)
	if err != nil {
		return NodeResult{}, fmt.Errorf("model call failed: %w", err)
	}
	return NodeResult{Output: map[string]any{"text": text, "model": model}}, nil
}

func (a *Activities) loop(in NodeInput, scope Scope) (NodeResult, error) {
	expr := strings.Trim(strings.TrimSpace(str(in.Node.Config["items"])), "{} ")
	v, err := scope.Lookup(expr)
	if err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	items, ok := decodeJSONString(v).([]any)
	if !ok {
		return NodeResult{}, fatal("loop items %q is not a list", expr)
	}
	return NodeResult{Output: map[string]any{"items": items, "count": float64(len(items))}}, nil
}

// response reads a capped body and parses JSON when it is JSON.
func readBody(resp *http.Response) (any, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBodyBytes {
		return nil, fatal("response body is larger than %d bytes", maxBodyBytes)
	}
	var parsed any
	if json.Unmarshal(raw, &parsed) == nil {
		return parsed, nil
	}
	return string(raw), nil
}

func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusRequestTimeout || code >= 500
}

func snippet(v any) string {
	text := str(v)
	if len(text) > 500 {
		text = text[:500] + "..."
	}
	return text
}

// transportError marks a refused private address as permanent; other network
// errors are retried.
func transportError(err error) error {
	if strings.Contains(err.Error(), "refusing to connect to private address") {
		return fatal("%s", err)
	}
	return err
}

func (a *Activities) httpNode(ctx context.Context, in NodeInput, scope Scope) (NodeResult, error) {
	cfg, err := scope.Render(in.Node.Config)
	if err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	c := cfg.(map[string]any)
	method := strings.ToUpper(strings.TrimSpace(str(c["method"])))
	if method == "" {
		method = http.MethodGet
	}
	target := str(c["url"])
	if u, err := url.Parse(target); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return NodeResult{}, fatal("invalid url %q", target)
	}
	if in.Preview && !isRead(method) {
		return planned("call", map[string]any{"tool": "HTTP request", "method": method, "url": target, "body": decodeJSONString(c["body"])}), nil
	}
	var body io.Reader
	if c["body"] != nil && str(c["body"]) != "" {
		body = strings.NewReader(str(c["body"]))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	headers, err := headerMap(c["headers"])
	if err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := netguard.Client(httpTimeout, true).Do(req)
	if err != nil {
		return NodeResult{}, transportError(err)
	}
	defer resp.Body.Close()
	parsed, err := readBody(resp)
	if err != nil {
		return NodeResult{}, err
	}
	if retryableStatus(resp.StatusCode) {
		return NodeResult{}, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, req.URL.Host, snippet(parsed))
	}
	// A 4xx is a result here: graphs branch on outputs.x.status.
	return NodeResult{Output: map[string]any{"status": float64(resp.StatusCode), "body": parsed}}, nil
}

func headerMap(v any) (map[string]string, error) {
	out := map[string]string{}
	switch t := v.(type) {
	case nil:
	case string:
		if strings.TrimSpace(t) == "" {
			return out, nil
		}
		if err := json.Unmarshal([]byte(t), &out); err != nil {
			return nil, fmt.Errorf("headers must be a JSON object of strings: %w", err)
		}
	case map[string]any:
		for k, x := range t {
			out[k] = str(x)
		}
	default:
		return nil, errors.New("headers must be a JSON object of strings")
	}
	return out, nil
}

// findImported locates the catalog that defines the node's type, among the
// catalogs this run was given.
func (a *Activities) findImported(ctx context.Context, in NodeInput) (*store.Catalog, compiler.CatalogNode, error) {
	cats, err := a.Store.GetCatalogs(ctx, in.Tenant, in.CatalogIDs)
	if errors.Is(err, store.ErrNotFound) {
		return nil, compiler.CatalogNode{}, fatal("%s (it may have been deleted)", err)
	}
	if err != nil {
		return nil, compiler.CatalogNode{}, err
	}
	for _, c := range cats {
		for _, n := range c.Nodes {
			if n.ID == in.Node.Type {
				return c, n, nil
			}
		}
	}
	return nil, compiler.CatalogNode{}, fatal("no catalog on this run defines node type %q", in.Node.Type)
}

// buildAPIRequest fills an imported operation from a node's rendered config.
func buildAPIRequest(ctx context.Context, base string, spec compiler.CatalogNode, cfg map[string]any) (*http.Request, error) {
	path, _ := spec.Exec["path"].(string)
	method, _ := spec.Exec["method"].(string)
	query := url.Values{}
	headers := map[string]string{}
	params, _ := spec.Exec["params"].([]any)
	for _, raw := range params {
		p, _ := raw.(map[string]any)
		name, _ := p["name"].(string)
		in, _ := p["in"].(string)
		v, present := cfg[name]
		if !present || v == nil || str(v) == "" {
			if in == "path" {
				return nil, fmt.Errorf("missing path parameter %q", name)
			}
			continue
		}
		switch in {
		case "path":
			// Escaped, so a value cannot change which path or host is called.
			path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(str(v)))
		case "query":
			query.Set(name, str(v))
		case "header":
			headers[name] = str(v)
		}
	}
	target := strings.TrimSuffix(base, "/") + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var body io.Reader
	hasBody, _ := spec.Exec["hasBody"].(bool)
	if hasBody && cfg["body"] != nil {
		text := str(cfg["body"])
		if !json.Valid([]byte(text)) {
			return nil, fmt.Errorf("body is not valid JSON: %s", snippet(text))
		}
		body = bytes.NewReader([]byte(text))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (a *Activities) mcpNode(ctx context.Context, in NodeInput, scope Scope) (NodeResult, error) {
	c, spec, err := a.findImported(ctx, in)
	if err != nil {
		return NodeResult{}, err
	}
	rendered, err := scope.Render(in.Node.Config)
	if err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	args, err := coerceArgs(rendered.(map[string]any), spec)
	if err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	if in.Preview && !isTrue(spec.Exec["readOnly"]) {
		// An MCP tool does not say reliably whether it changes anything, so a
		// preview runs only the ones their server marks read-only.
		return planned("call", map[string]any{"tool": c.Name, "operation": spec.Label, "arguments": args}), nil
	}
	creds, err := a.Store.Credentials(ctx, in.Tenant, c.ID)
	if err != nil {
		return NodeResult{}, err
	}
	headers := map[string]string{}
	for k, v := range creds.Headers {
		headers[k] = v
	}
	if creds.OAuth != nil {
		token, err := oauth.Token(ctx, a.Store, in.Tenant, c.ID, false)
		if err != nil {
			return NodeResult{}, oauthError(err)
		}
		headers["Authorization"] = "Bearer " + token
	}
	client, err := catalog.NewMCPClient(ctx, c.BaseURL, headers, httpTimeout)
	if err != nil {
		return NodeResult{}, mcpError(err)
	}
	tool, _ := spec.Exec["tool"].(string)
	result, err := client.CallTool(ctx, tool, args)
	if err != nil {
		return NodeResult{}, mcpError(err)
	}
	if size, _ := json.Marshal(result); len(size) > maxBodyBytes {
		return NodeResult{}, fatal("tool result is larger than %d bytes", maxBodyBytes)
	}
	return NodeResult{Output: map[string]any{"result": result}}, nil
}

func mcpError(err error) error {
	var te *catalog.ToolError
	if errors.As(err, &te) {
		return fatal("%s", te)
	}
	var he *catalog.HTTPError
	if errors.As(err, &he) && !retryableStatus(he.Status) {
		return fatal("%s", he)
	}
	return transportError(err)
}

// coerceArgs converts text config to the types the tool's schema declares.
func coerceArgs(cfg map[string]any, spec compiler.CatalogNode) (map[string]any, error) {
	types, _ := spec.Exec["types"].(map[string]any)
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		typ, _ := types[k].(string)
		s, isStr := v.(string)
		if !isStr {
			out[k] = v
			continue
		}
		switch typ {
		case "number", "integer":
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return nil, fmt.Errorf("argument %q must be a number, got %q", k, s)
			}
			out[k] = f
		case "boolean":
			b, err := strconv.ParseBool(s)
			if err != nil {
				return nil, fmt.Errorf("argument %q must be true or false, got %q", k, s)
			}
			out[k] = b
		case "object", "array":
			var parsed any
			if err := json.Unmarshal([]byte(s), &parsed); err != nil {
				return nil, fmt.Errorf("argument %q must be JSON: %w", k, err)
			}
			out[k] = parsed
		default:
			out[k] = s
		}
	}
	return out, nil
}
