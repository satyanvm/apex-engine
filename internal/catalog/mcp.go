package catalog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/satyanvm/apex-engine/internal/compiler"
	"github.com/satyanvm/apex-engine/internal/netguard"
)

const mcpProtocol = "2025-06-18"

// MCPClient is a minimal Streamable HTTP client: initialize, tools/list, tools/call.
type MCPClient struct {
	url     string
	headers map[string]string
	http    *http.Client
	session string
	nextID  int
}

// NewMCPClient connects to url and completes the handshake.
func NewMCPClient(ctx context.Context, url string, headers map[string]string, timeout time.Duration) (*MCPClient, error) {
	c := &MCPClient{url: url, headers: headers, http: netguard.Client(timeout, false)}
	if _, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocol,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "apex-engine", "version": "1.0"},
	}); err != nil {
		return nil, fmt.Errorf("MCP initialize: %w", err)
	}
	if err := c.notify(ctx, "notifications/initialized"); err != nil {
		return nil, fmt.Errorf("MCP initialized notification: %w", err)
	}
	return c, nil
}

type rpcResponse struct {
	ID     *json.RawMessage `json:"id"`
	Result json.RawMessage  `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *MCPClient) post(ctx context.Context, body map[string]any) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", mcpProtocol)
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	return c.http.Do(req)
}

func (c *MCPClient) notify(ctx context.Context, method string) error {
	resp, err := c.post(ctx, map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *MCPClient) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	c.nextID++
	resp, err := c.post(ctx, map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.session = s
	}
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(snippet))}
	}
	var r *rpcResponse
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		r, err = c.readStream(resp.Body)
	} else {
		r = &rpcResponse{}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(r)
	}
	if err != nil {
		return nil, fmt.Errorf("unreadable response: %w", err)
	}
	if r.Error != nil {
		return nil, fmt.Errorf("server error %d: %s", r.Error.Code, r.Error.Message)
	}
	return r.Result, nil
}

// HTTPError is a non-2xx response from an MCP server.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// readStream returns the event answering our request, ignoring server notifications.
func (c *MCPClient) readStream(body io.Reader) (*rpcResponse, error) {
	sc := bufio.NewScanner(io.LimitReader(body, 8<<20))
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var data strings.Builder
	flush := func() (*rpcResponse, bool) {
		defer data.Reset()
		if data.Len() == 0 {
			return nil, false
		}
		var r rpcResponse
		if json.Unmarshal([]byte(data.String()), &r) != nil || r.ID == nil {
			return nil, false
		}
		return &r, true
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if r, ok := flush(); ok {
				return r, nil
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			data.WriteString(strings.TrimPrefix(rest, " "))
		}
	}
	if r, ok := flush(); ok {
		return r, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("stream ended without a response")
}

// MCPTool is one tool a server advertises.
type MCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ListTools returns every tool, following pagination.
func (c *MCPClient) ListTools(ctx context.Context) ([]MCPTool, error) {
	var (
		tools  []MCPTool
		cursor string
	)
	for page := 0; page < 50; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		var out struct {
			Tools      []MCPTool `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		tools = append(tools, out.Tools...)
		if out.NextCursor == "" {
			return tools, nil
		}
		cursor = out.NextCursor
	}
	return nil, errors.New("tools/list did not finish after 50 pages")
}

// ToolError is a tool that ran and reported failure.
type ToolError struct{ Message string }

func (e *ToolError) Error() string { return "tool reported an error: " + e.Message }

// CallTool invokes a tool and returns its result: structuredContent when the
// server provides it, else the text content (parsed when it is JSON).
func (c *MCPClient) CallTool(ctx context.Context, name string, args map[string]any) (any, error) {
	raw, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	var out struct {
		IsError           bool `json:"isError"`
		StructuredContent any  `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("tools/call: %w", err)
	}
	var texts []string
	for _, c := range out.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	text := strings.Join(texts, "\n")
	if out.IsError {
		return nil, &ToolError{Message: clip(text, 1000)}
	}
	if out.StructuredContent != nil {
		return out.StructuredContent, nil
	}
	var parsed any
	if json.Unmarshal([]byte(text), &parsed) == nil {
		return parsed, nil
	}
	return text, nil
}

// MCPNodes converts advertised tools to planner nodes.
func MCPNodes(slug string, tools []MCPTool) []compiler.CatalogNode {
	nodes := make([]compiler.CatalogNode, 0, len(tools))
	used := map[string]bool{}
	for _, t := range tools {
		id := strings.Trim(nonIdent.ReplaceAllString(t.Name, "_"), "_")
		if id == "" || used[id] {
			continue
		}
		used[id] = true
		props, _ := t.InputSchema["properties"].(map[string]any)
		required := map[string]bool{}
		if list, ok := t.InputSchema["required"].([]any); ok {
			for _, r := range list {
				if s, ok := r.(string); ok {
					required[s] = true
				}
			}
		}
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var (
			fields []compiler.CatalogField
			types  = map[string]any{}
		)
		for _, k := range keys {
			schema, _ := props[k].(map[string]any)
			typ, _ := schema["type"].(string)
			desc, _ := schema["description"].(string)
			control := "text"
			if typ == "object" || typ == "array" {
				control = "json"
			}
			types[k] = typ
			fields = append(fields, compiler.CatalogField{Key: k, Label: k, Control: control, Required: required[k], Help: clip(desc, 200)})
		}
		label := t.Name
		desc := t.Description
		if desc == "" {
			desc = t.Name
		}
		nodes = append(nodes, compiler.CatalogNode{
			ID: "mcp." + slug + "." + id, Label: clip(label, 80), Description: clip(desc, maxDescLen),
			Kind: "action", Runtime: "mcp", Config: fields, Outputs: []string{"result"},
			Exec: map[string]any{"tool": t.Name, "types": types},
		})
	}
	return nodes
}
