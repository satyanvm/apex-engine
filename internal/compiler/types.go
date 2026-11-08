package compiler

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// GraphNode is one node of a DAG.
type GraphNode struct {
	Key       string         `json:"key"`
	Type      string         `json:"type"`
	Name      string         `json:"name"`
	DependsOn []string       `json:"dependsOn"`
	Config    map[string]any `json:"config"`
	Col       int            `json:"col"`
	Row       int            `json:"row"`
}

func itoa(n int) string { return strconv.Itoa(n) }

// StageEvent reports compiler progress. Status is start, done or failed.
type StageEvent struct {
	Stage  string `json:"stage"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type progressKey struct{}

// WithProgress makes every compile stage on ctx report to fn. fn may be called
// from several goroutines.
func WithProgress(ctx context.Context, fn func(StageEvent)) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

func report(ctx context.Context, stage, status, detail string) {
	if fn, ok := ctx.Value(progressKey{}).(func(StageEvent)); ok {
		fn(StageEvent{Stage: stage, Status: status, Detail: detail})
	}
}

// Models reports which models the compiler is configured to use.
func Models() (mapName, reduce string) { return mapModel(), reduceModel() }

func mergeInput(defaults, perRun map[string]any) map[string]any {
	out := make(map[string]any, len(defaults)+len(perRun))
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range perRun {
		out[k] = v
	}
	return out
}

func ollamaHost() string {
	return strings.TrimSuffix(envOr("OLLAMA_HOST", "http://localhost:11434"), "/")
}

// Complete runs one model call for agent.llm nodes.
func Complete(ctx context.Context, model, system, user string, timeout time.Duration) (string, error) {
	return planLLMWith(ctx, model, 8192, timeout, system, []llmMsg{{Role: "user", Content: user}})
}

// AgentModel is the model agent.llm nodes run on.
func AgentModel() string { return envOr("APEX_AGENT_MODEL", mapModel()) }
