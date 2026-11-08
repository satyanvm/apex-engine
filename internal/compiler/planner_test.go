package compiler

import (
	"context"
	"strings"
	"testing"
)

// A missing provider must be visible to the caller. Structural validation, layout
// and routing logic remain pure and are tested directly.
//
// These replace two tests that asserted the OPPOSITE: that a keyless CompilePrompt
// still returned a valid runnable graph. It did — a fixed trigger→research→report
// shape — and that was the bug, not the feature.

func TestCompile_MissingKeyNamesTheKey(t *testing.T) {
	// The one credential must name itself, so an operator learns what is missing.
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("PLANNER_MAP_MODEL", "gemini-3.1-flash-lite")
	_, err := Compile(context.Background(), BuiltinCatalog(), "Summarize my orders", "")
	if err == nil {
		t.Fatal("GEMINI_API_KEY unset: expected an error, got a compiled plan")
	}
	if !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Fatalf("GEMINI_API_KEY unset: error must name it, got %v", err)
	}
}

func TestLoadCatalog_HasBuiltinsAndIntegrations(t *testing.T) {
	cat := LoadCatalog()
	for _, id := range []string{"trigger.manual", "agent.llm", "tool.http", "logic.loop", "output.report"} {
		if _, ok := cat.ByID[id]; !ok {
			t.Errorf("builtin %q missing from catalog", id)
		}
	}
}

func TestValidateGraph_CatchesStructuralErrors(t *testing.T) {
	cat := LoadCatalog()
	nodes := []GraphNode{
		{Key: "a", Type: "agent.llm", Config: map[string]any{"prompt": "x"}, DependsOn: []string{"b"}},
		{Key: "b", Type: "agent.llm", Config: map[string]any{"prompt": "x"}, DependsOn: []string{"a"}},
		{Key: "b", Type: "nope.unknown", DependsOn: []string{"ghost"}},
	}
	errs := validateGraph(nodes, cat)
	joined := strings.Join(errs, "\n")
	for _, want := range []string{"duplicate node key: b", "unknown type", "unknown node 'ghost'", "cycle detected"} {
		if !strings.Contains(joined, want) {
			t.Errorf("validation missing %q in:\n%s", want, joined)
		}
	}

	valid := []GraphNode{
		{Key: "t", Type: "trigger.manual", Config: map[string]any{}},
		{Key: "r", Type: "agent.llm", Config: map[string]any{"prompt": "go"}, DependsOn: []string{"t"}},
	}
	if errs := validateGraph(valid, cat); len(errs) != 0 {
		t.Errorf("valid graph rejected: %v", errs)
	}
}

func TestValidateGraph_RequiredConfig(t *testing.T) {
	cat := LoadCatalog()
	nodes := []GraphNode{
		{Key: "a", Type: "agent.llm", Config: map[string]any{}}, // prompt is required
	}
	errs := validateGraph(nodes, cat)
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, " "), "required config 'prompt'") {
		t.Errorf("missing required config not caught: %v", errs)
	}
}

func TestLayoutGraph_DepthColumns(t *testing.T) {
	nodes := layoutGraph([]GraphNode{
		{Key: "t", Type: "trigger.manual"},
		{Key: "a", DependsOn: []string{"t"}},
		{Key: "b", DependsOn: []string{"t"}},
		{Key: "join", DependsOn: []string{"a", "b"}},
	})
	byKey := map[string]GraphNode{}
	for _, n := range nodes {
		byKey[n.Key] = n
	}
	if byKey["t"].Col != 0 || byKey["a"].Col != 1 || byKey["b"].Col != 1 || byKey["join"].Col != 2 {
		t.Errorf("cols wrong: t=%d a=%d b=%d join=%d",
			byKey["t"].Col, byKey["a"].Col, byKey["b"].Col, byKey["join"].Col)
	}
	if byKey["a"].Row == byKey["b"].Row {
		t.Errorf("parallel nodes share a row: %d", byKey["a"].Row)
	}
}

func TestScheduleFrom_TimeParsing(t *testing.T) {
	cases := map[string]string{
		"every day at 9:30pm": "daily 21:30",
		"each morning":        "daily 09:00",
		"hourly":              "hourly",
	}
	for prompt, want := range cases {
		got := scheduleFrom(prompt)
		if got == nil || *got != want {
			t.Errorf("scheduleFrom(%q) = %v, want %q", prompt, got, want)
		}
	}
	if got := scheduleFrom("just once please"); got != nil {
		t.Errorf("scheduleFrom(no cadence) = %v, want nil", *got)
	}
}
