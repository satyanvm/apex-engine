package runtime

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/satyanvm/apex-engine/internal/compiler"
)

// graph is the pure, deterministic view of a DAG. It uses no maps for ordering,
// so a replay computes the identical schedule.
type graph struct {
	ordered    []compiler.GraphNode
	byKey      map[string]compiler.GraphNode
	dependents map[string][]string
}

func newGraph(nodes []compiler.GraphNode) (*graph, error) {
	g := &graph{byKey: map[string]compiler.GraphNode{}, dependents: map[string][]string{}}
	indeg := map[string]int{}
	for _, n := range nodes {
		if _, dup := g.byKey[n.Key]; dup {
			return nil, fmt.Errorf("duplicate node key: %s", n.Key)
		}
		g.byKey[n.Key] = n
	}
	for _, n := range nodes {
		for _, d := range n.DependsOn {
			if _, ok := g.byKey[d]; !ok {
				return nil, fmt.Errorf("node %s depends on unknown node %s", n.Key, d)
			}
			indeg[n.Key]++
			g.dependents[d] = append(g.dependents[d], n.Key)
		}
	}
	placed := map[string]bool{}
	for len(g.ordered) < len(nodes) {
		progressed := false
		for _, n := range nodes { // original order is the tie-break
			if !placed[n.Key] && indeg[n.Key] == 0 {
				placed[n.Key], progressed = true, true
				g.ordered = append(g.ordered, n)
				for _, c := range g.dependents[n.Key] {
					indeg[c]--
				}
			}
		}
		if !progressed {
			return nil, fmt.Errorf("cycle detected in graph")
		}
	}
	return g, nil
}

func (g *graph) descendantsOf(key string) []string {
	seen := map[string]bool{}
	stack := append([]string{}, g.dependents[key]...)
	for len(stack) > 0 {
		k := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !seen[k] {
			seen[k] = true
			stack = append(stack, g.dependents[k]...)
		}
	}
	var out []string
	for _, n := range g.ordered {
		if seen[n.Key] {
			out = append(out, n.Key)
		}
	}
	return out
}

// readsLoopResults reports whether a node's config uses the loop's collected
// results. Those exist only once the loop has finished, so such a node cannot be
// part of the body.
func readsLoopResults(n compiler.GraphNode, loop string) bool {
	raw, err := json.Marshal(n.Config)
	if err != nil {
		return false
	}
	return strings.Contains(string(raw), "outputs."+loop+".results")
}

// loopBody is the subgraph every path into which passes through the loop. A join
// that also depends on something outside runs once, after the loop, and so does
// a node that reads the loop's collected results, along with anything after it.
func (g *graph) loopBody(key string) []string {
	body := map[string]bool{}
	var out []string
	for _, n := range g.ordered {
		if n.Key == key || len(n.DependsOn) == 0 || readsLoopResults(n, key) {
			continue
		}
		inside := true
		for _, d := range n.DependsOn {
			if d != key && !body[d] {
				inside = false
				break
			}
		}
		if inside {
			body[n.Key] = true
			out = append(out, n.Key)
		}
	}
	return out
}

func (g *graph) depsSettled(n compiler.GraphNode, state map[string]string, fanned map[string]bool) bool {
	for _, d := range n.DependsOn {
		if s := state[d]; s != "done" && s != "skipped" {
			return false
		}
	}
	return true
}

func (g *graph) allDepsSkipped(n compiler.GraphNode, state map[string]string) bool {
	if len(n.DependsOn) == 0 {
		return false
	}
	for _, d := range n.DependsOn {
		if state[d] != "skipped" {
			return false
		}
	}
	return true
}
