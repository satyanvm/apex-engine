package compiler

// nodecatalog.go loads the shared integration-node catalog (packages/nodes/catalog)
// and provides the two things the map-reduce prompt compiler needs from it:
//
//   - per-cluster COMPACT INDEXES ("id — label — description" lines) for the map
//     phase, where a small fast model routes the user's request to relevant nodes;
//   - FULL SCHEMAS (config fields, output fields, credential slots) for the reduce
//     phase, where the big model authors the graph against only the selected nodes.
//
// It also owns structural graph validation (unknown types, missing deps, duplicate
// keys, cycles) — the Go mirror of the reasoner's rules in reasoner/plan.py — and
// the layout pass that assigns (col,row) the same way the reasoner's _grid does.
//
// The catalog is DATA: adding an integration means adding a JSON entry, never code.
// If the catalog directory is missing (stripped deployment), we degrade to the
// built-in fifteen so prompt compilation still works.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/itchyny/gojq"
)

// CatalogField mirrors one config entry of a node definition (also the shape the
// web inspector renders).
type CatalogField struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Control     string   `json:"control"`
	Required    bool     `json:"required"`
	Placeholder string   `json:"placeholder,omitempty"`
	Help        string   `json:"help,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// CatalogCredential names an env var a node resolves at execution time.
type CatalogCredential struct {
	Key  string `json:"key"`
	Help string `json:"help,omitempty"`
}

// CatalogNode is one node definition. The compiler cares about identity, prompt
// text and config/outputs; the execution template (http/browser/code blocks) is
// the reasoner's business and stays opaque here.
type CatalogNode struct {
	ID          string              `json:"id"`
	Label       string              `json:"label"`
	Description string              `json:"description"`
	Kind        string              `json:"kind"`    // trigger|action|logic|output
	Runtime     string              `json:"runtime"` // builtin|http|browser|code|llm
	Search      string              `json:"search"`
	Config      []CatalogField      `json:"config"`
	Outputs     []string            `json:"outputs"`
	Credentials []CatalogCredential `json:"credentials"`
	Cluster     string              `json:"-"` // filled at load from the file
	// Exec is how the runtime performs an imported node (HTTP operation or MCP
	// tool). The compiler never reads it.
	Exec map[string]any `json:"exec,omitempty"`
}

type catalogFile struct {
	Cluster     string `json:"cluster"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// Categories is piece-cluster metadata (Activepieces framework categories,
	// e.g. "PRODUCTIVITY") used by the router directory; hand-written cluster
	// files don't set it.
	Categories []string      `json:"categories,omitempty"`
	Nodes      []CatalogNode `json:"nodes"`
}

// Catalog is the loaded universe of node types.
type Catalog struct {
	Clusters map[string]catalogFile // cluster key → file (nodes carry Cluster)
	ByID     map[string]CatalogNode
	// RequireApproval is the operator's rule for steps that change something:
	// "deletes" refuses a graph that deletes with no approval in front of it,
	// "changes" refuses one that changes anything that way. Empty allows both.
	RequireApproval string
}

// Approval policies.
const (
	ApproveDeletes = "deletes"
	ApproveChanges = "changes"
)

//go:embed builtin.json
var builtinJSON []byte

// BuiltinCatalog returns a fresh catalog holding only the built-in core nodes.
// Callers merge tenant catalogs into it per compile, so nothing is shared
// between tenants.
func BuiltinCatalog() *Catalog {
	var f catalogFile
	if err := json.Unmarshal(builtinJSON, &f); err != nil {
		panic("builtin.json: " + err.Error()) // embedded at build time
	}
	cat := &Catalog{Clusters: map[string]catalogFile{}, ByID: map[string]CatalogNode{}}
	cat.AddCluster(f.Cluster, f.Label, f.Description, f.Nodes)
	return cat
}

// AddCluster adds one group of nodes the planner can route to.
func (c *Catalog) AddCluster(key, label, description string, nodes []CatalogNode) {
	f := catalogFile{Cluster: key, Label: label, Description: description, Nodes: make([]CatalogNode, 0, len(nodes))}
	for _, n := range nodes {
		n.Cluster = key
		if n.ID == "" {
			continue
		}
		c.ByID[n.ID] = n
		f.Nodes = append(f.Nodes, n)
	}
	c.Clusters[key] = f
}

/* ------------------------------ prompt material ------------------------------ */

// clusterIndex renders the compact map-phase index for one cluster.
func clusterIndex(f catalogFile) string {
	var b strings.Builder
	for _, n := range f.Nodes {
		fmt.Fprintf(&b, "%s — %s — %s\n", n.ID, n.Label, n.Description)
	}
	return b.String()
}

// nodeSchema renders one node's full schema for the reduce prompt: everything the
// model needs to fill the node correctly and reference its outputs downstream.
func nodeSchema(n CatalogNode) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s (%s, runtime=%s)\n%s\n", n.ID, n.Label, n.Runtime, n.Description)
	if len(n.Config) > 0 {
		b.WriteString("config:\n")
		for _, f := range n.Config {
			req := ""
			if f.Required {
				req = " (required)"
			}
			opts := ""
			if len(f.Options) > 0 {
				opts = " options=[" + strings.Join(f.Options, ", ") + "]"
			}
			help := f.Help
			if help == "" {
				help = f.Placeholder
			}
			fmt.Fprintf(&b, "  - %s%s%s: %s\n", f.Key, req, opts, help)
		}
	}
	if len(n.Outputs) > 0 {
		fmt.Fprintf(&b, "outputs: %s\n", strings.Join(n.Outputs, ", "))
	}
	if len(n.Credentials) > 0 {
		keys := make([]string, len(n.Credentials))
		for i, c := range n.Credentials {
			keys[i] = c.Key
		}
		fmt.Fprintf(&b, "credentials (required at run time; missing values fail the node): %s\n", strings.Join(keys, ", "))
	}
	return b.String()
}

// sortedClusters returns cluster keys with "builtin" first (always in context),
// then alphabetical — stable prompt ordering keeps compilation reproducible.
func (c *Catalog) sortedClusters() []string {
	keys := make([]string, 0, len(c.Clusters))
	for k := range c.Clusters {
		if k != "builtin" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return append([]string{"builtin"}, keys...)
}

/* ------------------------------ graph validation ------------------------------ */

var validNodeKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validateGraph applies the structural rules the reasoner enforces at load time
// (reasoner/plan.py topo_order) plus catalog-type checks, returning human-readable
// error strings the repair loop can feed back to the model. Empty slice == valid.
func validateGraph(nodes []GraphNode, cat *Catalog) []string {
	var errs []string
	if len(nodes) == 0 {
		return []string{"graph has no nodes"}
	}
	byKey := map[string]bool{}
	for _, n := range nodes {
		if strings.TrimSpace(n.Key) == "" {
			errs = append(errs, "a node is missing its key")
			continue
		}
		if byKey[n.Key] {
			errs = append(errs, "duplicate node key: "+n.Key)
		}
		if !validNodeKey.MatchString(n.Key) {
			// References are dotted paths (outputs.<key>.<field>), so a key like
			// "hackernews.fetch" can never be referenced.
			errs = append(errs, "node key '"+n.Key+"' must use only letters, digits, _ or - (no dots or spaces), e.g. 'fetch_stories'")
		}
		byKey[n.Key] = true
		if n.Type == "" {
			errs = append(errs, "node '"+n.Key+"' has no type")
			continue
		}
		spec, ok := cat.ByID[n.Type]
		if !ok {
			errs = append(errs, "node '"+n.Key+"' uses unknown type '"+n.Type+"'")
			continue
		}
		for _, f := range spec.Config {
			if !f.Required {
				continue
			}
			v, present := n.Config[f.Key]
			if !present || v == nil || fmt.Sprintf("%v", v) == "" {
				errs = append(errs, "node '"+n.Key+"' ("+n.Type+") is missing required config '"+f.Key+"'")
			}
		}
	}
	for _, n := range nodes {
		for _, dep := range n.DependsOn {
			if !byKey[dep] {
				errs = append(errs, "node '"+n.Key+"' depends on unknown node '"+dep+"'")
			}
		}
	}
	if cycle := findCycle(nodes); cycle != "" {
		errs = append(errs, "cycle detected among nodes: "+cycle)
		return errs
	}
	if len(errs) == 0 {
		errs = append(errs, validateSemantics(nodes)...)
	}
	return errs
}

// ConditionChecker parses a gate condition without evaluating it. The runtime
// owns the condition grammar and registers its parser here, so the compiler
// rejects a condition the runtime could not read.
var ConditionChecker func(string) error

// validateSemantics checks the rules that are about what a node means rather
// than how the graph is wired: loops, gate conditions, transforms and waits.
func validateSemantics(nodes []GraphNode) []string {
	var errs []string
	byKey := map[string]GraphNode{}
	for _, n := range nodes {
		byKey[n.Key] = n
	}
	for _, n := range nodes {
		switch n.Type {
		case "logic.loop":
			for _, k := range LoopBody(nodes, n.Key) {
				if byKey[k].Type == "logic.loop" {
					errs = append(errs, "loop '"+k+"' is inside loop '"+n.Key+"': nested loops are not supported. Flatten the two lists into one with a data.transform step, then loop once")
				}
			}
			if v, ok := n.Config["onError"]; ok && v != nil {
				s := strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
				if s != "" && s != "stop" && s != "continue" {
					errs = append(errs, "node '"+n.Key+"': onError must be \"stop\" or \"continue\"")
				}
			}
		case "logic.branch", "logic.filter":
			if cond, ok := n.Config["condition"].(string); ok && ConditionChecker != nil {
				if err := ConditionChecker(cond); err != nil {
					hint := ". Write a comparison (== != > < >= <= contains), or several joined with && and ||"
					if strings.Contains(strings.ReplaceAll(cond, "||", ""), "|") {
						hint += ". A condition is not jq and has no pipes: a non-empty list is already true, so test the reference on its own," +
							" or work the value out in a data.transform step first"
					}
					errs = append(errs, "node '"+n.Key+"': "+err.Error()+hint)
				}
			}
		case "data.transform":
			expr, _ := n.Config["expression"].(string)
			if _, err := gojq.Parse(expr); err != nil {
				errs = append(errs, "node '"+n.Key+"': expression is not valid jq ("+err.Error()+")")
			} else if tplRef.MatchString(expr) {
				errs = append(errs, "node '"+n.Key+"': a jq expression does not use {{...}}; read data as .outputs.<dependency key>.<field> or .input.<field>")
			} else {
				if msg := jqUnavailable(expr); msg != "" {
					errs = append(errs, "node '"+n.Key+"': "+msg)
				}
				for _, p := range jqRootProblems(expr) {
					errs = append(errs, "node '"+n.Key+"': "+p)
				}
			}
		case "logic.wait":
			secs, until := strings.TrimSpace(fmt.Sprint(orEmpty(n.Config["seconds"]))), strings.TrimSpace(fmt.Sprint(orEmpty(n.Config["until"])))
			if secs == "" && until == "" {
				errs = append(errs, "node '"+n.Key+"' (logic.wait) needs seconds or until")
			}
		}
	}
	return errs
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// LoopBody returns the keys of the nodes that run once per item of the loop:
// every node all of whose dependencies are the loop or other body nodes, except
// one that reads the loop's collected results. The runtime applies the same rule.
func LoopBody(nodes []GraphNode, loop string) []string {
	body := map[string]bool{}
	var out []string
	for _, n := range topoOrder(nodes) {
		if n.Key == loop || len(n.DependsOn) == 0 {
			continue
		}
		if raw, err := json.Marshal(n.Config); err == nil && strings.Contains(string(raw), "outputs."+loop+".results") {
			continue
		}
		inside := true
		for _, d := range n.DependsOn {
			if d != loop && !body[d] {
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

// topoOrder returns the nodes dependencies first, ties broken by the given
// order. It assumes the graph has no cycle.
func topoOrder(nodes []GraphNode) []GraphNode {
	indeg := map[string]int{}
	dependents := map[string][]string{}
	known := map[string]bool{}
	for _, n := range nodes {
		known[n.Key] = true
	}
	for _, n := range nodes {
		for _, d := range n.DependsOn {
			if known[d] {
				indeg[n.Key]++
				dependents[d] = append(dependents[d], n.Key)
			}
		}
	}
	placed := map[string]bool{}
	var out []GraphNode
	for len(out) < len(nodes) {
		progressed := false
		for _, n := range nodes {
			if !placed[n.Key] && indeg[n.Key] == 0 {
				placed[n.Key], progressed = true, true
				out = append(out, n)
				for _, c := range dependents[n.Key] {
					indeg[c]--
				}
			}
		}
		if !progressed {
			break
		}
	}
	return out
}

// findCycle runs Kahn's algorithm; returns the stuck keys ("a, b") or "".
func findCycle(nodes []GraphNode) string {
	indeg := map[string]int{}
	dependents := map[string][]string{}
	byKey := map[string]bool{}
	for _, n := range nodes {
		byKey[n.Key] = true
		if _, ok := indeg[n.Key]; !ok {
			indeg[n.Key] = 0
		}
	}
	for _, n := range nodes {
		for _, dep := range n.DependsOn {
			if !byKey[dep] {
				continue // reported separately by validateGraph
			}
			indeg[n.Key]++
			dependents[dep] = append(dependents[dep], n.Key)
		}
	}
	var ready []string
	for k, d := range indeg {
		if d == 0 {
			ready = append(ready, k)
		}
	}
	seen := 0
	for len(ready) > 0 {
		k := ready[0]
		ready = ready[1:]
		seen++
		for _, child := range dependents[k] {
			indeg[child]--
			if indeg[child] == 0 {
				ready = append(ready, child)
			}
		}
	}
	if seen == len(indeg) {
		return ""
	}
	var stuck []string
	for k, d := range indeg {
		if d > 0 {
			stuck = append(stuck, k)
		}
	}
	sort.Strings(stuck)
	return strings.Join(stuck, ", ")
}

/* --------------------------------- layout --------------------------------- */

// layoutGraph assigns (col,row): col = longest-path depth from the roots, row =
// running index within the column. The Go mirror of reasoner/plan.py _grid, so
// prompt-compiled graphs render identically in the builder and the run view.
func layoutGraph(nodes []GraphNode) []GraphNode {
	byKey := map[string]*GraphNode{}
	for i := range nodes {
		byKey[nodes[i].Key] = &nodes[i]
	}
	depth := map[string]int{}
	var colOf func(key string, seen map[string]bool) int
	colOf = func(key string, seen map[string]bool) int {
		if d, ok := depth[key]; ok {
			return d
		}
		n, ok := byKey[key]
		if !ok {
			return 0
		}
		d := 0
		for _, dep := range n.DependsOn {
			if _, exists := byKey[dep]; !exists || seen[dep] {
				continue
			}
			seen[key] = true
			if c := colOf(dep, seen) + 1; c > d {
				d = c
			}
			delete(seen, key)
		}
		depth[key] = d
		return d
	}
	rowPerCol := map[int]int{}
	for i := range nodes {
		c := colOf(nodes[i].Key, map[string]bool{})
		nodes[i].Col = c
		nodes[i].Row = rowPerCol[c]
		rowPerCol[c]++
	}
	return nodes
}
