package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const petstore = `
openapi: 3.0.3
servers: [{url: "https://{env}.pets.test/v1", variables: {env: {default: api}}}]
paths:
  /pets/{id}:
    parameters:
      - {name: id, in: path, schema: {type: string}, description: The pet}
    get:
      operationId: getPet
      summary: Fetch a pet
      tags: [pets]
      parameters:
        - {$ref: "#/components/parameters/Verbose"}
        - {name: Authorization, in: header, schema: {type: string}}
      responses:
        200:
          description: ok
          content:
            application/json:
              schema: {$ref: "#/components/schemas/Pet"}
  /pets:
    post:
      summary: Create a pet
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: "#/components/schemas/Pet"}
      responses: {201: {description: created}}
components:
  parameters:
    Verbose: {name: verbose, in: query, schema: {type: boolean}}
  schemas:
    Pet:
      type: object
      properties: {name: {type: string}, age: {type: integer}}
`

func TestParseOpenAPI(t *testing.T) {
	api, err := ParseOpenAPI([]byte(petstore), "pets", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if api.BaseURL != "https://api.pets.test/v1" {
		t.Errorf("BaseURL = %q", api.BaseURL)
	}
	if len(api.Nodes) != 2 {
		t.Fatalf("got %d nodes", len(api.Nodes))
	}
	create, get := api.Nodes[0], api.Nodes[1]
	if get.ID != "api.pets.getPet" || create.ID != "api.pets.post_pets" {
		t.Errorf("ids = %s, %s", get.ID, create.ID)
	}
	if !strings.Contains(get.Description, "age: integer, name: string") {
		t.Errorf("description lacks the response shape: %q", get.Description)
	}
	keys := map[string]bool{}
	for _, f := range get.Config {
		keys[f.Key] = f.Required
	}
	if req, ok := keys["id"]; !ok || !req {
		t.Errorf("path parameter from the path item should be required: %v", keys)
	}
	if _, ok := keys["verbose"]; !ok {
		t.Errorf("$ref parameter was not resolved: %v", keys)
	}
	if _, ok := keys["Authorization"]; ok {
		t.Error("Authorization must come from stored credentials, not node config")
	}
	if len(create.Config) != 1 || create.Config[0].Key != "body" || !create.Config[0].Required {
		t.Errorf("body field wrong: %+v", create.Config)
	}
	if get.Exec["group"] != "pets" {
		t.Errorf("group = %v", get.Exec["group"])
	}
}

func TestParseOpenAPIRejects(t *testing.T) {
	for name, tc := range map[string]struct{ spec, want string }{
		"swagger":     {`{"swagger":"2.0","paths":{"/a":{}}}`, "Swagger 2.0"},
		"not openapi": {`{"hello":"world"}`, "not an OpenAPI 3"},
		"no server":   {`{"openapi":"3.0.0","paths":{"/a":{"get":{}}}}`, "no servers"},
		"bad scheme":  {`{"openapi":"3.0.0","servers":[{"url":"ftp://x"}],"paths":{"/a":{"get":{}}}}`, "http or https"},
		"no paths":    {`{"openapi":"3.0.0","servers":[{"url":"https://x"}]}`, "no paths"},
	} {
		if _, err := ParseOpenAPI([]byte(tc.spec), "x", "", ""); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestParseOpenAPICircularRefTerminates(t *testing.T) {
	spec := `{"openapi":"3.0.0","servers":[{"url":"https://x"}],"paths":{"/a":{"get":{"parameters":[{"$ref":"#/components/parameters/A"}]}}},
	"components":{"parameters":{"A":{"$ref":"#/components/parameters/B"},"B":{"$ref":"#/components/parameters/A"}}}}`
	if _, err := ParseOpenAPI([]byte(spec), "x", "", ""); err != nil {
		t.Fatal(err)
	}
}

// fakeMCP answers either plain JSON or SSE and requires the session header.
func fakeMCP(t *testing.T, sse bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "s1" {
			t.Errorf("%s arrived without the session id", req.Method)
		}
		if r.Header.Get("X-Key") != "k" {
			t.Errorf("%s arrived without the credential", req.Method)
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			result = map[string]any{}
		case "tools/list":
			if req.Params["cursor"] == nil {
				result = map[string]any{"nextCursor": "p2", "tools": []any{map[string]any{"name": "search.docs", "description": "Search docs",
					"inputSchema": map[string]any{"properties": map[string]any{"q": map[string]any{"type": "string"}, "opts": map[string]any{"type": "object"}}, "required": []any{"q"}}}}}
			} else {
				result = map[string]any{"tools": []any{map[string]any{"name": "fail"}}}
			}
		case "tools/call":
			if req.Params["name"] == "fail" {
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "nope"}}}
			} else {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"hits":3}`}}}
			}
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\ndata: " + string(body) + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

func TestMCPRoundTrip(t *testing.T) {
	t.Setenv("APEX_ALLOW_PRIVATE_NETWORK", "1")
	for _, sse := range []bool{false, true} {
		srv := fakeMCP(t, sse)
		ctx := context.Background()
		c, err := NewMCPClient(ctx, srv.URL, map[string]string{"X-Key": "k"}, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		tools, err := c.ListTools(ctx)
		if err != nil || len(tools) != 2 {
			t.Fatalf("sse=%v tools=%v err=%v", sse, tools, err)
		}
		nodes := MCPNodes("docs", tools)
		if nodes[0].ID != "mcp.docs.search_docs" || nodes[0].Exec["tool"] != "search.docs" {
			t.Errorf("node = %+v", nodes[0])
		}
		if !nodes[0].Config[1].Required || nodes[0].Config[0].Control != "json" {
			t.Errorf("fields = %+v", nodes[0].Config)
		}
		got, err := c.CallTool(ctx, "search.docs", map[string]any{"q": "x"})
		if err != nil {
			t.Fatal(err)
		}
		if got.(map[string]any)["hits"] != float64(3) {
			t.Errorf("result = %v", got)
		}
		if _, err := c.CallTool(ctx, "fail", nil); err == nil || !strings.Contains(err.Error(), "nope") {
			t.Errorf("tool error = %v", err)
		}
		srv.Close()
	}
}
