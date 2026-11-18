package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/satyanvm/apex-engine/internal/compiler"
	"github.com/satyanvm/apex-engine/internal/netguard"
	"github.com/satyanvm/apex-engine/internal/oauth"
	"github.com/satyanvm/apex-engine/internal/store"
)

const (
	defaultMaxPages = 20
	hardMaxPages    = 200
	maxItemsBytes   = 1 << 20
)

// oauthError turns a token failure into a node failure: a refused grant is
// final, a flaky token endpoint is retried.
func oauthError(err error) error {
	var oe *oauth.Error
	if errors.As(err, &oe) && oe.Permanent {
		return fatal("%s", oe.Msg)
	}
	return err
}

func isTrue(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, _ := strconv.ParseBool(strings.TrimSpace(t))
		return b
	case float64:
		return t != 0
	}
	return false
}

func (a *Activities) apiNode(ctx context.Context, in NodeInput, scope Scope) (NodeResult, error) {
	c, spec, err := a.findImported(ctx, in)
	if err != nil {
		return NodeResult{}, err
	}
	rendered, err := scope.Render(in.Node.Config)
	if err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	cfg := rendered.(map[string]any)
	creds, err := a.Store.Credentials(ctx, in.Tenant, c.ID)
	if err != nil {
		return NodeResult{}, err
	}
	call := &apiCall{a: a, in: in, cat: c, creds: creds}
	method, _ := spec.Exec["method"].(string)
	pg := paginationOf(spec)
	if pg == nil || method != http.MethodGet {
		req, err := buildAPIRequest(ctx, c.BaseURL, spec, cfg)
		if err != nil {
			return NodeResult{}, fatal("%s", err)
		}
		if in.Preview && !isRead(method) {
			return planned("call", map[string]any{"tool": c.Name, "operation": spec.Label, "method": method, "url": req.URL.String(), "body": bodyOf(req)}), nil
		}
		status, body, _, err := call.do(req)
		if err != nil {
			return NodeResult{}, err
		}
		return NodeResult{Output: map[string]any{"status": float64(status), "body": body}}, nil
	}
	return call.paged(ctx, spec, cfg, pg)
}

type apiCall struct {
	a     *Activities
	in    NodeInput
	cat   *store.Catalog
	creds *store.Credentials
}

// do sends one request with the catalog's credentials. With OAuth, a 401 gets
// one more try on a freshly fetched token.
func (c *apiCall) do(req *http.Request) (int, any, http.Header, error) {
	for attempt := 0; ; attempt++ {
		r := req.Clone(req.Context())
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return 0, nil, nil, err
			}
			r.Body = body
		}
		if len(c.creds.Query) > 0 {
			q := r.URL.Query()
			for k, v := range c.creds.Query {
				q.Set(k, v)
			}
			r.URL.RawQuery = q.Encode()
		}
		for k, v := range c.creds.Headers {
			r.Header.Set(k, v)
		}
		if c.creds.OAuth != nil {
			token, err := oauth.Token(r.Context(), c.a.Store, c.in.Tenant, c.cat.ID, attempt > 0)
			if err != nil {
				return 0, nil, nil, oauthError(err)
			}
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if r.Method != http.MethodGet {
			// Stable across retries and across a resumed run, so servers that
			// honour it dedupe a repeated call.
			root := c.in.IdemRoot
			if root == "" {
				root = c.in.RunID
			}
			key := root + ":" + c.in.Node.Key
			if c.in.HasItem {
				key += ":" + strconv.Itoa(c.in.Index)
			}
			r.Header.Set("Idempotency-Key", key)
		}
		resp, err := netguard.Client(httpTimeout, false).Do(r)
		if err != nil {
			return 0, nil, nil, transportError(err)
		}
		parsed, err := readBody(resp)
		resp.Body.Close()
		if err != nil {
			return 0, nil, nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && c.creds.OAuth != nil && attempt == 0 {
			continue
		}
		if retryableStatus(resp.StatusCode) {
			return 0, nil, nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, c.cat.Name, snippet(parsed))
		}
		if resp.StatusCode >= 300 {
			return 0, nil, nil, fatal("HTTP %d from %s: %s", resp.StatusCode, c.cat.Name, snippet(parsed))
		}
		return resp.StatusCode, parsed, resp.Header, nil
	}
}

/* --------------------------------- pagination ------------------------------- */

// pagination is how an imported list operation asks for its next page. It is
// worked out from the operation's parameter names when the spec is imported.
type pagination struct {
	Style string // page, offset or cursor
	Param string
	Limit string
}

func paginationOf(spec compiler.CatalogNode) *pagination {
	m, _ := spec.Exec["pagination"].(map[string]any)
	if m == nil {
		return nil
	}
	p := &pagination{}
	p.Style, _ = m["style"].(string)
	p.Param, _ = m["param"].(string)
	p.Limit, _ = m["limit"].(string)
	if p.Style == "" {
		return nil
	}
	return p
}

var listKeys = []string{"data", "items", "results", "records", "values", "value", "entries", "list", "rows", "objects", "resources", "hits", "nodes", "edges", "content", "elements"}

// pageItems finds the list in a page: the body itself, a well-known field, or
// the only field that is a list.
func pageItems(body any) ([]any, bool) {
	switch t := body.(type) {
	case []any:
		return t, true
	case map[string]any:
		for _, k := range listKeys {
			if l, ok := t[k].([]any); ok {
				return l, true
			}
		}
		var found []any
		n := 0
		for _, v := range t {
			if l, ok := v.([]any); ok {
				found, n = l, n+1
			}
		}
		if n == 1 {
			return found, true
		}
	}
	return nil, false
}

var (
	cursorKeys = []string{"next_cursor", "nextCursor", "next_page_token", "nextPageToken", "next_token", "nextToken",
		"continuation_token", "continuationToken", "continuation", "next_page", "nextPage", "next", "after", "cursor"}
	moreKeys = []string{"has_more", "hasMore", "has_next", "hasNext", "has_next_page", "hasNextPage", "more"}
	linkNext = regexp.MustCompile(`<([^>]+)>\s*;[^,]*rel="?next"?`)
)

// findKey looks for the first of keys in a JSON object, descending into nested
// objects (never lists) up to three levels: enough for meta.next_cursor,
// paging.cursors.after and response_metadata.next_cursor.
func findKey(v any, keys []string, depth int) (any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	for _, k := range keys {
		if x, ok := m[k]; ok && x != nil {
			switch x.(type) {
			case string, float64, bool:
				return x, true
			}
		}
	}
	if depth == 0 {
		return nil, false
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sortStrings(names)
	for _, k := range names {
		if _, isMap := m[k].(map[string]any); isMap {
			if x, ok := findKey(m[k], keys, depth-1); ok {
				return x, true
			}
		}
	}
	return nil, false
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func sameOrigin(base, target string) bool {
	b, err1 := url.Parse(base)
	t, err2 := url.Parse(target)
	return err1 == nil && err2 == nil && strings.EqualFold(b.Scheme, t.Scheme) && strings.EqualFold(b.Host, t.Host)
}

// paged runs a list operation. It always returns the first page as status and
// body, plus items: the list from that page, or from every page when the node
// sets allPages.
func (c *apiCall) paged(ctx context.Context, spec compiler.CatalogNode, cfg map[string]any, pg *pagination) (NodeResult, error) {
	all := isTrue(cfg["allPages"])
	maxPages := defaultMaxPages
	if raw := strings.TrimSpace(str(cfg["maxPages"])); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > hardMaxPages {
			return NodeResult{}, fatal("maxPages must be a whole number from 1 to %d", hardMaxPages)
		}
		maxPages = n
	}
	// Each page is fetched with the node's own parameters plus the one that
	// moves it forward, so nothing the caller set is lost between pages.
	params := map[string]any{}
	for k, v := range cfg {
		params[k] = v
	}
	var (
		first     any
		status    int
		items     []any
		size      int
		pages     int
		truncated bool
		nextURL   string
		lastHead  string
	)
	for {
		var (
			req *http.Request
			err error
		)
		if nextURL != "" {
			req, err = http.NewRequestWithContext(ctx, http.MethodGet, nextURL, nil)
			if err == nil {
				req.Header.Set("Accept", "application/json")
			}
		} else {
			req, err = buildAPIRequest(ctx, c.cat.BaseURL, spec, params)
		}
		if err != nil {
			return NodeResult{}, fatal("%s", err)
		}
		code, body, header, err := c.do(req)
		if err != nil {
			return NodeResult{}, err
		}
		page, ok := pageItems(body)
		if pages == 0 {
			first, status = body, code
			if !ok {
				if all {
					return NodeResult{}, fatal("allPages is set, but the response has no list to collect")
				}
				return NodeResult{Output: map[string]any{"status": float64(code), "body": body, "pages": float64(1), "truncated": false}}, nil
			}
		}
		// An API that ignores the paging parameter answers with the same page
		// again. Stop, rather than collect it until the page limit.
		head := ""
		if len(page) > 0 {
			raw, _ := json.Marshal(page[0])
			head = string(raw)
		}
		if pages > 0 && (len(page) == 0 || head == lastHead) {
			break
		}
		pages++
		lastHead = head
		for _, it := range page {
			raw, _ := json.Marshal(it)
			size += len(raw)
		}
		if size > maxItemsBytes {
			return NodeResult{}, fatal("the pages collected so far are larger than 1 MB; filter the request, or lower maxPages")
		}
		items = append(items, page...)
		if !all {
			break
		}
		next, more := nextPage(pg, params, body, header, page)
		if !more {
			break
		}
		if pages >= maxPages {
			truncated = true
			break
		}
		nextURL = ""
		if u, isURL := next.(string); isURL && (strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) {
			if !sameOrigin(c.cat.BaseURL, u) {
				return NodeResult{}, fatal("the next page is on another host (%s); credentials are only sent to %s", u, c.cat.BaseURL)
			}
			nextURL = u
		} else {
			params[pg.Param] = next
		}
	}
	if items == nil {
		items = []any{}
	}
	return NodeResult{Output: map[string]any{
		"status": float64(status), "body": first, "items": items, "pages": float64(pages), "truncated": truncated,
	}}, nil
}

// nextPage works out how to ask for the page after this one. more is false when
// this was the last page.
func nextPage(pg *pagination, params map[string]any, body any, header http.Header, page []any) (next any, more bool) {
	if len(page) == 0 {
		return nil, false
	}
	if m := linkNext.FindStringSubmatch(header.Get("Link")); m != nil {
		return m[1], true
	}
	if v, ok := findKey(body, moreKeys, 2); ok {
		if b, isBool := v.(bool); isBool && !b {
			return nil, false
		}
	}
	limit := 0
	if pg.Limit != "" {
		limit, _ = strconv.Atoi(strings.TrimSpace(str(params[pg.Limit])))
	}
	switch pg.Style {
	case "page":
		if limit > 0 && len(page) < limit {
			return nil, false
		}
		cur := 1
		if raw := strings.TrimSpace(str(params[pg.Param])); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return nil, false
			}
			cur = n
		}
		return strconv.Itoa(cur + 1), true
	case "offset":
		if limit > 0 && len(page) < limit {
			return nil, false
		}
		cur, _ := strconv.Atoi(strings.TrimSpace(str(params[pg.Param])))
		return strconv.Itoa(cur + len(page)), true
	case "cursor":
		switch strings.ToLower(pg.Param) {
		case "starting_after", "after_id", "since_id":
			// Stripe style: the cursor is the id of the last item seen.
			if v, ok := findKey(body, moreKeys, 2); !ok || v != true {
				return nil, false
			}
			last, _ := page[len(page)-1].(map[string]any)
			if id := str(last["id"]); id != "" {
				return id, true
			}
			return nil, false
		}
		if v, ok := findKey(body, cursorKeys, 3); ok {
			if s := str(v); s != "" && s != "false" {
				return s, true
			}
		}
	}
	return nil, false
}
