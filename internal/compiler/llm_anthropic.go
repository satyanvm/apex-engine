package compiler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/shared/constant"
)

func anthropicKey() string { return strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) }

// RequireAnthropicKey is the preflight for claude-* models.
func RequireAnthropicKey() error {
	if anthropicKey() == "" {
		return errors.New("ANTHROPIC_API_KEY is required for this model. Set it in .env")
	}
	return nil
}

// anthropicThinkingRoom is added to max_tokens, since thinking draws from it.
const anthropicThinkingRoom = 8192

// anthropicEffort sizes effort to the call: selection over a list is cheap work,
// graph authorship is the hard part. Older models reject the parameter.
func anthropicEffort(model string, maxTokens int) anthropic.BetaOutputConfigEffort {
	if strings.Contains(model, "-4-5") || strings.Contains(model, "-3-") {
		return ""
	}
	if maxTokens < 4096 {
		return anthropic.BetaOutputConfigEffortLow
	}
	return anthropic.BetaOutputConfigEffortHigh
}

// anthropicHasFallback names the models whose safety classifiers can decline a
// request, and which accept the server-side "default" fallback routing.
func anthropicHasFallback(model string) bool {
	switch model {
	case "claude-fable-5-1", "claude-opus-5-5", "claude-opus-5", "claude-sonnet-5-5":
		return true
	}
	return false
}

func anthropicParams(model string, maxTokens int, system string, msgs []llmMsg) anthropic.BetaMessageNewParams {
	turns := make([]anthropic.BetaMessageParam, 0, len(msgs))
	for _, m := range msgs {
		block := anthropic.NewBetaTextBlock(m.Content)
		if m.Role == "assistant" {
			turns = append(turns, anthropic.BetaMessageParam{OfAssistant: &anthropic.BetaAssistantMessageParam{Content: []anthropic.BetaContentBlockParamUnion{block}}})
		} else {
			turns = append(turns, anthropic.NewBetaUserMessage(block))
		}
	}
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: int64(maxTokens + anthropicThinkingRoom),
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Messages:  turns,
	}
	if effort := anthropicEffort(model, maxTokens); effort != "" {
		params.OutputConfig = anthropic.BetaOutputConfigParam{Effort: effort}
	}
	if anthropicHasFallback(model) {
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}
	return params
}

// planLLMAnthropic streams, because a large graph at high effort can outlast a
// plain HTTP request, and accumulates the events into the final message.
func planLLMAnthropic(ctx context.Context, model string, maxTokens int, timeout time.Duration, system string, msgs []llmMsg) (string, error) {
	if err := RequireAnthropicKey(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := anthropic.NewClient(option.WithAPIKey(anthropicKey()))
	stream := client.Beta.Messages.NewStreaming(ctx, anthropicParams(model, maxTokens, systemFor(ctx, system), msgs))
	defer stream.Close()
	var message anthropic.BetaMessage
	for stream.Next() {
		if err := message.Accumulate(stream.Current()); err != nil {
			return "", fmt.Errorf("%s: reading response: %w", model, err)
		}
	}
	if err := stream.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", fmt.Errorf("%s: timed out: %w", model, errTimedOut)
		}
		return "", fmt.Errorf("%s: %w", model, err)
	}
	switch message.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return "", fmt.Errorf("%s declined this request", model)
	case anthropic.BetaStopReasonMaxTokens:
		return "", fmt.Errorf("%s hit the %d-token output cap before finishing its JSON: %w", model, maxTokens, errOutputCapped)
	}
	var b strings.Builder
	for _, block := range message.Content {
		if text, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			b.WriteString(text.Text)
		}
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", fmt.Errorf("%s returned no text (stop reason %q)", model, message.StopReason)
	}
	return b.String(), nil
}
