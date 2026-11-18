package compiler

import (
	"fmt"
	"strings"
)

// ReviewStep says in plain words what one step of a graph does.
type ReviewStep struct {
	Node string `json:"node"`
	// Kind is start, read, write, delete, action, model, approval, loop, gate,
	// wait or transform. write, delete and action can change something outside
	// the engine; an action is a tool that does not say whether it does.
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

// Review is what a person needs to agree to a graph without reading it as a
// graph: the steps in order, what changes, and what changes with nobody asked.
type Review struct {
	Steps     []ReviewStep `json:"steps"`
	Reads     int          `json:"reads"`
	Changes   int          `json:"changes"`
	Approvals int          `json:"approvals"`
	// Warnings are not errors. Each names a step that changes something with no
	// approval in front of it.
	Warnings []string `json:"warnings"`
}

// Changes reports whether a step of this kind can change something outside.
func changes(kind string) bool { return kind == "write" || kind == "delete" || kind == "action" }

// ReviewGraph describes a graph. It is deterministic: the same graph and tools
// always give the same words, and no model is involved.
func ReviewGraph(nodes []GraphNode, cat *Catalog) Review {
	byKey := map[string]GraphNode{}
	for _, n := range nodes {
		byKey[n.Key] = n
	}
	inLoop := map[string]bool{}
	for _, n := range nodes {
		if n.Type == "logic.loop" {
			for _, k := range LoopBody(nodes, n.Key) {
				inLoop[k] = true
			}
		}
	}
	// guarded: some approval sits on every path's way in, approximated as "an
	// approval is among the ancestors", which is what the runtime enforces: a
	// refused approval skips everything after it.
	var guarded func(key string, seen map[string]bool) bool
	guarded = func(key string, seen map[string]bool) bool {
		if seen[key] {
			return false
		}
		seen[key] = true
		for _, d := range byKey[key].DependsOn {
			if byKey[d].Type == "logic.approval" || guarded(d, seen) {
				return true
			}
		}
		return false
	}

	rv := Review{Steps: []ReviewStep{}, Warnings: []string{}}
	for _, n := range topoOrder(nodes) {
		spec := cat.ByID[n.Type]
		tool := ""
		if f, ok := cat.Clusters[spec.Cluster]; ok && isToolType(n.Type) && n.Type != "tool.http" {
			tool = f.Label
		}
		name := strings.TrimSpace(n.Name)
		if name == "" {
			name = spec.Label
		}
		st := ReviewStep{Node: n.Key, Tool: tool, PerItem: inLoop[n.Key], AfterApproval: guarded(n.Key, map[string]bool{})}
		cfg := func(k string) string { return strings.TrimSpace(fmt.Sprint(orEmpty(n.Config[k]))) }
		switch {
		case strings.HasPrefix(n.Type, "trigger."):
			st.Kind, st.Text = "start", "Start"
		case n.Type == "logic.approval":
			st.Kind, st.Text = "approval", "Ask a person: "+quote(cfg("message"))
		case n.Type == "logic.loop":
			st.Kind, st.Text = "loop", "Repeat the steps after this for each item of "+strings.Trim(cfg("items"), "{} ")
			if strings.EqualFold(cfg("onError"), "continue") {
				st.Text += ", carrying on if one fails"
			}
		case n.Type == "logic.branch" || n.Type == "logic.filter":
			st.Kind, st.Text = "gate", "Continue only if "+cfg("condition")
		case n.Type == "logic.wait":
			st.Kind = "wait"
			if u := cfg("until"); u != "" {
				st.Text = "Wait until " + u
			} else {
				st.Text = "Wait " + cfg("seconds") + " seconds"
			}
		case n.Type == "data.transform":
			st.Kind, st.Text, st.Detail = "transform", "Work out: "+name, clipText(cfg("expression"), 400)
		case strings.HasPrefix(n.Type, "agent."):
			st.Kind, st.Text, st.Detail = "model", "Ask the model: "+name, clipText(cfg("prompt"), 400)
		case n.Type == "tool.http":
			method := strings.ToUpper(cfg("method"))
			if method == "" {
				method = "GET"
			}
			st.Kind = methodKind(method)
			st.Text = fmt.Sprintf("%s %s", method, cfg("url"))
		case strings.HasPrefix(n.Type, "api."):
			method, _ := spec.Exec["method"].(string)
			st.Kind = methodKind(method)
			st.Text = verb(st.Kind) + " " + tool + ": " + name
			if isTrue(n.Config["allPages"]) {
				st.Text += " (every page)"
			}
		case strings.HasPrefix(n.Type, "mcp."):
			st.Kind = "action"
			if isTrue(spec.Exec["readOnly"]) {
				st.Kind = "read"
			}
			st.Text = verb(st.Kind) + " " + tool + ": " + name
		default: // pieces, and anything a later version adds
			st.Kind = "action"
			st.Text = verb(st.Kind) + " " + tool + ": " + name
		}
		if isToolType(n.Type) || strings.HasPrefix(n.Type, "agent.") {
			if st.PerItem {
				st.Text += ", once for each item"
			}
			if st.AfterApproval && changes(st.Kind) {
				st.Text += ", after a person approves"
			}
		}
		switch {
		case st.Kind == "read":
			rv.Reads++
		case st.Kind == "approval":
			rv.Approvals++
		case changes(st.Kind):
			rv.Changes++
			if !st.AfterApproval {
				what := "changes data"
				if st.Kind == "delete" {
					what = "deletes data"
				} else if st.Kind == "action" {
					what = "may change data"
				}
				where := ""
				if tool != "" {
					where = " in " + tool
				}
				each := ""
				if st.PerItem {
					each = ", once for each item,"
				}
				rv.Warnings = append(rv.Warnings, fmt.Sprintf("'%s' %s%s%s and no person is asked first", name, what, where, each))
			}
		}
		rv.Steps = append(rv.Steps, st)
	}
	return rv
}

func methodKind(method string) string {
	switch strings.ToUpper(method) {
	case "GET", "HEAD", "OPTIONS", "":
		return "read"
	case "DELETE":
		return "delete"
	}
	return "write"
}

func verb(kind string) string {
	switch kind {
	case "read":
		return "Read from"
	case "delete":
		return "Delete in"
	case "write":
		return "Change"
	}
	return "Use"
}

func clipText(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func quote(s string) string {
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return `"` + s + `"`
}
