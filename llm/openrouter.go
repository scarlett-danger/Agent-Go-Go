package llm

import (
	"bytes"

	"context"

	"encoding/json"
	"errors"

	"fmt"

	"io"

	"log"

	"net/http"
	"strings"

	"time"
)

// OpenRouter is the single LLM client. It speaks OpenAI-compatible

// /v1/chat/completions and forwards request metadata to Langfuse via

// OpenRouter's native integration (configured in the OpenRouter dashboard).

type OpenRouter struct {
	apiKey string

	baseURL string

	http *http.Client
}

func NewOpenRouter(apiKey, baseURL string) *OpenRouter {

	return &OpenRouter{

		apiKey: apiKey,

		baseURL: baseURL,

		// 120s budget — Nemotron 120B can be slow on the free tier under load.

		// Outer Temporal activity timeout is the real bound.

		http: &http.Client{Timeout: 120 * time.Second},
	}

}

func (o *OpenRouter) Name() string { return "openrouter" }

// --- Wire types (OpenAI-compatible) ----------------------------------------

type oaiTool struct {
	Type string `json:"type"`

	Function oaiToolFunc `json:"function"`
}

type oaiToolFunc struct {
	Name string `json:"name"`

	Description string `json:"description,omitempty"`

	Parameters json.RawMessage `json:"parameters"`
}

// oaiToolCall: OpenAI quirk — Arguments is a JSON-encoded STRING, not an

// object. We translate at the boundary so the rest of the codebase uses

// proper RawMessage.

type oaiToolCall struct {
	ID string `json:"id"`

	Type string `json:"type"`

	Function struct {
		Name string `json:"name"`

		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiMessage struct {
	Role string `json:"role"`

	Content string `json:"content,omitempty"`

	ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`

	ToolCallID string `json:"tool_call_id,omitempty"`
}

type oaiRequest struct {
	Model string `json:"model"`

	Messages []oaiMessage `json:"messages"`

	Temperature float64 `json:"temperature,omitempty"`

	MaxTokens int `json:"max_tokens,omitempty"`

	Stream bool `json:"stream"`

	Tools []oaiTool `json:"tools,omitempty"`

	ToolChoice string `json:"tool_choice,omitempty"`

	// Metadata is OpenRouter's Langfuse passthrough field.

	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

type oaiResponse struct {
	ID string `json:"id"`

	Model string `json:"model"`

	Choices []struct {
		Message oaiMessage `json:"message"`

		FinishReason string `json:"finish_reason"`
	} `json:"choices"`

	Usage struct {
		PromptTokens int `json:"prompt_tokens"`

		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Complete runs one chat completion against OpenRouter.

//

// Tracing: when req.Metadata is non-empty, OpenRouter forwards it to your

// configured observability integration (Langfuse). Each call produces a

// Langfuse generation; calls sharing a session_id group into a session view.

// User-level analytics map onto user_id.

func (o *OpenRouter) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {

	// --- Translate our messages → OpenAI wire format -----------------------

	wire := make([]oaiMessage, 0, len(req.Messages)+1)

	if req.System != "" {

		wire = append(wire, oaiMessage{Role: RoleSystem, Content: req.System})

	}

	for _, m := range req.Messages {

		om := oaiMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}

		for _, tc := range m.ToolCalls {

			args := string(tc.Arguments)

			if args == "" {

				args = "{}"

			}

			om.ToolCalls = append(om.ToolCalls, oaiToolCall{

				ID: tc.ID,

				Type: "function",

				Function: struct {
					Name string `json:"name"`

					Arguments string `json:"arguments"`
				}{Name: tc.Name, Arguments: args},
			})

		}

		wire = append(wire, om)

	}

	// --- Translate tools ---------------------------------------------------

	var tools []oaiTool

	for _, t := range req.Tools {

		tools = append(tools, oaiTool{

			Type: "function",

			Function: oaiToolFunc{

				Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
			},
		})

	}

	temp := req.Temperature

	if temp == 0 {

		temp = 0.7

	}

	maxTok := req.MaxTokens

	if maxTok == 0 {

		maxTok = 1024

	}

	body, err := json.Marshal(oaiRequest{

		Model: req.Model,

		Messages: wire,

		Temperature: temp,

		MaxTokens: maxTok,

		Stream: false,

		Tools: tools,

		ToolChoice: func() string {

			if len(tools) > 0 {

				return "auto"

			}

			return ""

		}(),

		Metadata: req.Metadata,
	})

	if err != nil {

		return nil, fmt.Errorf("marshal openrouter request: %w", err)

	}

	var respBytes []byte

	// Free-tier providers can be bursty. Retry a few times on transient
	// failures so short spikes don't fail the whole request path.
	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,

			o.baseURL+"/chat/completions", bytes.NewReader(body))

		if err != nil {

			return nil, fmt.Errorf("build openrouter request: %w", err)

		}

		httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

		httpReq.Header.Set("Content-Type", "application/json")

		// X-Title is OpenRouter's optional ranking header; helps in their

		// public model-usage rankings if you opt in.

		httpReq.Header.Set("X-Title", "Agent-Go-Go")

		resp, reqErr := o.http.Do(httpReq)

		if reqErr != nil {

			if attempt < maxAttempts && isRetryableTransportError(reqErr) {

				backoff := time.Duration(attempt) * 2 * time.Second
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("execute openrouter request: %w", ctx.Err())
				case <-time.After(backoff):
					continue
				}
			}

			return nil, fmt.Errorf("execute openrouter request: %w", reqErr)

		}

		respBytes, err = io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {

			if attempt < maxAttempts && isRetryableTransportError(err) {

				backoff := time.Duration(attempt) * 2 * time.Second
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("read openrouter response: %w", ctx.Err())
				case <-time.After(backoff):
					continue
				}
			}

			return nil, fmt.Errorf("read openrouter response: %w", err)

		}

		if resp.StatusCode >= 400 {

			if attempt < maxAttempts && isRetryableStatus(resp.StatusCode, respBytes) {

				backoff := time.Duration(attempt) * 2 * time.Second
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("openrouter api error: status=%d body=%s", resp.StatusCode, string(respBytes))
				case <-time.After(backoff):
					continue
				}
			}

			return nil, fmt.Errorf("openrouter api error: status=%d body=%s",

				resp.StatusCode, string(respBytes))

		}

		break
	}

	var parsed oaiResponse

	if err := json.Unmarshal(respBytes, &parsed); err != nil {

		return nil, fmt.Errorf("unmarshal openrouter response: %w (body=%s)",

			err, string(respBytes))

	}

	if len(parsed.Choices) == 0 {

		return nil, fmt.Errorf("openrouter returned no choices")

	}

	choice := parsed.Choices[0]

	out := &CompletionResponse{

		Text: choice.Message.Content,

		StopReason: choice.FinishReason,

		PromptTokens: parsed.Usage.PromptTokens,

		CompletionTokens: parsed.Usage.CompletionTokens,

		Model: parsed.Model,
	}

	for _, tc := range choice.Message.ToolCalls {

		out.ToolCalls = append(out.ToolCalls, ToolCall{

			ID: tc.ID,

			Name: tc.Function.Name,

			Arguments: json.RawMessage(tc.Function.Arguments),
		})

	}

	// Lightweight log for local debugging — Langfuse has the full picture.

	log.Printf("[llm] model=%s prompt=%d completion=%d tools=%d session=%v",

		req.Model, out.PromptTokens, out.CompletionTokens, len(out.ToolCalls),

		req.Metadata["session_id"])

	return out, nil

}

func isRetryableTransportError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") || strings.Contains(msg, "temporarily unavailable")
}

func isRetryableStatus(code int, body []byte) bool {
	if code == http.StatusTooManyRequests || code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout {
		return true
	}

	if code == http.StatusNotFound {
		// Some providers briefly report no endpoints during routing updates.
		msg := strings.ToLower(string(body))
		if strings.Contains(msg, "temporarily") || strings.Contains(msg, "retry") {
			return true
		}
	}

	return false
}
