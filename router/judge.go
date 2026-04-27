package router

import (
	"context"

	"encoding/json"

	"fmt"

	"strings"

	"github.com/scarlett-danger/Agent-Go-Go/llm"
)

// Judge wraps the small classifier LLM. It takes the user's message,

// asks the model to pick a category, parses the JSON response, and

// returns a Route. If the judge fails or returns garbage, callers

// should fall back to the default category.

type Judge struct {
	provider llm.Provider

	model string

	cfg *Config
}

// NewJudge wires a Provider + model + routing Config together.

// Provider should typically be a small fast model (e.g., llama-3.1-8b-instruct

// on NIM). Don't use the same big model that handles the actual reply —

// that defeats the cost/latency point of the router.

func NewJudge(provider llm.Provider, model string, cfg *Config) *Judge {

	return &Judge{provider: provider, model: model, cfg: cfg}

}

// judgeOutput is the JSON shape we ask the model to produce.

type judgeOutput struct {
	Category string `json:"category"`

	Reason string `json:"reason"`
}

// Classify runs one judge inference and returns a Route. On any failure

// (HTTP error, JSON parse error, unknown category) it returns the default

// route with the failure reason — never a hard error.

//

// `metadata` is forwarded to OpenRouter / Langfuse so judge calls show up

// in the trace tree alongside the expert call. Use trace_name="judge" so

// judge generations are filterable separately from expert generations.

func (j *Judge) Classify(ctx context.Context, userMessage string, metadata map[string]interface{}) (Route, error) {

	system := buildJudgeSystemPrompt(j.cfg)

	// Tag this call as the judge so Langfuse can filter it out of cost

	// and latency dashboards if desired.

	md := map[string]interface{}{}

	for k, v := range metadata {

		md[k] = v

	}

	md["trace_name"] = "judge"

	resp, err := j.provider.Complete(ctx, llm.CompletionRequest{

		System: system,

		Messages: []llm.ChatMessage{

			{Role: llm.RoleUser, Content: userMessage},
		},

		Model: j.model,

		Temperature: 0.1, // low temp for deterministic-ish classification

		MaxTokens: 150,

		Metadata: md,
	})

	if err != nil {

		return j.cfg.Resolve(j.cfg.Default, fmt.Sprintf("judge call failed: %v", err)), err

	}

	out, parseErr := parseJudgeJSON(resp.Text)

	if parseErr != nil {

		// Degrade to default — don't fail the workflow over bad JSON.

		return j.cfg.Resolve(j.cfg.Default, fmt.Sprintf("judge parse failed: %v", parseErr)), nil

	}

	if _, ok := j.cfg.Categories[out.Category]; !ok {

		return j.cfg.Resolve(j.cfg.Default, fmt.Sprintf("judge picked unknown category %q", out.Category)), nil

	}

	return j.cfg.Resolve(out.Category, out.Reason), nil

}

// parseJudgeJSON tolerates a few common LLM quirks: code-fenced JSON,

// leading/trailing prose, surrounding whitespace.

func parseJudgeJSON(raw string) (*judgeOutput, error) {

	s := strings.TrimSpace(raw)

	// Strip code fences if the model wrapped its output.

	s = strings.TrimPrefix(s, "```json")

	s = strings.TrimPrefix(s, "```")

	s = strings.TrimSuffix(s, "```")

	s = strings.TrimSpace(s)

	// Find the first '{' and last '}' to extract JSON if there's surrounding text.

	start := strings.Index(s, "{")

	end := strings.LastIndex(s, "}")

	if start < 0 || end < 0 || end <= start {

		return nil, fmt.Errorf("no JSON object found in: %q", raw)

	}

	s = s[start : end+1]

	var out judgeOutput

	if err := json.Unmarshal([]byte(s), &out); err != nil {

		return nil, fmt.Errorf("unmarshal: %w", err)

	}

	return &out, nil

}

// buildJudgeSystemPrompt assembles the classification instructions. The

// category list is pulled live from the config, so adding a category to

// routing.yaml updates the judge's prompt automatically.

func buildJudgeSystemPrompt(cfg *Config) string {

	return `You are a routing classifier. Read the user's message and pick the single best category.

Available categories:

` + cfg.CategorySummary() + `

Respond with ONLY a JSON object — no prose, no code fences:

{"category": "<one of the names above>", "reason": "<short justification, max 15 words>"}

If unsure, pick "` + cfg.Default + `". Never invent categories.`

}
