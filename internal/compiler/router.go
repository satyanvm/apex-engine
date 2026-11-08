package compiler

// router.go narrows a tenant's tool groups before the map phase: one small-model
// call over a directory with one line per group. A failure fails the compile.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// directClusterLimit is the group count below which routing is skipped.
const directClusterLimit = 3

// routerTimeout allows for the larger directory prompt (~35k tokens full,
// ~5k prefiltered) while keeping plan latency interactive.
// Generous enough to wait out one free-tier per-minute rate limit (geminiPost).
const routerTimeout = 75 * time.Second

// customAPICall is boilerplate present in most pieces; stated once in the
// directory header instead of repeated ~455 times.
const customAPICall = "Custom API Call"

// clusterDirectory renders one entry per cluster: slug, display name,
// categories, then EVERY action label (and trigger labels, marked) — no
// truncation, so no capability is invisible to the router. Keys are rendered
// in the given order (prefilter relevance order, or alphabetical for the full
// directory). Hand-written cluster keys ("communication", …) render the same
// way; their key doubles as the slug.
func clusterDirectory(cat *Catalog, keys []string) string {
	var b strings.Builder
	b.WriteString("Every piece also exposes a \"" + customAPICall + "\" action (arbitrary authenticated call to that service's API); it is omitted from the lists below.\n\n")
	for _, key := range keys {
		f, ok := cat.Clusters[key]
		if !ok {
			continue
		}
		slug := strings.TrimPrefix(key, "pieces.")
		b.WriteString(slug)
		b.WriteString(" — ")
		b.WriteString(strings.TrimSuffix(f.Label, " (Activepieces)"))
		if len(f.Categories) > 0 {
			b.WriteString(" [" + strings.Join(f.Categories, ", ") + "]")
		}
		b.WriteString(": ")
		var actions, triggers []string
		for _, n := range f.Nodes {
			if n.Label == customAPICall {
				continue
			}
			if n.Kind == "trigger" {
				triggers = append(triggers, n.Label)
			} else {
				actions = append(actions, n.Label)
			}
		}
		b.WriteString(strings.Join(actions, "; "))
		if len(triggers) > 0 {
			if len(actions) > 0 {
				b.WriteString(" | ")
			}
			b.WriteString("triggers: " + strings.Join(triggers, "; "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// routeClusters picks up to max cluster keys (piece AND hand-written) for the
// map phase with one small-model call over the cluster directory. The returned
// order is the model's own ranking — downstream (capFairly) uses it to decide
// which cluster contributes the marginal node, so it is deliberately NOT
// re-sorted. Any failure (no key, timeout, junk output, nothing valid) is
// returned as an error and fails the compile.
func routeClusters(ctx context.Context, cat *Catalog, prompt string, clusterKeys []string, max int) ([]string, error) {
	if len(clusterKeys) == 0 {
		return nil, nil // nothing to route is a valid answer, not a failure
	}

	// A handful of groups costs less to search directly than to route.
	if len(clusterKeys) <= directClusterLimit {
		return clusterKeys, nil
	}
	keys := clusterKeys

	system := `You route a user's automation request to integration clusters (each is one service
or capability area, offering the listed actions/triggers).
Given the request and a directory of clusters (one entry per cluster: "slug — Name [categories]: action labels | triggers: ..."),
return JSON: {"clusters": ["slug", ...]} listing ONLY slugs from the directory whose actions or
triggers could plausibly serve the request, MOST relevant first. Include every plausibly relevant
cluster (a later stage narrows precisely), but never pad with irrelevant ones. At most ` + itoa(max) + `.
Return {"clusters": []} if none apply.`

	user := "Request: " + prompt + "\n\nCluster directory:\n" + clusterDirectory(cat, keys)
	raw, err := planLLMWith(ctx, mapModel(), 1024, routerTimeout, system, []llmMsg{{Role: "user", Content: user}})
	if err != nil {
		return nil, fmt.Errorf("cluster router: %w", err)
	}
	var out struct {
		Clusters []string `json:"clusters"`
		Pieces   []string `json:"pieces"` // tolerated: the field name used by an earlier prompt
	}
	if err := json.Unmarshal([]byte(extractJSON(raw)), &out); err != nil {
		return nil, fmt.Errorf("cluster router returned unparseable JSON: %w", err)
	}
	picked := out.Clusters
	if len(picked) == 0 {
		picked = out.Pieces
	}

	// Validate against the offered directory (not the whole catalog): the model
	// may only pick from what it was shown. A hand-written cluster's slug IS its
	// key; a piece slug maps to pieces.<slug>.
	offered := map[string]bool{}
	for _, k := range keys {
		offered[k] = true
	}
	var selected []string
	seen := map[string]bool{}
	for _, slug := range picked {
		key := strings.TrimSpace(slug)
		if offered[key] && !seen[key] {
			seen[key] = true
			selected = append(selected, key)
			if len(selected) == max {
				break
			}
		}
	}
	if len(selected) == 0 && len(picked) > 0 {
		// The model answered but named nothing in the directory it was shown.
		return nil, fmt.Errorf("cluster router picked %d cluster(s), none of them in the offered directory: %s",
			len(picked), truncateStr(strings.Join(picked, ", "), 200))
	}
	return selected, nil
}
