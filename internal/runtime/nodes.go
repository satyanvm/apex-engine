package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/itchyny/gojq"
	"github.com/satyanvm/apex-engine/internal/oauth"
)

const (
	transformTimeout = 3 * time.Second // a reshape of at most 1.5 MB takes milliseconds; this bounds a runaway one
	maxTransformOut  = 10000
	pieceTimeout     = 3 * time.Minute
)

// waitNode works out how long a logic.wait pauses. The workflow does the
// waiting, on a durable timer.
func waitNode(in NodeInput, scope Scope) (NodeResult, error) {
	cfg, err := scope.Render(in.Node.Config)
	if err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	c := cfg.(map[string]any)
	if until := strings.TrimSpace(str(c["until"])); until != "" {
		at, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return NodeResult{}, fatal("until must be a time like 2026-12-01T09:00:00Z, got %q", until)
		}
		secs := time.Until(at).Seconds()
		if secs < 0 {
			secs = 0
		}
		return NodeResult{Output: map[string]any{"seconds": secs, "until": at.UTC().Format(time.RFC3339)}}, nil
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(str(c["seconds"])), 64)
	if err != nil || secs < 0 {
		return NodeResult{}, fatal("a wait needs seconds (a number, 0 or more) or until (a time)")
	}
	if time.Duration(secs*float64(time.Second)) > maxWait {
		return NodeResult{}, fatal("a wait can be at most 30 days")
	}
	return NodeResult{Output: map[string]any{"seconds": secs}}, nil
}

// transformNode reshapes data with a jq expression. The expression's input is
// {input, outputs, item, loop}: the same things a template can see.
func transformNode(ctx context.Context, in NodeInput, scope Scope) (NodeResult, error) {
	expr := strings.TrimSpace(str(in.Node.Config["expression"]))
	query, err := gojq.Parse(expr)
	if err != nil {
		return NodeResult{}, fatal("expression is not valid jq: %s", err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		return NodeResult{}, fatal("expression is not valid jq: %s", err)
	}
	// A round trip through JSON gives jq the plain types it expects and a copy
	// it cannot alter.
	raw, err := json.Marshal(scope.root())
	if err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	ctx, cancel := context.WithTimeout(ctx, transformTimeout)
	defer cancel()
	var values []any
	iter := code.RunWithContext(ctx, root)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, isErr := v.(error); isErr {
			if ctx.Err() != nil {
				return NodeResult{}, fatal("expression took longer than %s", transformTimeout)
			}
			return NodeResult{}, fatal("expression failed: %s", err)
		}
		if len(values) == maxTransformOut {
			return NodeResult{}, fatal("expression produced more than %d values; wrap it in [ ] to return one list", maxTransformOut)
		}
		values = append(values, v)
	}
	var result any
	switch len(values) {
	case 0:
		result = nil
	case 1:
		result = values[0]
	default:
		result = values
	}
	// jq numbers may be ints; normalise through JSON so the output looks like
	// every other node's.
	out, err := json.Marshal(result)
	if err != nil {
		return NodeResult{}, fatal("expression produced a value that is not JSON: %s", err)
	}
	if len(out) > maxBodyBytes {
		return NodeResult{}, fatal("expression result is larger than %d bytes", maxBodyBytes)
	}
	var normal any
	if err := json.Unmarshal(out, &normal); err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	return NodeResult{Output: map[string]any{"result": normal}}, nil
}

// pieceNode runs an Activepieces action on the pieces runner. The connection
// value is read here and sent straight to the runner, so it never enters
// workflow history.
func (a *Activities) pieceNode(ctx context.Context, in NodeInput, scope Scope) (NodeResult, error) {
	c, spec, err := a.findImported(ctx, in)
	if err != nil {
		return NodeResult{}, err
	}
	if a.PiecesURL == "" {
		return NodeResult{}, fatal("this engine has no pieces runner (set APEX_PIECES_URL), so %s cannot run", in.Node.Type)
	}
	rendered, err := scope.Render(in.Node.Config)
	if err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	props, err := coerceArgs(rendered.(map[string]any), spec)
	if err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	if in.Preview {
		// A piece action does not say whether it reads or writes, so a preview
		// never runs one.
		return planned("call", map[string]any{"tool": c.Name, "operation": spec.Label, "arguments": props}), nil
	}
	creds, err := a.Store.Credentials(ctx, in.Tenant, c.ID)
	if err != nil {
		return NodeResult{}, err
	}
	var auth any
	if len(creds.Piece) > 0 {
		if err := json.Unmarshal(creds.Piece, &auth); err != nil {
			return NodeResult{}, fatal("stored connection is not valid JSON")
		}
	}
	if creds.OAuth != nil {
		token, err := oauth.Token(ctx, a.Store, in.Tenant, c.ID, false)
		if err != nil {
			return NodeResult{}, oauthError(err)
		}
		conn := map[string]any{"access_token": token, "token_type": "Bearer", "data": map[string]any{}}
		if extra, ok := auth.(map[string]any); ok {
			conn["props"] = extra
		}
		auth = conn
	}
	piece, _ := spec.Exec["piece"].(string)
	action, _ := spec.Exec["action"].(string)
	body, err := json.Marshal(map[string]any{"piece": piece, "action": action, "props": props, "auth": auth,
		"run": map[string]any{"id": in.RunID, "node": in.Node.Key}})
	if err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	ctx, cancel := context.WithTimeout(ctx, pieceTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(a.PiecesURL, "/")+"/execute", bytes.NewReader(body))
	if err != nil {
		return NodeResult{}, fatal("%s", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.PiecesToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.PiecesToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return NodeResult{}, fmt.Errorf("pieces runner unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return NodeResult{}, err
	}
	if len(raw) > maxBodyBytes {
		return NodeResult{}, fatal("action result is larger than %d bytes", maxBodyBytes)
	}
	var answer struct {
		OK     bool   `json:"ok"`
		Output any    `json:"output"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		if resp.StatusCode >= 500 {
			return NodeResult{}, fmt.Errorf("pieces runner answered HTTP %d", resp.StatusCode)
		}
		return NodeResult{}, fatal("pieces runner answered HTTP %d: %s", resp.StatusCode, snippet(string(raw)))
	}
	if !answer.OK {
		if resp.StatusCode >= 500 { // the runner itself failed, not the action
			return NodeResult{}, fmt.Errorf("pieces runner: %s", answer.Error)
		}
		return NodeResult{}, fatal("%s", answer.Error)
	}
	return NodeResult{Output: map[string]any{"result": answer.Output}}, nil
}
