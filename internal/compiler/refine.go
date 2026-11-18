package compiler

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type baseKey struct{}

// WithBase makes a compile a change to an existing graph instead of a new one:
// the prompt says what to change, and everything it does not mention is kept.
func WithBase(ctx context.Context, base Plan) context.Context {
	return context.WithValue(ctx, baseKey{}, base)
}

func baseOf(ctx context.Context) (Plan, bool) {
	b, ok := ctx.Value(baseKey{}).(Plan)
	return b, ok && len(b.Nodes) > 0
}

const refineRules = `
You are CHANGING an existing workflow graph, not writing a new one.
- Return the FULL graph after the change, in the usual JSON format.
- Copy every node the change does not concern exactly as it is: the same key,
  type, name, dependsOn and config. Do not rename keys. Do not tidy or reorder.
- Change, add or remove only what the request asks for. If a node is removed,
  fix the dependsOn and the references of the nodes that used it.
- Keep defaultInput, and add to it only if the change needs a new input.`

// refineRequest is the user message for a change.
func refineRequest(base Plan, change string) string {
	type node struct {
		Key       string         `json:"key"`
		Type      string         `json:"type"`
		Name      string         `json:"name"`
		DependsOn []string       `json:"dependsOn"`
		Config    map[string]any `json:"config"`
	}
	cur := struct {
		Name         string         `json:"name"`
		Description  string         `json:"description"`
		DefaultInput map[string]any `json:"defaultInput"`
		Nodes        []node         `json:"nodes"`
	}{Name: base.Name, Description: base.Description, DefaultInput: base.DefaultInput}
	for _, n := range base.Nodes {
		deps := n.DependsOn
		if deps == nil {
			deps = []string{}
		}
		cur.Nodes = append(cur.Nodes, node{n.Key, n.Type, n.Name, deps, n.Config})
	}
	raw, _ := json.MarshalIndent(cur, "", " ")
	return "The current workflow graph:\n" + string(raw) + "\n\nThe change to make:\n" + strings.TrimSpace(change) + "\n" + refineRules
}

// refineGraph has the model change the base graph, then validates and repairs
// the result like any other compile.
func refineGraph(ctx context.Context, cat *Catalog, base Plan, change string, p *Plan) ([]GraphNode, error) {
	// The tools the graph already uses stay available, whatever the change
	// request happens to mention.
	selected, err := mapPhase(ctx, cat, change+"\n\n(This changes a workflow described as: "+base.Description+")")
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, id := range selected {
		have[id] = true
	}
	for _, n := range base.Nodes {
		if spec, ok := cat.ByID[n.Type]; ok && spec.Cluster != "builtin" && !have[n.Type] {
			have[n.Type] = true
			selected = append(selected, n.Type)
		}
	}
	sort.Strings(selected)
	for k, v := range base.DefaultInput {
		if _, set := p.DefaultInput[k]; !set {
			p.DefaultInput[k] = v
		}
	}
	report(ctx, "reduce", "start", fmt.Sprintf("changing a graph of %d steps", len(base.Nodes)))
	return authorGraphSingle(ctx, cat, reduceSystemPrompt(cat, selected), refineRequest(base, change), p)
}

// GraphDiff says what a change did to a graph, by node key.
type GraphDiff struct {
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
	Changed   []string `json:"changed"`
	Unchanged int      `json:"unchanged"`
}

// DiffGraphs compares two graphs node by node. Position on a canvas is not a
// change.
func DiffGraphs(before, after []GraphNode) GraphDiff {
	d := GraphDiff{Added: []string{}, Removed: []string{}, Changed: []string{}}
	shape := func(n GraphNode) string {
		deps := append([]string{}, n.DependsOn...)
		sort.Strings(deps)
		raw, _ := json.Marshal([]any{n.Type, n.Name, deps, n.Config})
		return string(raw)
	}
	old := map[string]string{}
	for _, n := range before {
		old[n.Key] = shape(n)
	}
	seen := map[string]bool{}
	for _, n := range after {
		seen[n.Key] = true
		was, existed := old[n.Key]
		switch {
		case !existed:
			d.Added = append(d.Added, n.Key)
		case was != shape(n):
			d.Changed = append(d.Changed, n.Key)
		default:
			d.Unchanged++
		}
	}
	for _, n := range before {
		if !seen[n.Key] {
			d.Removed = append(d.Removed, n.Key)
		}
	}
	return d
}
