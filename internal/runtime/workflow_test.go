package runtime

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/satyanvm/apex-engine/internal/compiler"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

var (
	mockCtx = mock.Anything
	mockAny = mock.Anything
)

type recorder struct {
	mu       sync.Mutex
	ran      []string
	inputs   map[string]NodeInput
	finished map[string]string
	run      FinishRunInput
}

func n(key, typ string, deps ...string) compiler.GraphNode {
	return compiler.GraphNode{Key: key, Type: typ, Name: key, DependsOn: deps, Config: map[string]any{}}
}

// exec runs a DAG in the test environment with stubbed activities. handle
// supplies a node's result; nil means an empty success.
func exec(t *testing.T, nodes []compiler.GraphNode, handle func(NodeInput) (NodeResult, error), setup func(*testsuite.TestWorkflowEnvironment)) (*recorder, *testsuite.TestWorkflowEnvironment) {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.SetTestTimeout(30 * time.Second)
	rec := &recorder{inputs: map[string]NodeInput{}, finished: map[string]string{}}
	env.RegisterActivity(&Activities{})
	env.OnActivity("MarkRunStarted", mockCtx, "r").Maybe().Return(nil)
	env.OnActivity("RefreshRunStatus", mockCtx, "r").Maybe().Return(nil)
	env.OnActivity("FinishNode", mockCtx, mockAny).Maybe().Return(func(_ context.Context, in FinishNodeInput) error {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.finished[in.Key] = in.Status
		return nil
	})
	env.OnActivity("FinishRun", mockCtx, mockAny).Return(func(_ context.Context, in FinishRunInput) error {
		rec.run = in
		return nil
	})
	env.OnActivity("ExecuteNode", mockCtx, mockAny).Maybe().Return(func(_ context.Context, in NodeInput) (NodeResult, error) {
		rec.mu.Lock()
		rec.ran = append(rec.ran, in.Node.Key)
		rec.inputs[in.Node.Key] = in
		rec.mu.Unlock()
		if handle != nil {
			return handle(in)
		}
		return NodeResult{Output: map[string]any{"text": in.Node.Key}, Passed: true}, nil
	})
	if setup != nil {
		setup(env)
	}
	env.ExecuteWorkflow(RunWorkflow, RunParams{RunID: "r", Tenant: "t", Nodes: nodes, Input: map[string]any{}})
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	return rec, env
}

func sorted(s []string) []string {
	out := append([]string{}, s...)
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDiamondRunsEachNodeOnceWithDirectUpstream(t *testing.T) {
	rec, env := exec(t, []compiler.GraphNode{n("t", "trigger.manual"), n("a", "agent.llm", "t"), n("b", "agent.llm", "t"), n("join", "agent.llm", "a", "b")}, nil, nil)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if rec.run.Status != statusSucceeded {
		t.Fatalf("run = %+v", rec.run)
	}
	if got := sorted(rec.ran); !equal(got, []string{"a", "b", "join", "t"}) {
		t.Errorf("ran = %v", got)
	}
	if rec.ran[len(rec.ran)-1] != "join" {
		t.Errorf("join ran before its dependencies: %v", rec.ran)
	}
	up := rec.inputs["join"].Upstream
	if len(up) != 2 || up["a"]["text"] != "a" || up["b"]["text"] != "b" {
		t.Errorf("join upstream = %v", up)
	}
}

func TestClosedGatePrunesDescendants(t *testing.T) {
	rec, _ := exec(t, []compiler.GraphNode{n("t", "trigger.manual"), n("gate", "logic.branch", "t"), n("x", "agent.llm", "gate"), n("y", "agent.llm", "x"), n("other", "agent.llm", "t")},
		func(in NodeInput) (NodeResult, error) {
			return NodeResult{Output: map[string]any{"passed": in.Node.Key != "gate"}, Passed: in.Node.Key != "gate"}, nil
		}, nil)
	if rec.run.Status != statusSucceeded {
		t.Fatalf("run = %+v", rec.run)
	}
	if got := sorted(rec.ran); !equal(got, []string{"gate", "other", "t"}) {
		t.Errorf("ran = %v", got)
	}
	if rec.finished["x"] != "skipped" || rec.finished["y"] != "skipped" {
		t.Errorf("finished = %v", rec.finished)
	}
}

func TestNodeFailureFailsRunAndStopsDownstream(t *testing.T) {
	rec, env := exec(t, []compiler.GraphNode{n("t", "trigger.manual"), n("a", "agent.llm", "t"), n("b", "agent.llm", "a")},
		func(in NodeInput) (NodeResult, error) {
			if in.Node.Key == "a" {
				return NodeResult{}, temporal.NewNonRetryableApplicationError("HTTP 404 from Acme", "NodeError", nil)
			}
			return NodeResult{Output: map[string]any{}}, nil
		}, nil)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("a failed run should finish the workflow cleanly: %v", err)
	}
	if rec.run.Status != statusFailed || rec.run.Error != "node a: HTTP 404 from Acme" {
		t.Fatalf("run = %+v", rec.run)
	}
	if rec.finished["a"] != statusFailed {
		t.Errorf("finished = %v", rec.finished)
	}
	for _, k := range rec.ran {
		if k == "b" {
			t.Error("b ran after its dependency failed")
		}
	}
}

func TestRetryableFailureIsRetried(t *testing.T) {
	calls := 0
	rec, _ := exec(t, []compiler.GraphNode{n("a", "agent.llm")}, func(NodeInput) (NodeResult, error) {
		calls++
		if calls < 3 {
			return NodeResult{}, errors.New("HTTP 503")
		}
		return NodeResult{Output: map[string]any{"ok": true}}, nil
	}, nil)
	if calls != 3 || rec.run.Status != statusSucceeded {
		t.Fatalf("calls=%d run=%+v", calls, rec.run)
	}
}

func TestLoopFansOutBodyPerItem(t *testing.T) {
	var (
		mu    sync.Mutex
		items []any
		idx   []any
	)
	rec, _ := exec(t, []compiler.GraphNode{n("t", "trigger.manual"), n("each", "logic.loop", "t"), n("do", "agent.llm", "each"), n("after", "agent.llm", "do", "t")},
		func(in NodeInput) (NodeResult, error) {
			switch in.Node.Key {
			case "each":
				return NodeResult{Output: map[string]any{"items": []any{"x", "y", "z"}, "count": float64(3)}}, nil
			case "do":
				mu.Lock()
				items = append(items, in.Item)
				idx = append(idx, in.Loop["each"].(map[string]any)["index"])
				mu.Unlock()
				return NodeResult{Output: map[string]any{"text": "done-" + in.Item.(string)}}, nil
			}
			return NodeResult{Output: map[string]any{}}, nil
		}, nil)
	if rec.run.Status != statusSucceeded {
		t.Fatalf("run = %+v", rec.run)
	}
	if len(items) != 3 || items[2] != "z" || idx[2] != float64(2) {
		t.Errorf("items=%v idx=%v", items, idx)
	}
	if rec.finished["each"] != statusSucceeded {
		t.Errorf("loop not finished: %v", rec.finished)
	}
	count := 0
	for _, k := range rec.ran {
		if k == "after" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("join after the loop ran %d times", count)
	}
}

func TestNodeReadingLoopResultsRunsOnceAfterTheLoop(t *testing.T) {
	summary := compiler.GraphNode{Key: "summary", Type: "agent.llm", Name: "summary", DependsOn: []string{"each"},
		Config: map[string]any{"prompt": "Done: {{outputs.each.results}}"}}
	next := n("notify", "agent.llm", "summary")
	var got map[string]any
	rec, _ := exec(t, []compiler.GraphNode{n("each", "logic.loop"), n("do", "agent.llm", "each"), summary, next},
		func(in NodeInput) (NodeResult, error) {
			switch in.Node.Key {
			case "each":
				return NodeResult{Output: map[string]any{"items": []any{"x", "y"}, "count": float64(2)}}, nil
			case "summary":
				got = in.Upstream["each"]
			}
			return NodeResult{Output: map[string]any{"text": in.Node.Key}}, nil
		}, nil)
	if rec.run.Status != statusSucceeded {
		t.Fatalf("run = %+v", rec.run)
	}
	counts := map[string]int{}
	for _, k := range rec.ran {
		counts[k]++
	}
	if counts["do"] != 2 || counts["summary"] != 1 || counts["notify"] != 1 {
		t.Errorf("counts = %v", counts)
	}
	if results, _ := got["results"].([]any); len(results) != 2 {
		t.Errorf("summary saw %v", got)
	}
}

func TestFailureInsideLoopFailsTheLoopNode(t *testing.T) {
	rec, _ := exec(t, []compiler.GraphNode{n("each", "logic.loop"), n("do", "agent.llm", "each")}, func(in NodeInput) (NodeResult, error) {
		if in.Node.Key == "each" {
			return NodeResult{Output: map[string]any{"items": []any{"x"}}}, nil
		}
		return NodeResult{}, temporal.NewNonRetryableApplicationError("boom", "NodeError", nil)
	}, nil)
	if rec.run.Status != statusFailed || rec.finished["each"] != statusFailed || rec.finished["do"] != statusFailed {
		t.Fatalf("run=%+v finished=%v", rec.run, rec.finished)
	}
}

func TestLoopOverLimitFails(t *testing.T) {
	big := make([]any, maxLoopItems+1)
	for i := range big {
		big[i] = i
	}
	rec, _ := exec(t, []compiler.GraphNode{n("each", "logic.loop"), n("do", "agent.llm", "each")}, func(in NodeInput) (NodeResult, error) {
		return NodeResult{Output: map[string]any{"items": big}}, nil
	}, nil)
	if rec.run.Status != statusFailed {
		t.Fatalf("run = %+v", rec.run)
	}
}

func TestOversizedUpstreamFailsTheNode(t *testing.T) {
	huge := make([]byte, maxPayloadBytes+10)
	for i := range huge {
		huge[i] = 'a'
	}
	rec, _ := exec(t, []compiler.GraphNode{n("a", "agent.llm"), n("b", "agent.llm", "a")}, func(in NodeInput) (NodeResult, error) {
		return NodeResult{Output: map[string]any{"text": string(huge)}}, nil
	}, nil)
	if rec.run.Status != statusFailed || rec.finished["b"] != statusFailed {
		t.Fatalf("run=%+v finished=%v", rec.run, rec.finished)
	}
}

func approvalDag() []compiler.GraphNode {
	return []compiler.GraphNode{n("t", "trigger.manual"), n("ok", "logic.approval", "t"), n("send", "agent.llm", "ok")}
}

func TestApprovalApprovedContinues(t *testing.T) {
	rec, _ := exec(t, approvalDag(), nil, func(env *testsuite.TestWorkflowEnvironment) {
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalName("ok"), ApprovalSignal{Approved: true, Note: "lgtm"})
		}, time.Hour)
	})
	if rec.run.Status != statusSucceeded || rec.finished["ok"] != statusSucceeded {
		t.Fatalf("run=%+v finished=%v", rec.run, rec.finished)
	}
	if got := sorted(rec.ran); !equal(got, []string{"ok", "send", "t"}) {
		t.Errorf("ran = %v", got)
	}
}

func TestApprovalRejectedSkipsDownstream(t *testing.T) {
	rec, _ := exec(t, approvalDag(), nil, func(env *testsuite.TestWorkflowEnvironment) {
		env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalName("ok"), ApprovalSignal{Approved: false}) }, time.Hour)
	})
	if rec.run.Status != statusSucceeded || rec.finished["send"] != "skipped" {
		t.Fatalf("run=%+v finished=%v", rec.run, rec.finished)
	}
}

func TestApprovalTimesOutAsRejection(t *testing.T) {
	rec, _ := exec(t, approvalDag(), nil, nil)
	if rec.finished["send"] != "skipped" {
		t.Fatalf("finished=%v", rec.finished)
	}
}

func TestCancelClosesRunAsCancelled(t *testing.T) {
	rec, env := exec(t, approvalDag(), nil, func(env *testsuite.TestWorkflowEnvironment) {
		env.RegisterDelayedCallback(env.CancelWorkflow, time.Hour)
	})
	if rec.run.Status != statusCancelled {
		t.Fatalf("run=%+v", rec.run)
	}
	if err := env.GetWorkflowError(); err == nil || !temporal.IsCanceledError(errors.Unwrap(err)) && !temporal.IsCanceledError(err) {
		t.Errorf("workflow error = %v", err)
	}
	for _, k := range rec.ran {
		if k == "send" {
			t.Error("downstream ran after cancel")
		}
	}
}

func TestCycleFailsTheRun(t *testing.T) {
	rec, _ := exec(t, []compiler.GraphNode{n("a", "agent.llm", "b"), n("b", "agent.llm", "a")}, nil, nil)
	if rec.run.Status != statusFailed {
		t.Fatalf("run=%+v", rec.run)
	}
}
