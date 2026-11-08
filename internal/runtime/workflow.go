package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/satyanvm/apex-engine/internal/compiler"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// TaskQueue is the queue the engine's workers poll.
const TaskQueue = "apex-engine"

const (
	maxParallel      = 8
	maxLoopItems     = 500
	approvalTimeout  = 7 * 24 * time.Hour
	nodeTimeout      = 10 * time.Minute
	heartbeatTimeout = 15 * time.Second
)

// RunParams starts a run. The DAG travels in the workflow input, so a replay
// never depends on database state.
type RunParams struct {
	RunID      string
	Tenant     string
	Nodes      []compiler.GraphNode
	Input      map[string]any
	CatalogIDs []string
}

// ApprovalSignal is sent to a run waiting on a logic.approval node.
type ApprovalSignal struct {
	Approved bool
	Note     string
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
	Loop       map[string]any
	CatalogIDs []string
}

// NodeResult is what a node produced.
type NodeResult struct {
	Output map[string]any
	Passed bool // gates only
}

// FinishNodeInput records a node state the workflow decided.
type FinishNodeInput struct {
	RunID, Key, Status, Error string
	Output                    map[string]any
}

// FinishRunInput closes a run.
type FinishRunInput struct{ RunID, Status, Error string }

// ScheduledInput creates the row for a run started by a schedule.
type ScheduledInput struct {
	RunID, Tenant, WorkflowID string
	Nodes                     []compiler.GraphNode
	Input                     map[string]any
	CatalogIDs                []string
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
	in := ScheduledInput{RunID: id, Tenant: p.Tenant, WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID, Nodes: p.Nodes, Input: p.Input, CatalogIDs: p.CatalogIDs}
	if err := workflow.ExecuteActivity(ioCtx(ctx), a.CreateScheduledRun, in).Get(ctx, nil); err != nil {
		return err
	}
	return RunWorkflow(ctx, p)
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

func drive(ctx workflow.Context, p RunParams) error {
	if err := workflow.ExecuteActivity(ioCtx(ctx), a.MarkRunStarted, p.RunID).Get(ctx, nil); err != nil {
		return err
	}
	g, err := newGraph(p.Nodes)
	if err != nil {
		return err
	}
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
					if err := finishNode(ctx, p.RunID, n.Key, "skipped", map[string]any{"skipped": true}, ""); err != nil {
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
					out, open, err := runNode(gctx, p, g, node, up)
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

func finishNode(ctx workflow.Context, runID, key, status string, out map[string]any, msg string) error {
	return workflow.ExecuteActivity(ioCtx(ctx), a.FinishNode, FinishNodeInput{RunID: runID, Key: key, Status: status, Output: out, Error: msg}).Get(ctx, nil)
}

// runNode runs one scheduled node, including a loop's whole fan-out, and reports
// whether its descendants may run.
func runNode(ctx workflow.Context, p RunParams, g *graph, n compiler.GraphNode, up map[string]map[string]any) (map[string]any, bool, error) {
	if n.Type == "logic.approval" {
		return runApproval(ctx, p, n, up, NodeScopeItem{})
	}
	res, err := execute(ctx, p, n, up, NodeScopeItem{})
	if err != nil {
		return nil, false, err
	}
	if n.Type == "logic.loop" {
		results, err := fanOut(ctx, p, g, n, up, res.Output)
		if err != nil {
			if !temporal.IsCanceledError(err) {
				_ = failNode(ctx, p, n.Key, "a step inside this loop failed")
			}
			return nil, false, err
		}
		res.Output["results"] = results
		if err := finishNode(ctx, p.RunID, n.Key, statusSucceeded, res.Output, ""); err != nil {
			return nil, false, err
		}
	}
	return res.Output, !isGate(n.Type) || res.Passed, nil
}

// NodeScopeItem carries the loop context of a body node.
type NodeScopeItem struct {
	Has  bool
	Item any
	Loop map[string]any
}

func execute(ctx workflow.Context, p RunParams, n compiler.GraphNode, up map[string]map[string]any, it NodeScopeItem) (NodeResult, error) {
	var res NodeResult
	in := NodeInput{RunID: p.RunID, Tenant: p.Tenant, Node: n, Input: p.Input, Upstream: up, HasItem: it.Has, Item: it.Item, Loop: it.Loop, CatalogIDs: p.CatalogIDs}
	if tooLarge(in) {
		return res, failNode(ctx, p, n.Key, payloadMessage)
	}
	err := workflow.ExecuteActivity(nodeCtx(ctx), a.ExecuteNode, in).Get(ctx, &res)
	if err == nil {
		return res, nil
	}
	if temporal.IsCanceledError(err) {
		return res, err
	}
	return res, failNode(ctx, p, n.Key, rootMessage(err))
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

func failNode(ctx workflow.Context, p RunParams, key, msg string) error {
	dctx, _ := workflow.NewDisconnectedContext(ctx)
	_ = finishNode(dctx, p.RunID, key, statusFailed, nil, msg)
	return fmt.Errorf("node %s: %s", key, msg)
}

const payloadMessage = "upstream data is too large to pass to this node (over 1.5 MB)"

func runApproval(ctx workflow.Context, p RunParams, n compiler.GraphNode, up map[string]map[string]any, it NodeScopeItem) (map[string]any, bool, error) {
	var res NodeResult
	in := NodeInput{RunID: p.RunID, Tenant: p.Tenant, Node: n, Input: p.Input, Upstream: up, HasItem: it.Has, Item: it.Item, Loop: it.Loop, CatalogIDs: p.CatalogIDs}
	if tooLarge(in) {
		return nil, false, failNode(ctx, p, n.Key, payloadMessage)
	}
	if err := workflow.ExecuteActivity(nodeCtx(ctx), a.ExecuteNode, in).Get(ctx, &res); err != nil {
		if temporal.IsCanceledError(err) {
			return nil, false, err
		}
		return nil, false, failNode(ctx, p, n.Key, rootMessage(err))
	}
	if err := workflow.ExecuteActivity(ioCtx(ctx), a.RefreshRunStatus, p.RunID).Get(ctx, nil); err != nil {
		return nil, false, err
	}

	var (
		sig      ApprovalSignal
		timedOut bool
	)
	timerCtx, stopTimer := workflow.WithCancel(ctx)
	sel := workflow.NewSelector(ctx)
	sel.AddReceive(workflow.GetSignalChannel(ctx, SignalName(n.Key)), func(c workflow.ReceiveChannel, _ bool) { c.Receive(ctx, &sig) })
	sel.AddFuture(workflow.NewTimer(timerCtx, approvalTimeout), func(f workflow.Future) { timedOut = f.Get(ctx, nil) == nil })
	sel.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) {})
	sel.Select(ctx)
	stopTimer()
	if ctx.Err() != nil {
		return nil, false, temporal.NewCanceledError()
	}

	out := map[string]any{"approved": sig.Approved, "note": sig.Note}
	if timedOut {
		out["note"] = "No decision within 7 days."
	}
	if err := finishNode(ctx, p.RunID, n.Key, statusSucceeded, out, ""); err != nil {
		return nil, false, err
	}
	if err := workflow.ExecuteActivity(ioCtx(ctx), a.RefreshRunStatus, p.RunID).Get(ctx, nil); err != nil {
		return nil, false, err
	}
	return out, sig.Approved, nil
}

// fanOut runs a loop's body once per item. The item list came from the loop
// node's activity result, so a replay reconstructs the same fan-out.
func fanOut(ctx workflow.Context, p RunParams, g *graph, loop compiler.GraphNode, up map[string]map[string]any, out map[string]any) ([]any, error) {
	items, _ := out["items"].([]any)
	if len(items) > maxLoopItems {
		return nil, failNode(ctx, p, loop.Key, fmt.Sprintf("loop has %d items, more than the limit of %d", len(items), maxLoopItems))
	}
	body := g.loopBody(loop.Key)
	results := make([]any, 0, len(items))
	for idx, item := range items {
		outputs := map[string]map[string]any{loop.Key: out}
		for k, v := range up {
			outputs[k] = v
		}
		pruned := map[string]bool{}
		it := NodeScopeItem{Has: true, Item: item, Loop: map[string]any{loop.Key: map[string]any{"index": float64(idx)}}}
		perItem := map[string]any{}
		for _, key := range body {
			n := g.byKey[key]
			if pruned[key] {
				continue
			}
			var (
				nodeOut map[string]any
				open    bool
				err     error
			)
			nu := upstream(n, outputs)
			if n.Type == "logic.approval" {
				nodeOut, open, err = runApproval(ctx, p, n, nu, it)
			} else {
				var res NodeResult
				res, err = execute(ctx, p, n, nu, it)
				nodeOut, open = res.Output, !isGate(n.Type) || res.Passed
			}
			if err != nil {
				return nil, err
			}
			outputs[key], perItem[key] = nodeOut, nodeOut
			if !open {
				for _, d := range g.descendantsOf(key) {
					pruned[d] = true
				}
			}
		}
		results = append(results, perItem)
	}
	if tooLarge(results) {
		return nil, failNode(ctx, p, loop.Key, "loop results are too large to keep (over 1.5 MB)")
	}
	return results, nil
}
