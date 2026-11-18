package compiler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These run each provider's real client against a stand-in server, so what is
// asserted is the request that goes on the wire.

func anthropicStub(t *testing.T, stopReason string, got *map[string]any, header *http.Header) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, got)
		*header = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(event, data string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data) }
		send("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"x","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}`)
		send("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		send("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"ok\":"}}`)
		send("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"true}"}}`)
		send("content_block_stop", `{"type":"content_block_stop","index":0}`)
		send("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":5}}`, stopReason))
		send("message_stop", `{"type":"message_stop"}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
}

func TestAnthropicRequestAndStreamedAnswer(t *testing.T) {
	var body map[string]any
	var header http.Header
	anthropicStub(t, "end_turn", &body, &header)
	out, err := planLLMWith(context.Background(), "claude-opus-5-5", 16000, time.Minute, "be useful", []llmMsg{
		{Role: "user", Content: "one"}, {Role: "assistant", Content: "two"}, {Role: "user", Content: "three"},
	})
	if err != nil || out != `{"ok":true}` {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if body["fallbacks"] != "default" || !strings.Contains(header.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("fallback routing not requested: %v / %v", body["fallbacks"], header.Get("Anthropic-Beta"))
	}
	if cfg, _ := body["output_config"].(map[string]any); cfg["effort"] != "high" {
		t.Errorf("effort: %v", body["output_config"])
	}
	if !strings.Contains(header.Get("Authorization")+header.Get("X-Api-Key"), "test-key") {
		t.Errorf("the key was not sent: %v", header)
	}
	if turns := body["messages"].([]any); len(turns) != 3 || turns[1].(map[string]any)["role"] != "assistant" {
		t.Errorf("turns: %v", turns)
	}
	if system, _ := json.Marshal(body["system"]); !strings.Contains(string(system), "JSON") {
		t.Errorf("planner calls must demand JSON: %s", system)
	}
}

func TestAnthropicReportsRefusalAndTruncation(t *testing.T) {
	var body map[string]any
	var header http.Header
	anthropicStub(t, "refusal", &body, &header)
	if _, err := planLLMWith(context.Background(), "claude-opus-5-5", 2000, time.Minute, "s", []llmMsg{{Role: "user", Content: "x"}}); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Errorf("refusal: %v", err)
	}
	anthropicStub(t, "max_tokens", &body, &header)
	if _, err := planLLMWith(context.Background(), "claude-opus-5-5", 2000, time.Minute, "s", []llmMsg{{Role: "user", Content: "x"}}); err == nil || !strings.Contains(err.Error(), "output cap") {
		t.Errorf("truncation: %v", err)
	}
}

func chatStub(t *testing.T, got *map[string]any, path *string, auth *string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, got)
		*path, *auth = r.URL.Path, r.Header.Get("Authorization")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

func TestOpenAICompatibleRequest(t *testing.T) {
	var body map[string]any
	var path, auth string
	t.Setenv("OPENAI_BASE_URL", chatStub(t, &body, &path, &auth))
	t.Setenv("OPENAI_API_KEY", "")
	out, err := planLLMWith(context.Background(), "openai/meta/llama-3", 1000, time.Minute, "s", []llmMsg{{Role: "user", Content: "x"}})
	if err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if path != "/v1/chat/completions" || auth != "" {
		t.Errorf("path=%q auth=%q", path, auth)
	}
	if body["model"] != "meta/llama-3" || body["max_tokens"] != float64(1000) || body["response_format"] == nil {
		t.Errorf("body: %v", body)
	}
}

func TestAgentCallsAskForProseNotJSON(t *testing.T) {
	var body map[string]any
	var path, auth string
	t.Setenv("OPENAI_BASE_URL", chatStub(t, &body, &path, &auth))
	if _, err := Complete(context.Background(), "openai/m", "be brief", "hello", time.Minute); err != nil {
		t.Fatal(err)
	}
	if body["response_format"] != nil {
		t.Errorf("an agent step must not be forced into JSON: %v", body["response_format"])
	}
	system := body["messages"].([]any)[0].(map[string]any)["content"].(string)
	if system != "be brief" {
		t.Errorf("system = %q", system)
	}
}
