// Workflow compatibility: put command-producing changes behind GetVersion and
// keep the old path for recorded versions, including DefaultVersion for
// histories without a marker. Keep workflow and activity names stable, retain
// readers for old payloads, and replay the fixtures before shipping changes.
// ScheduledWorkflow has its own setup commands before entering RunWorkflow.
package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/satyanvm/apex-engine/internal/compiler"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// TaskQueue is the queue the engine's workers poll.
const TaskQueue = "apex-engine"

// This marker sits before the first DAG command so each run keeps the same
// runner version, including work it hasn't reached when its worker is replaced.
const runChangeID = "apex-run"
const runVersion = 1

const (
	maxParallel      = 8
	maxLoopItems     = 500
	approvalTimeout  = 7 * 24 * time.Hour
	nodeTimeout      = 10 * time.Minute
	heartbeatTimeout = 15 * time.Second
	maxWait          = 30 * 24 * time.Hour

	// A run's event history holds every node's input and output. Temporal ends a
	// workflow whose history passes 50 MB or about 51,000 events, and that looks
	// like a crash. These limits fail the run first, with the reason.
	maxRunBytes      = 30 << 20
	maxRunActivities = 10000
)

// RunParams starts a run. The DAG travels in the workflow input, so a replay
// never depends on database state.
type RunParams struct {
	RunID      string
	Tenant     string
	Nodes      []compiler.GraphNode
	Input      map[string]any
	CatalogIDs []string
	// Name and WorkflowRef label the run. On a schedule, WorkflowRef also makes
	// each tick load the workflow's latest saved graph.
	Name        string
	WorkflowRef string
	// CallbackURL, when set, receives this run's events.
	CallbackURL string
	// ResumeOf is an earlier run of the same graph. Every node (and loop item)
	// that succeeded there is reused instead of being executed again.
	ResumeOf string
	// IdemRoot is the run id idempotency keys are built from. A resumed run
	// keeps the root of the run it resumes, so a call that was in flight when
	// the first run failed carries the same key the second time.
	IdemRoot string
	// Preview makes the run do its reads and only record its writes, approvals
	// and waits, so a person can see what it would do before it does it.
	Preview bool
}

// ApprovalSignal is sent to a run waiting on a logic.approval node. Token names
// the one wait it answers: a signal for any other wait is ignored, so a repeated
// decision can never approve the next item of a loop.
type ApprovalSignal struct {
	Approved bool
	Note     string
	Token    string
	By       string
}

// ApprovalToken identifies one wait of an approval node: the node, and the loop
// item when it is inside a loop.
func ApprovalToken(key string, hasItem bool, index int) string {
	if !hasItem {
		return key
	}
	return key + "#" + strconv.Itoa(index)
}

// SignalName is the per-node approval signal.
func SignalName(key string) string { return "approve:" + key }

// NodeInput is one node execution.
type NodeInput struct {
	RunID      string
	Tenant     string
	Node       compiler.GraphNode
	Input      map[string]any
	Upstream   map[string]map[string]any
	HasItem    bool
	Item       any
	Index      int
	Loop       map[string]any
	CatalogIDs []string
	ResumeOf   string
	IdemRoot   string
	// ApprovalToken is set for logic.approval nodes; see ApprovalToken.
	ApprovalToken string
	Preview       bool
}

// NodeResult is what a node produced.
type NodeResult struct {
	Output map[string]any
	Passed bool // gates only
	// Reused means the output was copied from the run being resumed.
	Reused bool
}

// FinishNodeInput records a node state the workflow decided. HasItem and Index
// name the loop item it is for.
type FinishNodeInput struct {
	RunID, Key, Status, Error string
	Output                    map[string]any
	HasItem                   bool
	Index                     int
}

// CopyPriorInput copies finished node rows from the run being resumed.
type CopyPriorInput struct {
	RunID, Prior string
	Keys         []string
}

// FinishRunInput closes a run.
type FinishRunInput struct{ RunID, Status, Error string }

// ScheduledInput creates the row for a run started by a schedule.
type ScheduledInput struct {
	RunID, Tenant, WorkflowID string
	Nodes                     []compiler.GraphNode
	Input                     map[string]any
	CatalogIDs                []string
	Name, WorkflowRef         string
}

// SavedGraph is a workflow as a scheduled tick sees it.
type SavedGraph struct {
	Name        string
	Nodes       []compiler.GraphNode
	Input       map[string]any
	CatalogIDs  []string
	CallbackURL string
}

var a *Activities // nil receiver: names only, for type-safe activity references

func isGate(t string) bool {
	return t == "logic.branch" || t == "logic.filter" || t == "logic.approval"
}

func ioCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 8, InitialInterval: time.Second},
	})
}

// eventCtx bounds how long a run keeps trying to tell its host something.
func eventCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 20 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 6, InitialInterval: 2 * time.Second, BackoffCoefficient: 2},
	})
}

func nodeCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: nodeTimeout,
		// A dead worker stops heartbeating, so its node is retried elsewhere in
		// seconds rather than after the whole node timeout.
		HeartbeatTimeout: heartbeatTimeout,
		RetryPolicy:      &temporal.RetryPolicy{MaximumAttempts: 3, InitialInterval: 2 * time.Second, BackoffCoefficient: 2},
	})
}

// RunWorkflow drives a DAG: one activity per node, independent branches in
// parallel, gates pruning their descendants, loops fanning out their body.
func RunWorkflow(ctx workflow.Context, p RunParams) error {
	workflow.GetVersion(ctx, runChangeID, workflow.DefaultVersion, runVersion)
	err := drive(ctx, p)
	status, msg := statusSucceeded, ""
	switch {
	case err == nil:
	case temporal.IsCanceledError(err):
		status = statusCancelled
	default:
		status, msg = statusFailed, err.Error()
	}
	// A disconnected context, so the row is closed even when the run was cancelled.
	dctx, _ := workflow.NewDisconnectedContext(ctx)
	if ferr := workflow.ExecuteActivity(ioCtx(dctx), a.FinishRun, FinishRunInput{RunID: p.RunID, Status: status, Error: msg}).Get(dctx, nil); ferr != nil {
		return ferr
	}
	if p.CallbackURL != "" {
		ev := EventInput{URL: p.CallbackURL, Tenant: p.Tenant, RunID: p.RunID, Event: EventFinished, Status: status, Error: msg, Delivery: p.RunID + ":finished"}
		// A receiver that stays down costs it the event, never the run.
		_ = workflow.ExecuteActivity(eventCtx(dctx), a.DeliverEvent, ev).Get(dctx, nil)
	}
	if status == statusCancelled {
		return err
	}
	return nil // a failed run is a finished workflow; the reason is on the run
}

// ScheduledWorkflow creates the run row, then behaves like RunWorkflow.
func ScheduledWorkflow(ctx workflow.Context, p RunParams) error {
	var id string
	if err := workflow.SideEffect(ctx, func(workflow.Context) any {
		return "run_s" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}).Get(&id); err != nil {
		return err
	}
	p.RunID = id
	if p.WorkflowRef != "" {
		var saved SavedGraph
		if err := workflow.ExecuteActivity(ioCtx(ctx), a.LoadSavedWorkflow, p.Tenant, p.WorkflowRef).Get(ctx, &saved); err != nil {
			return err
		}
		p.Name, p.Nodes, p.CatalogIDs, p.CallbackURL = saved.Name, saved.Nodes, saved.CatalogIDs, saved.CallbackURL
		p.Input = mergeInputs(saved.Input, p.Input)
	}
	in := ScheduledInput{RunID: id, Tenant: p.Tenant, WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID, Nodes: p.Nodes, Input: p.Input, CatalogIDs: p.CatalogIDs, Name: p.Name, WorkflowRef: p.WorkflowRef}
	if err := workflow.ExecuteActivity(ioCtx(ctx), a.CreateScheduledRun, in).Get(ctx, nil); err != nil {
		return err
	}
	return RunWorkflow(ctx, p)
}

func mergeInputs(defaults, extra map[string]any) map[string]any {
	out := make(map[string]any, len(defaults)+len(extra))
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

const (
	statusSucceeded = "succeeded"
	statusFailed    = "failed"
	statusCancelled = "cancelled"
)

type completion struct {
	key  string
	out  map[string]any
	err  error
	open bool
}

// runner drives one run. Workflow coroutines are cooperative, so its counters
// need no lock.
type runner struct {
	p         RunParams
	g         *graph
	bytes     int
	acts      int
	exhausted bool
}

func drive(ctx workflow.Context, p RunParams) error {
	if err := workflow.ExecuteActivity(ioCtx(ctx), a.MarkRunStarted, p.RunID).Get(ctx, nil); err != nil {
		return err
	}
	g, err := newGraph(p.Nodes)
	if err != nil {
		return err
	}
	r := &runner{p: p, g: g}
	var (
		outputs  = map[string]map[string]any{}
		state    = map[string]string{} // ""=pending, running, done, skipped
		pruned   = map[string]bool{}
		fanned   = map[string]bool{}
		inflight int
		firstErr error
		done     = workflow.NewBufferedChannel(ctx, len(p.Nodes))
	)
	for {
		if firstErr == nil && ctx.Err() == nil {
			for _, n := range g.ordered {
				if state[n.Key] != "" || fanned[n.Key] || !g.depsSettled(n, state, fanned) {
					continue
				}
				if pruned[n.Key] || g.allDepsSkipped(n, state) {
					state[n.Key] = "skipped"
					pruned[n.Key] = true
					for _, d := range g.descendantsOf(n.Key) {
						pruned[d] = true
					}
					if err := finishNode(ctx, FinishNodeInput{RunID: p.RunID, Key: n.Key, Status: "skipped", Output: map[string]any{"skipped": true}}); err != nil {
						return err
					}
					continue
				}
				if inflight == maxParallel {
					break
				}
				state[n.Key] = "running"
				inflight++
				node := n
				up := upstream(node, outputs)
				if node.Type == "logic.loop" {
					body := g.loopBody(node.Key)
					for _, k := range body {
						fanned[k] = true
					}
				}
				workflow.Go(ctx, func(gctx workflow.Context) {
					out, open, err := r.runNode(gctx, node, up)
					done.Send(gctx, completion{key: node.Key, out: out, err: err, open: open})
				})
			}
		}
		if inflight == 0 {
			break
		}
		var c completion
		done.Receive(ctx, &c)
		inflight--
		state[c.key] = "done"
		if c.err != nil {
			if firstErr == nil {
				firstErr = c.err
			}
			continue
		}
		outputs[c.key] = c.out
		if !c.open {
			for _, d := range g.descendantsOf(c.key) {
				pruned[d] = true
			}
		}
		if node := g.byKey[c.key]; node.Type == "logic.loop" {
			for _, k := range g.loopBody(c.key) {
				state[k] = "done"
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return temporal.NewCanceledError()
	}
	return firstErr
}

func upstream(n compiler.GraphNode, outputs map[string]map[string]any) map[string]map[string]any {
	up := make(map[string]map[string]any, len(n.DependsOn))
	for _, d := range n.DependsOn {
		up[d] = outputs[d]
	}
	return up
}

func finishNode(ctx workflow.Context, in FinishNodeInput) error {
	return workflow.ExecuteActivity(ioCtx(ctx), a.FinishNode, in).Get(ctx, nil)
}

// NodeScopeItem carries the loop context of a body node.
type NodeScopeItem struct {
	Has   bool
	Item  any
	Loop  map[string]any
	Index int
}

// runNode runs one scheduled node, including a loop's whole fan-out, and reports
// whether its descendants may run.
func (r *runner) runNode(ctx workflow.Context, n compiler.GraphNode, up map[string]map[string]any) (map[string]any, bool, error) {
	if n.Type != "logic.loop" {
		return r.step(ctx, n, up, NodeScopeItem{})
	}
	res, err := r.call(ctx, n, up, NodeScopeItem{}, "")
	if err != nil {
		return nil, false, err
	}
	body := r.g.loopBody(n.Key)
	if res.Reused {
		// The whole loop finished in the run being resumed. Carry its body rows over too.
		if len(body) > 0 {
			in := CopyPriorInput{RunID: r.p.RunID, Prior: r.p.ResumeOf, Keys: body}
			if err := workflow.ExecuteActivity(ioCtx(ctx), a.CopyPrior, in).Get(ctx, nil); err != nil {
				return nil, false, err
			}
		}
		return res.Output, true, nil
	}
	if err := r.fanOut(ctx, n, body, up, res.Output); err != nil {
		if !temporal.IsCanceledError(err) {
			_ = r.failNode(ctx, n.Key, NodeScopeItem{}, loopFailure(err))
		}
		return nil, false, err
	}
	if err := finishNode(ctx, FinishNodeInput{RunID: r.p.RunID, Key: n.Key, Status: statusSucceeded, Output: res.Output}); err != nil {
		return nil, false, err
	}
	return res.Output, true, nil
}

func loopFailure(err error) string {
	var le *loopError
	if errors.As(err, &le) {
		return le.msg
	}
	return "a step inside this loop failed"
}

// loopError is a failure of the loop itself rather than of a step inside it.
type loopError struct{ msg string }

func (e *loopError) Error() string { return e.msg }

// step runs one node that is not a loop: an approval, a wait, or an activity.
func (r *runner) step(ctx workflow.Context, n compiler.GraphNode, up map[string]map[string]any, it NodeScopeItem) (map[string]any, bool, error) {
	switch n.Type {
	case "logic.approval":
		return r.approval(ctx, n, up, it)
	case "logic.wait":
		return r.wait(ctx, n, up, it)
	}
	res, err := r.call(ctx, n, up, it, "")
	if err != nil {
		return nil, false, err
	}
	return res.Output, !isGate(n.Type) || res.Passed, nil
}

// call runs a node's activity, counting what it adds to the run's history.
func (r *runner) call(ctx workflow.Context, n compiler.GraphNode, up map[string]map[string]any, it NodeScopeItem, approvalToken string) (NodeResult, error) {
	var res NodeResult
	in := NodeInput{RunID: r.p.RunID, Tenant: r.p.Tenant, Node: n, Input: r.p.Input, Upstream: up, HasItem: it.Has, Item: it.Item, Index: it.Index,
		Loop: it.Loop, CatalogIDs: r.p.CatalogIDs, ResumeOf: r.p.ResumeOf, IdemRoot: r.p.IdemRoot, ApprovalToken: approvalToken, Preview: r.p.Preview}
	raw, err := json.Marshal(in)
	if err != nil || len(raw) > maxPayloadBytes {
		return res, r.failNode(ctx, n.Key, it, payloadMessage)
	}
	r.bytes += len(raw)
	r.acts++
	if r.bytes > maxRunBytes || r.acts > maxRunActivities {
		r.exhausted = true
		return res, r.failNode(ctx, n.Key, it, budgetMessage)
	}
	err = workflow.ExecuteActivity(nodeCtx(ctx), a.ExecuteNode, in).Get(ctx, &res)
	if err == nil {
		if out, merr := json.Marshal(res.Output); merr == nil {
			r.bytes += len(out)
		}
		return res, nil
	}
	if temporal.IsCanceledError(err) {
		return res, err
	}
	return res, r.failNode(ctx, n.Key, it, rootMessage(err))
}

func rootMessage(err error) string {
	var app *temporal.ApplicationError
	if errors.As(err, &app) {
		return app.Message()
	}
	return err.Error()
}

// maxPayloadBytes keeps every activity input under Temporal's 2 MB blob limit.
// Crossing that limit would wedge the workflow task instead of failing the node.
const maxPayloadBytes = 1500 << 10

func tooLarge(v any) bool {
	raw, err := json.Marshal(v)
	return err != nil || len(raw) > maxPayloadBytes
}

func (r *runner) failNode(ctx workflow.Context, key string, it NodeScopeItem, msg string) error {
	dctx, _ := workflow.NewDisconnectedContext(ctx)
	_ = finishNode(dctx, FinishNodeInput{RunID: r.p.RunID, Key: key, Status: statusFailed, Error: msg, HasItem: it.Has, Index: it.Index})
	return fmt.Errorf("node %s: %s", key, msg)
}

const (
	payloadMessage = "upstream data is too large to pass to this node (over 1.5 MB)"
	budgetMessage  = "this run has moved more data, or run more steps, than one run can hold (30 MB or 10,000 steps); " +
		"narrow what each step returns, or split the list across several runs"
)

// wait pauses the run on a durable timer. The activity only works out how long.
func (r *runner) wait(ctx workflow.Context, n compiler.GraphNode, up map[string]map[string]any, it NodeScopeItem) (map[string]any, bool, error) {
	res, err := r.call(ctx, n, up, it, "")
	if err != nil {
		return nil, false, err
	}
	if res.Reused {
		return res.Output, true, nil
	}
	secs, _ := res.Output["seconds"].(float64)
	d := time.Duration(secs * float64(time.Second))
	if d > maxWait {
		return nil, false, r.failNode(ctx, n.Key, it, "a wait can be at most 30 days")
	}
	if r.p.Preview { // a preview does not wait; it says how long the run would
		res.Output["preview"], res.Output["planned"] = true, map[string]any{"kind": "wait", "seconds": secs}
		d = 0
	}
	if d > 0 {
		if err := workflow.Sleep(ctx, d); err != nil {
			return nil, false, temporal.NewCanceledError()
		}
	}
	if err := finishNode(ctx, FinishNodeInput{RunID: r.p.RunID, Key: n.Key, Status: statusSucceeded, Output: res.Output, HasItem: it.Has, Index: it.Index}); err != nil {
		return nil, false, err
	}
	return res.Output, true, nil
}

func (r *runner) approval(ctx workflow.Context, n compiler.GraphNode, up map[string]map[string]any, it NodeScopeItem) (map[string]any, bool, error) {
	token := ApprovalToken(n.Key, it.Has, it.Index)
	res, err := r.call(ctx, n, up, it, token)
	if err != nil {
		return nil, false, err
	}
	if res.Reused { // decided in the run being resumed
		approved, _ := res.Output["approved"].(bool)
		return res.Output, approved, nil
	}
	p := r.p
	message, _ := res.Output["message"].(string)
	if p.Preview {
		// Nobody is asked. The preview shows the question and carries on as if
		// the answer were yes, so the steps behind it are previewed too.
		out := map[string]any{"approved": true, "note": "", "decidedBy": "preview", "message": message,
			"preview": true, "planned": map[string]any{"kind": "approval", "message": message}}
		if err := finishNode(ctx, FinishNodeInput{RunID: p.RunID, Key: n.Key, Status: statusSucceeded, Output: out, HasItem: it.Has, Index: it.Index}); err != nil {
			return nil, false, err
		}
		return out, true, nil
	}
	if err := workflow.ExecuteActivity(ioCtx(ctx), a.RefreshRunStatus, p.RunID).Get(ctx, nil); err != nil {
		return nil, false, err
	}
	if p.CallbackURL != "" {
		ev := EventInput{URL: p.CallbackURL, Tenant: p.Tenant, RunID: p.RunID, Event: EventApproval, Status: "waiting_approval", Node: n.Key, Message: message,
			Delivery: fmt.Sprintf("%s:approval:%s:%d", p.RunID, n.Key, it.Index)}
		workflow.ExecuteActivity(eventCtx(ctx), a.DeliverEvent, ev) // not awaited: the wait for a person starts now
	}

	var (
		sig      ApprovalSignal
		timedOut bool
	)
	timerCtx, stopTimer := workflow.WithCancel(ctx)
	timer := workflow.NewTimer(timerCtx, approvalTimeout)
	ch := workflow.GetSignalChannel(ctx, SignalName(n.Key))
	for {
		var got ApprovalSignal
		received := false
		sel := workflow.NewSelector(ctx)
		sel.AddReceive(ch, func(c workflow.ReceiveChannel, _ bool) { c.Receive(ctx, &got); received = true })
		sel.AddFuture(timer, func(f workflow.Future) { timedOut = f.Get(ctx, nil) == nil })
		sel.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) {})
		sel.Select(ctx)
		if ctx.Err() != nil {
			stopTimer()
			return nil, false, temporal.NewCanceledError()
		}
		if timedOut {
			break
		}
		// A decision meant for another wait (an earlier loop item, or a repeated
		// click) is dropped rather than applied here.
		if received && got.Token == token {
			sig = got
			break
		}
	}
	stopTimer()

	out := map[string]any{"approved": sig.Approved, "note": sig.Note, "decidedBy": sig.By, "message": message}
	if timedOut {
		out["note"] = "No decision within 7 days."
		out["decidedBy"] = "timeout"
	}
	if err := finishNode(ctx, FinishNodeInput{RunID: p.RunID, Key: n.Key, Status: statusSucceeded, Output: out, HasItem: it.Has, Index: it.Index}); err != nil {
		return nil, false, err
	}
	if err := workflow.ExecuteActivity(ioCtx(ctx), a.RefreshRunStatus, p.RunID).Get(ctx, nil); err != nil {
		return nil, false, err
	}
	return out, sig.Approved, nil
}

// fanOut runs a loop's body once per item, filling out with the results. The
// item list came from the loop node's activity result, so a replay reconstructs
// the same fan-out.
//
// With onError "continue" a failed item is recorded and the loop moves on; the
// default stops at the first failure.
func (r *runner) fanOut(ctx workflow.Context, loop compiler.GraphNode, body []string, up map[string]map[string]any, out map[string]any) error {
	items, _ := out["items"].([]any)
	if len(items) > maxLoopItems {
		return &loopError{fmt.Sprintf("loop has %d items, more than the limit of %d", len(items), maxLoopItems)}
	}
	keepGoing := continueOnError(loop)
	// A body step is handed the list itself only when it asks for it. Otherwise
	// every step of every item would carry the whole list into the run's history.
	lean := map[string]any{"count": out["count"]}
	var (
		results    = make([]any, 0, len(items))
		failures   []any
		failedKeys = map[string]int{}
	)
	for idx, item := range items {
		outputs := map[string]map[string]any{}
		for k, v := range up {
			outputs[k] = v
		}
		pruned := map[string]bool{}
		it := NodeScopeItem{Has: true, Item: item, Loop: map[string]any{loop.Key: map[string]any{"index": float64(idx)}}, Index: idx}
		perItem := map[string]any{}
		for _, key := range body {
			if pruned[key] {
				continue
			}
			n := r.g.byKey[key]
			outputs[loop.Key] = lean
			if readsLoopItems(n, loop.Key) {
				outputs[loop.Key] = out
			}
			nodeOut, open, err := r.step(ctx, n, upstream(n, outputs), it)
			if err != nil {
				if !keepGoing || r.exhausted || temporal.IsCanceledError(err) || ctx.Err() != nil {
					return err
				}
				perItem["error"], perItem["failedAt"] = err.Error(), key
				failures = append(failures, map[string]any{"index": float64(idx), "node": key, "error": err.Error()})
				failedKeys[key]++
				break
			}
			outputs[key], perItem[key] = nodeOut, nodeOut
			if !open {
				for _, d := range r.g.descendantsOf(key) {
					pruned[d] = true
				}
			}
		}
		results = append(results, perItem)
	}
	if tooLarge(results) {
		return &loopError{"loop results are too large to keep (over 1.5 MB)"}
	}
	out["results"] = results
	out["failed"] = float64(len(failures))
	out["succeeded"] = float64(len(items) - len(failures))
	if len(failures) > 0 {
		out["errors"] = failures
		// The body rows hold whatever the last item did. Make them say that some
		// items failed, which is what a reader of the run needs to know.
		for _, key := range body {
			if c := failedKeys[key]; c > 0 {
				msg := fmt.Sprintf("failed for %d of %d items; see the loop's errors", c, len(items))
				if err := finishNode(ctx, FinishNodeInput{RunID: r.p.RunID, Key: key, Status: statusFailed, Error: msg}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func continueOnError(loop compiler.GraphNode) bool {
	v, _ := loop.Config["onError"].(string)
	return strings.EqualFold(strings.TrimSpace(v), "continue")
}
