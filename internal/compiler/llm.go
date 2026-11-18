package compiler

// llm.go is the control-plane model client used by the prompt compiler. The
// provider follows the model name, so either tier can be re-pointed with
// PLANNER_MAP_MODEL / PLANNER_MODEL and the matching key, nothing else:
//
//   claude-*                  Anthropic (ANTHROPIC_API_KEY), see llm_anthropic.go
//   gpt-*, o1/o3/o4-*         OpenAI (OPENAI_API_KEY)
//   openai/<model>            any OpenAI-compatible endpoint (OPENAI_BASE_URL)
//   kimi-*, moonshot-*        Moonshot (MOONSHOT_API_KEY)
//   ollama/<model>            a local Ollama server, no key
//   anything else             Gemini (GEMINI_API_KEY)
//
// With no explicit model, the tiers default to whichever provider has a key
// (defaultModels). A prompt the model could not compile is a failure, reported
// with its reason: every error returned from here is expected to reach the user
// verbatim.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// llmMsg is one conversation turn for multi-message calls (the reduce phase's
// validate→repair loop replays the model's own graph plus the validator errors).
type llmMsg struct {
	Role    string // "user" | "assistant"
	Content string
}

const jsonOnly = "\nRespond with a single JSON object and nothing else."

// Compilation model tiers. Map wants cheap+fast over a large index; reduce wants
// the strongest available, because graph authorship is the hard part. Both
// env-overridable.
func mapModel() string {
	m, _ := defaultModels()
	return envOr("PLANNER_MAP_MODEL", m)
}

func reduceModel() string {
	_, r := defaultModels()
	return envOr("PLANNER_MODEL", r)
}

// defaultModels picks each tier's model from whichever provider has a key. Gemini
// stays the default when none does, so the missing-key error has a concrete fix.
func defaultModels() (mapName, reduce string) {
	switch {
	case strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) != "":
		return "gemini-3.1-flash-lite", "gemini-3.5-flash"
	case anthropicKey() != "":
		return "claude-haiku-5-5", "claude-opus-5-5"
	case strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "":
		return "gpt-5-mini", "gpt-5"
	case moonshotKey() != "":
		return "kimi-k2.6", "kimi-k3"
	}
	return "gemini-3.1-flash-lite", "gemini-3.5-flash"
}

// errNoModel is what a compile reports on an install with no model configured.
var errNoModel = errors.New("No model is connected, so prompts cannot be compiled. " +
	"Run ./apex model, or set one of GEMINI_API_KEY, ANTHROPIC_API_KEY or OPENAI_API_KEY " +
	"(or PLANNER_MODEL=ollama/<model> for a local model) in .env. Saved workflows without AI steps still run.")

// plainTextKey marks a call whose answer is prose rather than a JSON object.
type plainTextKey struct{}

func wantsJSON(ctx context.Context) bool { return ctx.Value(plainTextKey{}) == nil }

// systemFor adds the JSON-only demand to calls that parse their answer.
func systemFor(ctx context.Context, system string) string {
	if wantsJSON(ctx) {
		return system + jsonOnly
	}
	return system
}

// isMoonshotModel routes a model id to its provider: Kimi models run on the
// Moonshot API (OpenAI-compatible), everything else on Gemini.
func isMoonshotModel(model string) bool {
	l := strings.ToLower(model)
	return strings.HasPrefix(l, "kimi-") || strings.HasPrefix(l, "moonshot-")
}

func moonshotKey() string {
	if k := strings.TrimSpace(os.Getenv("MOONSHOT_API_KEY")); k != "" {
		return k
	}
	return strings.TrimSpace(os.Getenv("KIMI_API_KEY"))
}

func moonshotBaseURL() string { return envOr("MOONSHOT_BASE_URL", "https://api.moonshot.ai/v1") }

// RequireGeminiKey is the boot preflight and the guard tests use to tell a
// configuration error apart from a provider failure.
func RequireGeminiKey() error {
	if strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) == "" {
		return errors.New("GEMINI_API_KEY is required — the planner's route and map " +
			"phases run on Gemini. Set it in .env")
	}
	return nil
}

// RequireMoonshotKey is the same guard for the reduce phase's Kimi model.
func RequireMoonshotKey() error {
	if moonshotKey() == "" {
		return errors.New("MOONSHOT_API_KEY is required — the planner's reduce phase " +
			"authors every graph with Kimi (PLANNER_MODEL, default kimi-k3). Set it in .env")
	}
	return nil
}

// RequirePlannerKeys is the boot preflight for every credential the compiler can
// touch: Gemini always (route, map, embeddings), Moonshot when either tier is
// pointed at a Kimi model.
func RequirePlannerKeys() error {
	if os.Getenv("PLANNER_MODEL") == "" && os.Getenv("PLANNER_MAP_MODEL") == "" && !anyProviderKey() {
		return errNoModel
	}
	for _, model := range []string{mapModel(), reduceModel()} {
		var err error
		switch llmProvider(model) {
		case "gemini":
			err = RequireGeminiKey()
		case "moonshot":
			err = RequireMoonshotKey()
		case "anthropic":
			err = RequireAnthropicKey()
		case "openai":
			err = RequireOpenAIKey()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func anyProviderKey() bool {
	return strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) != "" || anthropicKey() != "" ||
		strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" || moonshotKey() != ""
}

// RequireOpenAIKey demands a key unless a custom endpoint is configured, since
// local OpenAI-compatible servers usually take none.
func RequireOpenAIKey() error {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" && os.Getenv("OPENAI_BASE_URL") == "" {
		return errors.New("OPENAI_API_KEY is required for this model. Set it in .env, " +
			"or set OPENAI_BASE_URL for an OpenAI-compatible server that takes no key")
	}
	return nil
}

// ModelStatus says whether compiles can run and on what.
type ModelStatus struct {
	Ready   bool   `json:"ready"`
	Problem string `json:"problem,omitempty"`
	Map     string `json:"map"`
	Reduce  string `json:"reduce"`
	Agent   string `json:"agent"`
}

// Status reports the model configuration without calling a provider.
func Status() ModelStatus {
	st := ModelStatus{Ready: true, Map: mapModel(), Reduce: reduceModel(), Agent: AgentModel()}
	if err := RequirePlannerKeys(); err != nil {
		st.Ready, st.Problem = false, err.Error()
	}
	return st
}

// llmProvider routes a model id to its provider: ollama/<model> runs on a local
// Ollama server (no key, no quota), kimi-*/moonshot-* on Moonshot, everything
// else on Gemini.
func llmProvider(model string) string {
	l := strings.ToLower(model)
	switch {
	case strings.HasPrefix(model, "ollama/"):
		return "ollama"
	case strings.HasPrefix(model, "openai/"), strings.HasPrefix(l, "gpt-"),
		strings.HasPrefix(l, "o1-"), strings.HasPrefix(l, "o3-"), strings.HasPrefix(l, "o4-"):
		return "openai"
	case strings.HasPrefix(l, "claude-"):
		return "anthropic"
	case isMoonshotModel(model):
		return "moonshot"
	}
	return "gemini"
}

// planLLMWith sends a JSON-only completion using the requested model. The model
// argument is intentionally honoured: map uses the small model, reduce the strong
// one, and the provider follows the model name (isMoonshotModel). (Its
// predecessor accepted a model and then silently ignored it, running whatever
// provider the environment happened to hold — which defeated the whole point of
// having tiers.)
func planLLMWith(ctx context.Context, model string, maxTokens int, timeout time.Duration, system string, msgs []llmMsg) (string, error) {
	switch llmProvider(model) {
	case "ollama":
		return planLLMOllama(ctx, model, maxTokens, timeout, system, msgs)
	case "moonshot":
		if err := RequireMoonshotKey(); err != nil {
			return "", err
		}
		return planLLMChatCompletions(ctx, moonshotBaseURL(), moonshotKey(), "moonshot", model, model, "max_tokens", maxTokens, timeout, system, msgs)
	case "openai":
		if err := RequireOpenAIKey(); err != nil {
			return "", err
		}
		base := envOr("OPENAI_BASE_URL", openAIBaseURL)
		tokensField := "max_tokens"
		if base == openAIBaseURL { // OpenAI's own reasoning models reject max_tokens
			tokensField = "max_completion_tokens"
		}
		return planLLMChatCompletions(ctx, base, strings.TrimSpace(os.Getenv("OPENAI_API_KEY")), "openai", model, strings.TrimPrefix(model, "openai/"), tokensField, maxTokens, timeout, system, msgs)
	case "anthropic":
		return planLLMAnthropic(ctx, model, maxTokens, timeout, system, msgs)
	}
	return planLLMGemini(ctx, model, maxTokens, timeout, system, msgs)
}

// ollamaNumCtx is the context window requested per call. Ollama's default is a
// few thousand tokens and it truncates longer prompts SILENTLY — the router's
// cluster directory alone is ~9.5k tokens — so it is always set explicitly.
// 16k covers the largest prompt (router; reduce is ~6.5k plus its output) while
// keeping an 8B model's KV cache small enough for a 16 GB laptop.
func ollamaNumCtx() int {
	if n, err := strconv.Atoi(os.Getenv("OLLAMA_NUM_CTX")); err == nil && n > 0 {
		return n
	}
	return 16384
}

// planLLMOllama is the local path: Ollama's native /api/chat, which (unlike its
// OpenAI-compatible endpoint) takes num_ctx and a think switch. format=json
// constrains decoding to valid JSON. Thinking is off: on a laptop-sized model it
// multiplies latency for little gain on selection tasks.
//
// Local inference has no rate limit, only compute — so the per-phase timeouts,
// sized for a remote API, are stretched: map calls queue behind each other on
// one GPU and a large reduce can take minutes.
func planLLMOllama(ctx context.Context, model string, maxTokens int, timeout time.Duration, system string, msgs []llmMsg) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, max(timeout, 10*time.Minute))
	defer cancel()

	chat := make([]map[string]string, 0, len(msgs)+1)
	chat = append(chat, map[string]string{"role": "system", "content": systemFor(ctx, system)})
	for _, m := range msgs {
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		chat = append(chat, map[string]string{"role": role, "content": m.Content})
	}
	payload := map[string]any{
		"model":    strings.TrimPrefix(model, "ollama/"),
		"messages": chat,
		"stream":   false,
		"think":    false,
		"options": map[string]any{
			"num_ctx":     ollamaNumCtx(),
			"num_predict": maxTokens,
		},
	}
	if wantsJSON(ctx) {
		payload["format"] = "json"
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ollamaHost()+"/api/chat", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%s: timed out: %w", model, errTimedOut)
		}
		return "", fmt.Errorf("%s (is `ollama serve` running at %s?): %w", model, ollamaHost(), err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: ollama %d: %s", model, resp.StatusCode, truncateStr(string(raw), 300))
	}
	var parsed struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		DoneReason string `json:"done_reason"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("%s: unparseable response: %w", model, err)
	}
	if parsed.DoneReason == "length" {
		return "", fmt.Errorf("%s hit the %d-token output cap before finishing its JSON: %w", model, maxTokens, errOutputCapped)
	}
	if strings.TrimSpace(parsed.Message.Content) == "" {
		return "", fmt.Errorf("%s returned no text (done reason %q)", model, parsed.DoneReason)
	}
	return parsed.Message.Content, nil
}

// planLLMGemini is the Gemini generateContent path. Gemini carries the system
// prompt in system_instruction, maps the assistant role to "model", and pins JSON
// output with responseMimeType — the same contract the OpenAI-era response_format
// provided.
func planLLMGemini(ctx context.Context, model string, maxTokens int, timeout time.Duration, system string, msgs []llmMsg) (string, error) {
	if err := RequireGeminiKey(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	budget := geminiThinkingBudget(model, maxTokens)
	contents := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]any{
			"role":  role,
			"parts": []map[string]string{{"text": m.Content}},
		})
	}
	// Thinking tokens draw from maxOutputTokens, so the answer's budget is added
	// on top of the thinking allowance rather than shared with it. Only the 2.5
	// family takes an explicit token budget (3.x rejects budget 0 and Gemma
	// rejects thinkingConfig entirely); other models think as they see fit, so
	// they get a fixed ceiling of headroom instead.
	thinking := 8192
	is25 := strings.HasPrefix(model, "gemini-2.5-")
	if is25 {
		thinking = budget
	}
	genCfg := map[string]any{"maxOutputTokens": maxTokens + thinking}
	if wantsJSON(ctx) {
		genCfg["responseMimeType"] = "application/json"
	}
	if is25 {
		genCfg["thinkingConfig"] = map[string]any{"thinkingBudget": budget}
	}
	body, _ := json.Marshal(map[string]any{
		"system_instruction": map[string]any{
			"parts": []map[string]string{{"text": systemFor(ctx, system)}},
		},
		"contents":         contents,
		"generationConfig": genCfg,
	})
	raw, err := geminiPost(ctx, model, body)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("%s: unparseable response: %w", model, err)
	}
	if len(parsed.Candidates) == 0 {
		return "", fmt.Errorf("%s: gemini returned no candidates: %s", model, truncateStr(string(raw), 300))
	}
	cand := parsed.Candidates[0]
	if cand.FinishReason == "MAX_TOKENS" {
		// The JSON is truncated. Say so plainly rather than letting the caller
		// report a confusing parse error several frames down.
		return "", fmt.Errorf("%s hit the %d-token output cap before finishing its JSON: %w", model, maxTokens, errOutputCapped)
	}
	var b strings.Builder
	for _, p := range cand.Content.Parts {
		b.WriteString(p.Text)
	}
	out := b.String()
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("%s returned no text (finish reason %q)", model, cand.FinishReason)
	}
	return out, nil
}

// geminiPost sends one generateContent request, riding out per-minute rate
// limits: a 429 is retried after the delay Gemini suggests, for as long as the
// caller's deadline allows. Free-tier keys allow as few as 5 requests/minute per
// model, and the map phase alone fans out a dozen calls — without this a single
// prompt trips the limit. A limit that outlasts the deadline is still an error.
func geminiPost(ctx context.Context, model string, body []byte) ([]byte, error) {
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent", model)
	transient := 0
	for attempt := 0; ; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-goog-api-key", os.Getenv("GEMINI_API_KEY"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				return nil, fmt.Errorf("%s: timed out: %w", model, errTimedOut)
			}
			// A dropped connection (reset, EOF, TLS hiccup) on a long call is
			// transient: retry a few times before giving up.
			if transient < 3 && sleepCtx(ctx, time.Duration(transient+1)*2*time.Second) {
				transient++
				continue
			}
			return nil, fmt.Errorf("%s: %w", model, err)
		}
		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && readErr == nil {
			return raw, nil
		}
		if readErr != nil || resp.StatusCode == http.StatusInternalServerError ||
			resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusGatewayTimeout {
			if transient < 3 && sleepCtx(ctx, time.Duration(transient+1)*2*time.Second) {
				transient++
				continue
			}
			if readErr != nil {
				return nil, fmt.Errorf("%s: reading response: %w", model, readErr)
			}
		}
		if resp.StatusCode == http.StatusTooManyRequests && geminiDailyQuota(raw) {
			// A per-day cap does not lift in seconds: waiting out the
			// suggested delay only burns the caller's deadline.
			return nil, fmt.Errorf("%s: gemini daily request quota used up for this API key (resets at midnight Pacific; raise it by enabling billing, or set PLANNER_FALLBACK_MODEL): %w", model, errQuotaExhausted)
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			wait := geminiRetryDelay(raw, attempt)
			if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > wait {
				select {
				case <-time.After(wait):
					continue
				case <-ctx.Done():
				}
			}
		}
		return nil, fmt.Errorf("%s: gemini %d: %s", model, resp.StatusCode, truncateStr(string(raw), 300))
	}
}

// sleepCtx waits d unless ctx ends first or would end before d elapses;
// it reports whether the full wait happened.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= d {
		return false
	}
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// errQuotaExhausted marks a provider quota that will not reset within a call.
var errQuotaExhausted = errors.New("quota exhausted")

// geminiDailyQuota reports whether a 429 body names a per-day quota.
func geminiDailyQuota(raw []byte) bool {
	var e struct {
		Error struct {
			Details []struct {
				Violations []struct {
					QuotaID string `json:"quotaId"`
				} `json:"violations"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return false
	}
	for _, d := range e.Error.Details {
		for _, v := range d.Violations {
			if strings.Contains(v.QuotaID, "PerDay") {
				return true
			}
		}
	}
	return false
}

// geminiRetryDelay reads the RetryInfo delay ("17s") Gemini attaches to a 429,
// falling back to exponential backoff when it is absent.
func geminiRetryDelay(raw []byte, attempt int) time.Duration {
	var e struct {
		Error struct {
			Details []struct {
				RetryDelay string `json:"retryDelay"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		for _, d := range e.Error.Details {
			if dur, err := time.ParseDuration(d.RetryDelay); err == nil && d.RetryDelay != "" {
				return max(dur, time.Second) + 500*time.Millisecond
			}
		}
	}
	return min(time.Second<<attempt, 20*time.Second)
}

// geminiThinkingBudget sizes Gemini 2.5's thinking separately from the answer.
// Route and map (small answers) are classification over a list: thinking adds
// latency and nothing else, so it is off — except on pro, which cannot disable
// it and takes its 128-token minimum. Reduce (graph authorship, big answers) is
// the hard part and gets real room to think.
func geminiThinkingBudget(model string, maxTokens int) int {
	if maxTokens < 4096 {
		if strings.Contains(model, "-pro") {
			return 128
		}
		return 0
	}
	return 8192
}

const openAIBaseURL = "https://api.openai.com/v1"

// planLLMChatCompletions is the path for every OpenAI-compatible API (Moonshot,
// OpenAI, and anything behind OPENAI_BASE_URL). The system message carries the
// instruction plus the JSON-only demand, response_format pins object output, and
// a truncated answer is reported as the token cap it is, not a parse error
// several frames down. label names the model in errors; apiModel is what is sent.
func planLLMChatCompletions(ctx context.Context, baseURL, key, provider, label, apiModel, tokensField string, maxTokens int, timeout time.Duration, system string, msgs []llmMsg) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	chat := make([]map[string]string, 0, len(msgs)+1)
	chat = append(chat, map[string]string{"role": "system", "content": systemFor(ctx, system)})
	for _, m := range msgs {
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		chat = append(chat, map[string]string{"role": role, "content": m.Content})
	}
	payload := map[string]any{"model": apiModel, "messages": chat, tokensField: maxTokens}
	if wantsJSON(ctx) {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	if key != "" {
		req.Header.Set("authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", fmt.Errorf("%s: timed out: %w", label, errTimedOut)
		}
		return "", fmt.Errorf("%s: %w", label, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s %d: %s", label, provider, resp.StatusCode, truncateStr(string(raw), 300))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("%s: unparseable response: %w", label, err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("%s: %s returned no choices: %s", label, provider, truncateStr(string(raw), 300))
	}
	choice := parsed.Choices[0]
	if choice.FinishReason == "length" {
		return "", fmt.Errorf("%s hit the %d-token output cap before finishing its JSON: %w", label, maxTokens, errOutputCapped)
	}
	out := choice.Message.Content
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("%s returned no text (finish reason %q)", label, choice.FinishReason)
	}
	return out, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
