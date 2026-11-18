// Package client is the typed Go client for an apex-engine server. It uses only
// the standard library.
//
//	c := client.New("http://localhost:8700", apiKey)
//	res, _ := c.Compile(ctx, client.CompileRequest{Prompt: "...", CatalogIDs: ids})
//	run, _ := c.StartRun(ctx, client.RunRequest{DAG: res.Plan.DAG(), CatalogIDs: ids})
//	run, _ = c.WaitRun(ctx, run.ID, client.WaitOptions{})
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one engine as one tenant.
type Client struct {
	BaseURL string
	APIKey  string
	// HTTP is the client used for requests. It must not set a Timeout shorter
	// than a compile, which can take minutes; use contexts for deadlines.
	HTTP *http.Client
	// Tenant, Actor and Role say whom a platform key is acting for. Leave them
	// empty with an ordinary key, which already belongs to one tenant.
	Tenant, Actor, Role string
}

// As returns a copy of the client that acts as one end user with one role
// (viewer, approver, editor, operator or admin). The engine records the actor
// against everything the request does: who started a run, who approved a step.
// It needs a platform key, or a client from engine.Client.
func (c *Client) As(actor, role string) *Client {
	cp := *c
	cp.Actor, cp.Role = actor, role
	return &cp
}

// ForTenant returns a copy of a platform-key client that acts for another tenant.
func (c *Client) ForTenant(tenant string) *Client {
	cp := *c
	cp.Tenant = tenant
	return &cp
}

// New returns a client for the engine at baseURL.
func New(baseURL, apiKey string) *Client {
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), APIKey: apiKey, HTTP: &http.Client{}}
}

// Error is a failure the engine reported.
type Error struct {
	Status  int
	Code    string
	Message string
	// Problems lists each validation failure when Code is invalid_dag.
	Problems []string
}

func (e *Error) Error() string {
	if len(e.Problems) > 0 {
		return fmt.Sprintf("%s: %s", e.Message, strings.Join(e.Problems, "; "))
	}
	return e.Message
}

// IsNotFound reports whether err means the resource does not exist for this tenant.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, body, into any) error {
	resp, err := c.send(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if into == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// send returns a successful response, or the engine's error.
func (c *Client) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	for name, v := range map[string]string{"X-Apex-Tenant": c.Tenant, "X-Apex-Actor": c.Actor, "X-Apex-Role": c.Role} {
		if v != "" {
			req.Header.Set(name, v)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	var failure struct {
		Error struct {
			Code     string   `json:"code"`
			Message  string   `json:"message"`
			Problems []string `json:"problems"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(raw, &failure)
	msg := failure.Error.Message
	if msg == "" {
		msg = fmt.Sprintf("engine answered HTTP %d", resp.StatusCode)
	}
	return nil, &Error{Status: resp.StatusCode, Code: failure.Error.Code, Message: msg, Problems: failure.Error.Problems}
}

/* ---------------------------------- graphs --------------------------------- */

// Node is one step of a graph.
type Node struct {
	Key       string         `json:"key"`
	Type      string         `json:"type"`
	Name      string         `json:"name"`
	DependsOn []string       `json:"dependsOn"`
	Config    map[string]any `json:"config"`
}

// DAG is a runnable graph.
type DAG struct {
	Nodes        []Node         `json:"nodes"`
	DefaultInput map[string]any `json:"defaultInput,omitempty"`
}

// Plan is a compiled graph with its metadata.
type Plan struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	Tags         []string       `json:"tags,omitempty"`
	Nodes        []Node         `json:"nodes"`
	DefaultInput map[string]any `json:"defaultInput"`
	Schedule     *string        `json:"schedule,omitempty"`
}

// DAG returns the plan's runnable graph.
func (p Plan) DAG() DAG { return DAG{Nodes: p.Nodes, DefaultInput: p.DefaultInput} }

// CompileRequest asks for a prompt to be compiled over some tools.
type CompileRequest struct {
	Prompt     string   `json:"prompt"`
	CatalogIDs []string `json:"catalogIds"`
	Name       string   `json:"name,omitempty"`
	// Base makes the prompt a change to an existing plan ("also post it to
	// Slack", "only orders over 100"). Everything the prompt does not mention
	// is kept, and the result carries a Diff.
	Base *Plan `json:"base,omitempty"`
}

// Stage is one compiler stage's progress or outcome.
type Stage struct {
	Stage      string `json:"stage"`
	Status     string `json:"status"`
	Detail     string `json:"detail"`
	DurationMs int64  `json:"durationMs,omitempty"`
}

// Trace records how a compile went.
type Trace struct {
	DurationMs int64             `json:"durationMs"`
	Stages     []Stage           `json:"stages"`
	Repairs    int               `json:"repairs"`
	Models     map[string]string `json:"models"`
}

// CompileResult is a validated plan and how it was made.
type CompileResult struct {
	Plan  Plan  `json:"plan"`
	Trace Trace `json:"trace"`
	// Review says in plain words what the plan does; show it to whoever must
	// agree to the plan.
	Review Review `json:"review"`
	// Diff is set when the request had a Base: what the change did, by step key.
	Diff *Diff `json:"diff,omitempty"`
}

// ReviewStep says in plain words what one step does.
type ReviewStep struct {
	Node string `json:"node"`
	// Kind is start, read, write, delete, action, model, approval, loop, gate,
	// wait or transform. write, delete and action can change something outside
	// the engine.
	Kind string `json:"kind"`
	Text string `json:"text"`
	// Detail is the exact rule behind a step whose words cannot carry it: the
	// expression of a transform, the prompt of a model step.
	Detail string `json:"detail,omitempty"`
	Tool   string `json:"tool,omitempty"`
	// PerItem means the step runs once for each item of a loop.
	PerItem bool `json:"perItem"`
	// AfterApproval means a person is asked before the step can run.
	AfterApproval bool `json:"afterApproval"`
}

// Review describes a plan for a person: the steps in order, how many read and
// how many change something, and which changes happen with nobody asked.
type Review struct {
	Steps     []ReviewStep `json:"steps"`
	Reads     int          `json:"reads"`
	Changes   int          `json:"changes"`
	Approvals int          `json:"approvals"`
	Warnings  []string     `json:"warnings"`
}

// Diff says what a change did to a plan, by step key.
type Diff struct {
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
	Changed   []string `json:"changed"`
	Unchanged int      `json:"unchanged"`
}

// Review validates a graph and, when it is valid, describes it in plain words.
// Use it for a graph you stored or edited; Compile already returns one.
func (c *Client) Review(ctx context.Context, dag DAG, catalogIDs []string) (review *Review, problems []string, err error) {
	var out struct {
		Problems []string `json:"problems"`
		Review   *Review  `json:"review"`
	}
	err = c.do(ctx, http.MethodPost, "/v1/validate", map[string]any{"dag": dag, "catalogIds": orEmpty(catalogIDs)}, &out)
	return out.Review, out.Problems, err
}

// Compile turns a prompt into a validated plan. An error means there is no plan;
// the engine never substitutes a fallback graph.
func (c *Client) Compile(ctx context.Context, req CompileRequest) (*CompileResult, error) {
	var out CompileResult
	return &out, c.do(ctx, http.MethodPost, "/v1/compile", req, &out)
}

// CompileStream is Compile with progress: onStage sees each stage as it starts
// and ends. It may be nil.
func (c *Client) CompileStream(ctx context.Context, req CompileRequest, onStage func(Stage)) (*CompileResult, error) {
	resp, err := c.send(ctx, http.MethodPost, "/v1/compile/stream", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var (
		event  string
		result *CompileResult
	)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 32<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := []byte(strings.TrimPrefix(line, "data: "))
			switch event {
			case "stage":
				var st Stage
				if err := json.Unmarshal(data, &st); err != nil {
					return nil, err
				}
				if onStage != nil {
					onStage(st)
				}
			case "result":
				result = &CompileResult{}
				if err := json.Unmarshal(data, result); err != nil {
					return nil, err
				}
			case "error":
				var failure struct {
					Message string `json:"message"`
				}
				_ = json.Unmarshal(data, &failure)
				return nil, &Error{Status: http.StatusUnprocessableEntity, Code: "compile_failed", Message: failure.Message}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("the compile stream ended without a result")
	}
	return result, nil
}

// Validate checks a graph without running it. Problems is empty when it is valid.
func (c *Client) Validate(ctx context.Context, dag DAG, catalogIDs []string) (problems []string, err error) {
	var out struct {
		Problems []string `json:"problems"`
	}
	err = c.do(ctx, http.MethodPost, "/v1/validate", map[string]any{"dag": dag, "catalogIds": orEmpty(catalogIDs)}, &out)
	return out.Problems, err
}

func orEmpty(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

/* ----------------------------------- runs ---------------------------------- */

// Run statuses.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusWaiting   = "waiting_approval"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Run is one execution of a graph.
type Run struct {
	ID         string         `json:"id"`
	Status     string         `json:"status"`
	Name       string         `json:"name"`
	WorkflowID *string        `json:"workflowId"`
	Source     string         `json:"source"`
	CreatedAt  time.Time      `json:"createdAt"`
	FinishedAt *time.Time     `json:"finishedAt"`
	Error      *string        `json:"error"`
	Input      map[string]any `json:"input"`
	CatalogIDs []string       `json:"catalogIds"`
	Nodes      []NodeRun      `json:"nodes,omitempty"`
	DAG        []Node         `json:"dag,omitempty"`
	// StartedBy is the key name, "webhook" or "schedule".
	StartedBy string `json:"startedBy"`
	// ResumeOf is the failed run this one continues, if any.
	ResumeOf *string `json:"resumeOf"`
	// FailedItems counts loop items that failed in a loop set to continue. A
	// run can succeed with failed items, so check it.
	FailedItems int `json:"failedItems"`
	// Preview marks a run that made its reads and only recorded its writes.
	Preview bool `json:"preview"`
}

// PlannedAction is one thing a preview run would have done.
type PlannedAction struct {
	Node string `json:"node"`
	Name string `json:"name"`
	Type string `json:"type"`
	// Item is the loop item the action is for; nil outside a loop.
	Item *int `json:"item"`
	// Kind is "call" (a write that was not sent), "approval" (a question that
	// was not asked; the preview carried on as if the answer were yes), "wait"
	// (a pause that was skipped) or "unknown" (the step needs the result of a
	// write the preview did not send, so it cannot say what it would do).
	Kind string `json:"kind"`
	// Detail holds tool, operation, method, url and body for a call; message
	// for an approval; seconds for a wait; reason for unknown.
	Detail map[string]any `json:"detail"`
}

// PreviewResult is everything a preview run would have done.
type PreviewResult struct {
	RunID     string          `json:"runId"`
	Status    string          `json:"status"`
	Error     *string         `json:"error"`
	Done      bool            `json:"done"`
	Actions   []PlannedAction `json:"actions"`
	Counts    map[string]int  `json:"counts"`
	Truncated bool            `json:"truncated"`
}

// Preview returns what a preview run would have done. Call it once the run is
// done (WaitRun); before that the list is still growing.
func (c *Client) Preview(ctx context.Context, runID string) (*PreviewResult, error) {
	var out PreviewResult
	if err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(runID)+"/preview", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteTenant removes everything this client's tenant has: tools and their
// credentials, workflows, runs, schedules, keys and the audit log. Running work
// is stopped first. It cannot be undone. confirm must be the tenant's name.
func (c *Client) DeleteTenant(ctx context.Context, confirm string) error {
	return c.do(ctx, http.MethodDelete, "/v1/tenant?confirm="+url.QueryEscape(confirm), nil, nil)
}

// Done reports whether the run has reached a final status.
func (r *Run) Done() bool { return r.FinishedAt != nil }

// Waiting returns the steps currently waiting for a person.
func (r *Run) Waiting() []NodeRun {
	var out []NodeRun
	for _, n := range r.Nodes {
		if n.Status == StatusWaiting {
			out = append(out, n)
		}
	}
	return out
}

// Node returns the named step's state, or nil.
func (r *Run) Node(key string) *NodeRun {
	for i := range r.Nodes {
		if r.Nodes[i].Key == key {
			return &r.Nodes[i]
		}
	}
	return nil
}

// NodeRun is one step's state in a run.
type NodeRun struct {
	Key        string     `json:"key"`
	Type       string     `json:"type"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	Output     any        `json:"output"`
	Error      *string    `json:"error"`
	Attempts   int        `json:"attempts"`
	// Reused means the result was carried over from the run this one resumes.
	Reused bool `json:"reused,omitempty"`
	// ItemsSucceeded and ItemsFailed count this step's loop items.
	ItemsSucceeded int `json:"itemsSucceeded,omitempty"`
	ItemsFailed    int `json:"itemsFailed,omitempty"`
}

// RunRequest starts a graph.
type RunRequest struct {
	DAG        DAG            `json:"dag"`
	CatalogIDs []string       `json:"catalogIds"`
	Input      map[string]any `json:"input,omitempty"`
	// IdempotencyKey makes a repeated request return the first run.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	Name           string `json:"name,omitempty"`
	// CallbackURL receives this run's signed events. See VerifySignature.
	CallbackURL string `json:"callbackUrl,omitempty"`
	// Preview does every read for real and records every write, approval and
	// wait instead of doing it. Read the result with Client.Preview.
	Preview bool `json:"preview,omitempty"`
}

// StartRun validates a graph and starts it.
func (c *Client) StartRun(ctx context.Context, req RunRequest) (*Run, error) {
	req.CatalogIDs = orEmpty(req.CatalogIDs)
	var out Run
	return &out, c.do(ctx, http.MethodPost, "/v1/runs", req, &out)
}

// GetRun returns a run with its steps and graph.
func (c *Client) GetRun(ctx context.Context, id string) (*Run, error) {
	var out Run
	return &out, c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id), nil, &out)
}

// RunFilter narrows ListRuns. The zero value lists recent runs.
type RunFilter struct {
	Limit            int
	Status, Workflow string
}

// ListRuns returns recent runs, without their steps.
func (c *Client) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	q := url.Values{}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Status != "" {
		q.Set("status", f.Status)
	}
	if f.Workflow != "" {
		q.Set("workflowId", f.Workflow)
	}
	var out struct {
		Runs []Run `json:"runs"`
	}
	return out.Runs, c.do(ctx, http.MethodGet, "/v1/runs?"+q.Encode(), nil, &out)
}

// CancelRun stops a run.
func (c *Client) CancelRun(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(id)+"/cancel", map[string]any{}, nil)
}

// Decide answers a step waiting for approval.
func (c *Client) Decide(ctx context.Context, runID, nodeKey string, approved bool, note string) error {
	return c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/nodes/"+url.PathEscape(nodeKey)+"/approve",
		map[string]any{"approved": approved, "note": note}, nil)
}

// WaitOptions controls WaitRun.
type WaitOptions struct {
	// Poll is the time between checks. Default one second.
	Poll time.Duration
	// StopAtApproval also returns when the run needs a person.
	StopAtApproval bool
}

// WaitRun blocks until the run finishes, or ctx ends. A run that failed is
// returned with a nil error; check its Status. Long waits are better served by
// RunRequest.CallbackURL.
func (c *Client) WaitRun(ctx context.Context, id string, opts WaitOptions) (*Run, error) {
	poll := opts.Poll
	if poll <= 0 {
		poll = time.Second
	}
	for {
		run, err := c.GetRun(ctx, id)
		if err != nil {
			return nil, err
		}
		if run.Done() || (opts.StopAtApproval && run.Status == StatusWaiting) {
			return run, nil
		}
		select {
		case <-ctx.Done():
			return run, ctx.Err()
		case <-time.After(poll):
		}
	}
}

/* ---------------------------------- events --------------------------------- */

// Event names sent to a callback URL.
const (
	EventWaitingApproval = "run.waiting_approval"
	EventFinished        = "run.finished"
)

// Event is the JSON body posted to a callback URL. Delivery is at least once:
// the Delivery value is the same on every retry of one event, so drop repeats.
type Event struct {
	Event    string `json:"event"`
	RunID    string `json:"runId"`
	Status   string `json:"status"`
	Error    string `json:"error"`
	Node     string `json:"node"`
	Message  string `json:"message"`
	Delivery string `json:"delivery"`
	At       string `json:"at"`
}

// CallbackSecret returns the hex key that signs this tenant's events.
func (c *Client) CallbackSecret(ctx context.Context) (string, error) {
	var out struct {
		Secret string `json:"secret"`
	}
	return out.Secret, c.do(ctx, http.MethodGet, "/v1/callback-secret", nil, &out)
}

// VerifySignature checks an X-Apex-Signature header against the raw request body.
func VerifySignature(secret string, body []byte, header string) bool {
	key, err := hex.DecodeString(secret)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return hmac.Equal([]byte(header), []byte("sha256="+hex.EncodeToString(mac.Sum(nil))))
}

/* --------------------------------- catalogs -------------------------------- */

// Catalog is an imported tool source.
type Catalog struct {
	ID                string        `json:"id"`
	Name              string        `json:"name"`
	Kind              string        `json:"kind"`
	BaseURL           string        `json:"baseUrl"`
	NodeCount         int           `json:"nodeCount"`
	HasCredentials    bool          `json:"hasCredentials"`
	CredentialHeaders []string      `json:"credentialHeaders"`
	CreatedAt         time.Time     `json:"createdAt"`
	Nodes             []CatalogNode `json:"nodes,omitempty"`
	// Auth is none, headers, oauth2, oauth2_pending or piece.
	Auth string `json:"auth"`
}

// CatalogNode is one tool a plan may use.
type CatalogNode struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Outputs     []string `json:"outputs"`
}

// CatalogSource says where a catalog's tools come from. Type is "openapi" or
// "mcp". Spec is an OpenAPI document, as an object or a YAML string.
type CatalogSource struct {
	Type    string            `json:"type"`
	URL     string            `json:"url,omitempty"`
	Spec    any               `json:"spec,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Piece is an Activepieces piece name, for Type "piece", and Auth the
	// connection value that piece expects.
	Piece string `json:"piece,omitempty"`
	Auth  any    `json:"auth,omitempty"`
}

// CatalogRequest imports a tool source.
type CatalogRequest struct {
	Name    string        `json:"name"`
	Source  CatalogSource `json:"source"`
	BaseURL string        `json:"baseUrl,omitempty"`
}

// CreateCatalog imports a tool source.
func (c *Client) CreateCatalog(ctx context.Context, req CatalogRequest) (*Catalog, error) {
	var out Catalog
	return &out, c.do(ctx, http.MethodPost, "/v1/catalogs", req, &out)
}

// ListCatalogs returns the tenant's tool sources, without their tools.
func (c *Client) ListCatalogs(ctx context.Context) ([]Catalog, error) {
	var out struct {
		Catalogs []Catalog `json:"catalogs"`
	}
	return out.Catalogs, c.do(ctx, http.MethodGet, "/v1/catalogs", nil, &out)
}

// GetCatalog returns a tool source with its tools.
func (c *Client) GetCatalog(ctx context.Context, id string) (*Catalog, error) {
	var out Catalog
	return &out, c.do(ctx, http.MethodGet, "/v1/catalogs/"+url.PathEscape(id), nil, &out)
}

// DeleteCatalog removes a tool source.
func (c *Client) DeleteCatalog(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/catalogs/"+url.PathEscape(id), nil, nil)
}

// SetCredentials replaces the headers sent to a tool source. They are write-only.
func (c *Client) SetCredentials(ctx context.Context, id string, headers map[string]string) error {
	return c.do(ctx, http.MethodPut, "/v1/catalogs/"+url.PathEscape(id)+"/credentials", map[string]any{"headers": headers}, nil)
}

/* --------------------------------- workflows ------------------------------- */

// Workflow is a saved graph.
type Workflow struct {
	ID           string         `json:"id,omitempty"`
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	Prompt       string         `json:"prompt"`
	Nodes        []Node         `json:"nodes,omitempty"`
	DefaultInput map[string]any `json:"defaultInput"`
	CatalogIDs   []string       `json:"catalogIds"`
	// CallbackURL receives the events of every run of this workflow.
	CallbackURL string     `json:"callbackUrl"`
	WebhookPath *string    `json:"webhookPath,omitempty"`
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
}

// CreateWorkflow saves a graph. A graph that fails validation is refused.
func (c *Client) CreateWorkflow(ctx context.Context, w Workflow) (*Workflow, error) {
	w.CatalogIDs = orEmpty(w.CatalogIDs)
	var out Workflow
	return &out, c.do(ctx, http.MethodPost, "/v1/workflows", w, &out)
}

// UpdateWorkflow replaces a saved graph.
func (c *Client) UpdateWorkflow(ctx context.Context, id string, w Workflow) (*Workflow, error) {
	w.CatalogIDs = orEmpty(w.CatalogIDs)
	var out Workflow
	return &out, c.do(ctx, http.MethodPut, "/v1/workflows/"+url.PathEscape(id), w, &out)
}

// GetWorkflow returns a saved graph.
func (c *Client) GetWorkflow(ctx context.Context, id string) (*Workflow, error) {
	var out Workflow
	return &out, c.do(ctx, http.MethodGet, "/v1/workflows/"+url.PathEscape(id), nil, &out)
}

// ListWorkflows returns the tenant's saved graphs.
func (c *Client) ListWorkflows(ctx context.Context) ([]Workflow, error) {
	var out struct {
		Workflows []Workflow `json:"workflows"`
	}
	return out.Workflows, c.do(ctx, http.MethodGet, "/v1/workflows", nil, &out)
}

// DeleteWorkflow removes a saved graph and its schedules. Its runs stay.
func (c *Client) DeleteWorkflow(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/workflows/"+url.PathEscape(id), nil, nil)
}

// WorkflowRunRequest starts a saved graph. An empty CallbackURL uses the workflow's.
type WorkflowRunRequest struct {
	Input          map[string]any `json:"input,omitempty"`
	IdempotencyKey string         `json:"idempotencyKey,omitempty"`
	CallbackURL    string         `json:"callbackUrl,omitempty"`
	Preview        bool           `json:"preview,omitempty"`
}

// RunWorkflow starts a saved graph.
func (c *Client) RunWorkflow(ctx context.Context, id string, req WorkflowRunRequest) (*Run, error) {
	var out Run
	return &out, c.do(ctx, http.MethodPost, "/v1/workflows/"+url.PathEscape(id)+"/runs", req, &out)
}

// EnableWebhook turns a workflow's inbound webhook on, or replaces its URL, and
// returns the path to POST to.
func (c *Client) EnableWebhook(ctx context.Context, id string) (string, error) {
	var out struct {
		WebhookPath string `json:"webhookPath"`
	}
	return out.WebhookPath, c.do(ctx, http.MethodPut, "/v1/workflows/"+url.PathEscape(id)+"/webhook", nil, &out)
}

// DisableWebhook turns a workflow's inbound webhook off.
func (c *Client) DisableWebhook(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/workflows/"+url.PathEscape(id)+"/webhook", nil, nil)
}

/* --------------------------------- schedules ------------------------------- */

// Schedule is a recurring run. Cron is in UTC.
type Schedule struct {
	ID         string    `json:"id"`
	Cron       string    `json:"cron"`
	CreatedAt  time.Time `json:"createdAt"`
	WorkflowID *string   `json:"workflowId"`
}

// ScheduleWorkflow runs a saved workflow's latest version on a cron schedule.
func (c *Client) ScheduleWorkflow(ctx context.Context, workflowID, cron string) (*Schedule, error) {
	var out Schedule
	return &out, c.do(ctx, http.MethodPost, "/v1/schedules", map[string]any{"cron": cron, "workflowId": workflowID}, &out)
}

// ListSchedules returns the tenant's schedules.
func (c *Client) ListSchedules(ctx context.Context) ([]Schedule, error) {
	var out struct {
		Schedules []Schedule `json:"schedules"`
	}
	return out.Schedules, c.do(ctx, http.MethodGet, "/v1/schedules", nil, &out)
}

// DeleteSchedule stops a schedule.
func (c *Client) DeleteSchedule(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/schedules/"+url.PathEscape(id), nil, nil)
}

/* ---------------------------------- status --------------------------------- */

// Status says whether the engine can compile, and on which models.
type Status struct {
	Models struct {
		Ready   bool   `json:"ready"`
		Problem string `json:"problem"`
		Map     string `json:"map"`
		Reduce  string `json:"reduce"`
		Agent   string `json:"agent"`
	} `json:"models"`
}

// InstallDemo adds the sample tools and workflow, and returns the workflow's id.
// It needs an engine started with the sample tools.
func (c *Client) InstallDemo(ctx context.Context) (string, error) {
	var out struct {
		WorkflowID string `json:"workflowId"`
	}
	return out.WorkflowID, c.do(ctx, http.MethodPost, "/v1/demo", map[string]any{}, &out)
}

// Status reports the engine's model configuration.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var out Status
	return &out, c.do(ctx, http.MethodGet, "/v1/status", nil, &out)
}

/* ----------------------------- resume and items ----------------------------- */

// RetryRun continues a failed or cancelled run as a new run. Steps and loop
// items that succeeded are not executed again. Calling it twice for the same
// run returns the same new run.
func (c *Client) RetryRun(ctx context.Context, id string) (*Run, error) {
	var out Run
	err := c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(id)+"/retry", nil, &out)
	return &out, err
}

// ItemRun is one loop item of one step.
type ItemRun struct {
	Index      int        `json:"index"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	Output     any        `json:"output"`
	Error      *string    `json:"error"`
	Attempts   int        `json:"attempts"`
}

// ListItems returns the loop items of one step. An empty status means all.
func (c *Client) ListItems(ctx context.Context, runID, nodeKey, status string) ([]ItemRun, error) {
	var all []ItemRun
	for offset := 0; ; offset += 500 {
		var out struct {
			Items []ItemRun `json:"items"`
		}
		q := url.Values{"limit": {"500"}, "offset": {strconv.Itoa(offset)}}
		if status != "" {
			q.Set("status", status)
		}
		if err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(runID)+"/nodes/"+url.PathEscape(nodeKey)+"/items?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Items...)
		if len(out.Items) < 500 {
			return all, nil
		}
	}
}

/* -------------------------------- credentials ------------------------------- */

// OAuthClient is an OAuth 2.0 client for a tool source.
type OAuthClient struct {
	// Grant is "client_credentials" or "refresh_token". Leave it empty for
	// StartOAuth, which runs a consent screen.
	Grant        string            `json:"grant,omitempty"`
	TokenURL     string            `json:"tokenUrl"`
	AuthURL      string            `json:"authUrl,omitempty"`
	ClientID     string            `json:"clientId"`
	ClientSecret string            `json:"clientSecret,omitempty"`
	Scope        string            `json:"scope,omitempty"`
	Audience     string            `json:"audience,omitempty"`
	RefreshToken string            `json:"refreshToken,omitempty"`
	ClientAuth   string            `json:"clientAuth,omitempty"`
	AuthParams   map[string]string `json:"authParams,omitempty"`
}

// Credentials is how the engine authenticates to a tool source. It is
// write-only: the engine never returns it.
type Credentials struct {
	Headers map[string]string `json:"headers,omitempty"`
	Query   map[string]string `json:"query,omitempty"`
	OAuth   *OAuthClient      `json:"oauth,omitempty"`
	Piece   any               `json:"piece,omitempty"`
}

// SetAuth replaces a tool source's credentials. OAuth settings are tried
// before they are saved, so wrong ones come back as an error here.
func (c *Client) SetAuth(ctx context.Context, catalogID string, creds Credentials) (*Catalog, error) {
	var out Catalog
	err := c.do(ctx, http.MethodPut, "/v1/catalogs/"+url.PathEscape(catalogID)+"/credentials", creds, &out)
	return &out, err
}

// OAuthStart is where to send a person to grant access.
type OAuthStart struct {
	AuthorizeURL string `json:"authorizeUrl"`
	// RedirectURL must be registered with the provider.
	RedirectURL      string `json:"redirectUrl"`
	ExpiresInSeconds int    `json:"expiresInSeconds"`
}

// StartOAuth begins a consent for a tool source. Open AuthorizeURL in a
// browser; when the person finishes, the catalog's Auth becomes "oauth2".
func (c *Client) StartOAuth(ctx context.Context, catalogID string, client OAuthClient) (*OAuthStart, error) {
	var out OAuthStart
	err := c.do(ctx, http.MethodPost, "/v1/catalogs/"+url.PathEscape(catalogID)+"/oauth/authorize", client, &out)
	return &out, err
}

/* ----------------------------------- pieces --------------------------------- */

// Piece is an Activepieces piece the engine can add as a tool source.
type Piece struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName"`
	Description string   `json:"description"`
	Version     string   `json:"version"`
	Categories  []string `json:"categories"`
	ActionCount int      `json:"actionCount"`
}

// ListPieces returns the installed pieces whose name contains q.
func (c *Client) ListPieces(ctx context.Context, q string) ([]Piece, error) {
	var out struct {
		Pieces []Piece `json:"pieces"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/pieces?q="+url.QueryEscape(q), nil, &out)
	return out.Pieces, err
}

/* ----------------------------------- access --------------------------------- */

// Roles a key can have.
const (
	RoleViewer   = "viewer"
	RoleApprover = "approver"
	RoleEditor   = "editor"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

// APIKey is a key as the engine lists it. Key is set only by CreateKey.
type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	Prefix     string     `json:"prefix"`
	CreatedBy  string     `json:"createdBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
	Key        string     `json:"key,omitempty"`
}

// CreateKey makes a key for a person or a system. The returned Key is shown
// once; the engine keeps only its hash.
func (c *Client) CreateKey(ctx context.Context, name, role string) (*APIKey, error) {
	var out APIKey
	err := c.do(ctx, http.MethodPost, "/v1/keys", map[string]string{"name": name, "role": role}, &out)
	return &out, err
}

// ListKeys returns this tenant's keys.
func (c *Client) ListKeys(ctx context.Context) ([]APIKey, error) {
	var out struct {
		Keys []APIKey `json:"keys"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/keys", nil, &out)
	return out.Keys, err
}

// RevokeKey stops a key working.
func (c *Client) RevokeKey(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/keys/"+url.PathEscape(id), nil, nil)
}

// AuditEntry is one recorded action.
type AuditEntry struct {
	ID     int64          `json:"id"`
	At     time.Time      `json:"at"`
	Actor  string         `json:"actor"`
	Action string         `json:"action"`
	Target string         `json:"target"`
	Detail map[string]any `json:"detail"`
}

// AuditFilter narrows ListAudit. Action matches as a prefix.
type AuditFilter struct {
	Action, Actor, Target string
	Before                int64
	Limit                 int
}

// ListAudit returns audit entries, newest first.
func (c *Client) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	q := url.Values{}
	if f.Action != "" {
		q.Set("action", f.Action)
	}
	if f.Actor != "" {
		q.Set("actor", f.Actor)
	}
	if f.Target != "" {
		q.Set("target", f.Target)
	}
	if f.Before > 0 {
		q.Set("before", strconv.FormatInt(f.Before, 10))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	var out struct {
		Entries []AuditEntry `json:"entries"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/audit?"+q.Encode(), nil, &out)
	return out.Entries, err
}

// Whoami is the name and role of the key this client uses.
type Whoami struct {
	Actor string `json:"actor"`
	Role  string `json:"role"`
}

// Whoami reports which key this client is using.
func (c *Client) Whoami(ctx context.Context) (*Whoami, error) {
	var out Whoami
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &out)
	return &out, err
}
