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
}

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
	}
	return errs
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
