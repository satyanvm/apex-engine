// Command demo-api is a small Orders API with an OpenAPI document and an MCP
// endpoint, for trying apex-engine and for the crash-recovery demo. Its data is
// in memory and it is not meant to be deployed.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const token = "Bearer demo-token"

type order struct {
	ID       string  `json:"id"`
	Customer string  `json:"customer"`
	Total    float64 `json:"total"`
	Status   string  `json:"status"`
}

var (
	mu     sync.Mutex
	hits   = map[string]int{}
	orders = []order{
		{"ord_1", "Ada", 120, "late"}, {"ord_2", "Grace", 42, "shipped"}, {"ord_3", "Linus", 310, "late"},
	}
)

func count(name string) {
	mu.Lock()
	hits[name]++
	mu.Unlock()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			return
		}
		next(w, r)
	}
}

func spec(base string) map[string]any {
	ok := func(schema map[string]any) map[string]any {
		return map[string]any{"200": map[string]any{"description": "ok", "content": map[string]any{"application/json": map[string]any{"schema": schema}}}}
	}
	orderSchema := map[string]any{"type": "object", "properties": map[string]any{
		"id": map[string]any{"type": "string"}, "customer": map[string]any{"type": "string"},
		"total": map[string]any{"type": "number"}, "status": map[string]any{"type": "string"},
	}}
	return map[string]any{
		"openapi": "3.0.3",
		"info":    map[string]any{"title": "Demo Orders", "version": "1"},
		"servers": []any{map[string]any{"url": base}},
		"paths": map[string]any{
			"/orders": map[string]any{"get": map[string]any{
				"operationId": "listOrders", "summary": "List orders",
				"parameters": []any{map[string]any{"name": "status", "in": "query", "description": "Only orders with this status (late, shipped)", "schema": map[string]any{"type": "string"}}},
				"responses":  ok(map[string]any{"type": "array", "items": orderSchema}),
			}},
			"/orders/{id}/refund": map[string]any{"post": map[string]any{
				"operationId": "refundOrder", "summary": "Refund an order",
				"parameters": []any{map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}},
				"responses":  ok(orderSchema),
			}},
			"/slow/{name}": map[string]any{"get": map[string]any{
				"operationId": "slowStep", "summary": "Wait, then return. For the crash-recovery demo.",
				"parameters": []any{
					map[string]any{"name": "name", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					map[string]any{"name": "seconds", "in": "query", "schema": map[string]any{"type": "integer"}},
				},
				"responses": ok(map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}),
			}},
		},
	}
}

func handleMCP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     any `json:"id"`
		Method string
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "demo")
		result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "demo", "version": "1"}}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{
			"name": "lookup_policy", "description": "Look up the refund policy for an order total",
			"inputSchema": map[string]any{"type": "object", "required": []any{"total"}, "properties": map[string]any{"total": map[string]any{"type": "number", "description": "Order total in dollars"}}},
		}}}
	case "tools/call":
		count("mcp:" + req.Params.Name)
		total, _ := req.Params.Arguments["total"].(float64)
		text := fmt.Sprintf(`{"refundable":%v,"limit":200}`, total <= 200)
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	default:
		writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func main() {
	addr := ":8701"
	if v := os.Getenv("DEMO_ADDR"); v != "" {
		addr = v
	}
	base := "http://localhost" + addr
	if v := os.Getenv("DEMO_BASE_URL"); v != "" {
		base = v
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, spec(base)) })
	mux.HandleFunc("GET /hits", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		writeJSON(w, http.StatusOK, hits)
	})
	mux.HandleFunc("POST /mcp", handleMCP)
	mux.HandleFunc("GET /orders", authed(func(w http.ResponseWriter, r *http.Request) {
		count("listOrders")
		out := []order{}
		want := r.URL.Query().Get("status")
		mu.Lock()
		defer mu.Unlock()
		for _, o := range orders {
			if want == "" || o.Status == want {
				out = append(out, o)
			}
		}
		writeJSON(w, http.StatusOK, out)
	}))
	mux.HandleFunc("POST /orders/{id}/refund", authed(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		count("refund:" + id + ":" + r.Header.Get("Idempotency-Key"))
		mu.Lock()
		defer mu.Unlock()
		for i := range orders {
			if orders[i].ID == id {
				orders[i].Status = "refunded"
				writeJSON(w, http.StatusOK, orders[i])
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such order"})
	}))
	mux.HandleFunc("GET /slow/{name}", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		count("slow:" + name)
		secs, _ := strconv.Atoi(r.URL.Query().Get("seconds"))
		select {
		case <-time.After(time.Duration(secs) * time.Second):
			count("slow-finished:" + name)
			writeJSON(w, http.StatusOK, map[string]string{"name": name})
		case <-r.Context().Done():
		}
	}))
	log.Printf("demo API on %s (token: %s)", addr, strings.TrimPrefix(token, "Bearer "))
	log.Fatal(http.ListenAndServe(addr, mux))
}
