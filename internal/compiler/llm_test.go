package compiler

import (
	"context"
	"strings"
	"testing"
)

// The provider follows the model name, so the boot preflight must demand exactly
// the keys the configured tiers will actually touch — no more, no less.

func TestIsMoonshotModel(t *testing.T) {
	for model, want := range map[string]bool{
		"kimi-k3":          true,
		"kimi-k2.6":        true,
		"moonshot-v1-8k":   true,
		"gemini-2.5-flash": false,
		"gemini-2.5-pro":   false,
	} {
		if got := isMoonshotModel(model); got != want {
			t.Errorf("isMoonshotModel(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestRequirePlannerKeys_NamesMoonshotWhenReduceIsKimi(t *testing.T) {
	// Gemini is satisfied, but a Kimi reduce model has no key —
	// the error must name MOONSHOT_API_KEY, not "compilation failed".
	t.Setenv("GEMINI_API_KEY", "test-dummy")
	t.Setenv("MOONSHOT_API_KEY", "")
	t.Setenv("KIMI_API_KEY", "")
	t.Setenv("PLANNER_MODEL", "kimi-k3")
	if err := RequirePlannerKeys(); err == nil {
		t.Fatal("MOONSHOT_API_KEY unset with a Kimi reduce model: expected an error")
	} else if !strings.Contains(err.Error(), "MOONSHOT_API_KEY") {
		t.Fatalf("error must name MOONSHOT_API_KEY, got %v", err)
	}
}

func TestRequirePlannerKeys_GeminiOnlyTiersNeedNoMoonshot(t *testing.T) {
	// Point both tiers back at Gemini and the Moonshot key is not demanded.
	t.Setenv("GEMINI_API_KEY", "test-dummy")
	t.Setenv("MOONSHOT_API_KEY", "")
	t.Setenv("KIMI_API_KEY", "")
	t.Setenv("PLANNER_MODEL", "gemini-2.5-pro")
	t.Setenv("PLANNER_MAP_MODEL", "gemini-2.5-flash")
	if err := RequirePlannerKeys(); err != nil {
		t.Fatalf("all-Gemini tiers should need only GEMINI_API_KEY, got %v", err)
	}
}

func TestGeminiThinkingBudget(t *testing.T) {
	// Selection calls must not let thinking eat the answer's token budget —
	// that is what made the 1024-token router call fail on gemini-2.5-flash.
	for _, tc := range []struct {
		model     string
		maxTokens int
		want      int
	}{
		{"gemini-2.5-flash", 1024, 0},
		{"gemini-2.5-flash", 512, 0},
		{"gemini-2.5-pro", 1024, 128},
		{"gemini-2.5-flash", 16000, 8192},
		{"gemini-2.5-pro", 16000, 8192},
	} {
		if got := geminiThinkingBudget(tc.model, tc.maxTokens); got != tc.want {
			t.Errorf("geminiThinkingBudget(%q, %d) = %d, want %d", tc.model, tc.maxTokens, got, tc.want)
		}
	}
}

func TestRequirePlannerKeys_AllOllamaNeedsNoKeys(t *testing.T) {
	// Fully local: no Gemini, Moonshot or OpenAI key is demanded.
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("MOONSHOT_API_KEY", "")
	t.Setenv("KIMI_API_KEY", "")
	t.Setenv("PLANNER_MAP_MODEL", "ollama/qwen3:8b")
	t.Setenv("PLANNER_MODEL", "ollama/qwen3:8b")
	t.Setenv("PLANNER_EMBED_MODEL", "ollama/nomic-embed-text")
	if err := RequirePlannerKeys(); err != nil {
		t.Fatalf("all-Ollama tiers should need no keys, got %v", err)
	}
}

func TestLLMProvider(t *testing.T) {
	for model, want := range map[string]string{
		"ollama/qwen3:8b":       "ollama",
		"kimi-k3":               "moonshot",
		"gemini-3.5-flash":      "gemini",
		"gemini-3.1-flash-lite": "gemini",
	} {
		if got := llmProvider(model); got != want {
			t.Errorf("llmProvider(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestLLMProvider_NewProviders(t *testing.T) {
	for model, want := range map[string]string{
		"claude-opus-5-5":         "anthropic",
		"claude-haiku-5-5":        "anthropic",
		"gpt-5":                   "openai",
		"o3-mini":                 "openai",
		"openai/llama-3.3-70b":    "openai",
		"openai/anthropic/claude": "openai",
		"gemini-3.5-flash":        "gemini",
		"ollama/qwen3:8b":         "ollama",
		"kimi-k3":                 "moonshot",
	} {
		if got := llmProvider(model); got != want {
			t.Errorf("llmProvider(%q) = %q, want %q", model, got, want)
		}
	}
}

func clearModelEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GEMINI_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL",
		"MOONSHOT_API_KEY", "KIMI_API_KEY", "PLANNER_MODEL", "PLANNER_MAP_MODEL", "APEX_AGENT_MODEL"} {
		t.Setenv(k, "")
	}
}

func TestDefaultsFollowWhicheverKeyIsSet(t *testing.T) {
	clearModelEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "k")
	if st := Status(); !st.Ready || st.Map != "claude-haiku-5-5" || st.Reduce != "claude-opus-5-5" {
		t.Fatalf("anthropic defaults: %+v", st)
	}
	clearModelEnv(t)
	t.Setenv("OPENAI_API_KEY", "k")
	if st := Status(); !st.Ready || st.Reduce != "gpt-5" {
		t.Fatalf("openai defaults: %+v", st)
	}
}

func TestNoModelIsReportedNotFatal(t *testing.T) {
	clearModelEnv(t)
	st := Status()
	if st.Ready || !strings.Contains(st.Problem, "./apex model") {
		t.Fatalf("status with no model: %+v", st)
	}
}

func TestOpenAICompatibleNeedsNoKeyWithCustomEndpoint(t *testing.T) {
	clearModelEnv(t)
	t.Setenv("PLANNER_MODEL", "openai/local")
	t.Setenv("PLANNER_MAP_MODEL", "openai/local")
	if err := RequirePlannerKeys(); err == nil {
		t.Fatal("no key and no endpoint: expected an error")
	}
	t.Setenv("OPENAI_BASE_URL", "http://localhost:1234/v1")
	if err := RequirePlannerKeys(); err != nil {
		t.Fatalf("custom endpoint should need no key: %v", err)
	}
}

func TestAnthropicParamsCarryThinkingRoomEffortAndFallback(t *testing.T) {
	p := anthropicParams("claude-opus-5-5", 16000, "sys", []llmMsg{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}})
	if p.MaxTokens != 16000+anthropicThinkingRoom || p.OutputConfig.Effort != "high" || len(p.Betas) != 1 {
		t.Fatalf("opus params: %+v", p)
	}
	if p := anthropicParams("claude-haiku-5-5", 1024, "sys", nil); p.OutputConfig.Effort != "low" || len(p.Betas) != 0 {
		t.Fatalf("haiku params: %+v", p)
	}
	if p := anthropicParams("claude-haiku-4-5", 1024, "sys", nil); p.OutputConfig.Effort != "" {
		t.Fatalf("older model must not send effort: %+v", p)
	}
}

func TestPlainTextCallsDoNotDemandJSON(t *testing.T) {
	ctx := context.WithValue(context.Background(), plainTextKey{}, true)
	if got := systemFor(ctx, "sys"); got != "sys" {
		t.Errorf("plain text system = %q", got)
	}
	if got := systemFor(context.Background(), "sys"); !strings.Contains(got, "JSON") {
		t.Errorf("json system = %q", got)
	}
}
