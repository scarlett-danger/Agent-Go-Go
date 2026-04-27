package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// successResponse builds a minimal valid OpenAI-compatible response body.
func successResponse(t *testing.T, text string) []byte {
	t.Helper()
	resp := oaiResponse{
		Model: "test-model",
		Choices: []struct {
			Message      oaiMessage `json:"message"`
			FinishReason string     `json:"finish_reason"`
		}{
			{
				Message:      oaiMessage{Role: RoleAssistant, Content: text},
				FinishReason: "stop",
			},
		},
		Usage: struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		}{PromptTokens: 10, CompletionTokens: 5},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal success response: %v", err)
	}
	return b
}

func TestName(t *testing.T) {
	client := NewOpenRouter("key", "http://localhost")
	if client.Name() != "openrouter" {
		t.Errorf("Name() = %q, want %q", client.Name(), "openrouter")
	}
}

func TestIsRetryableStatus(t *testing.T) {
	tests := []struct {
		code int
		body []byte
		want bool
	}{
		{http.StatusTooManyRequests, nil, true},
		{http.StatusBadGateway, nil, true},
		{http.StatusServiceUnavailable, nil, true},
		{http.StatusGatewayTimeout, nil, true},
		{http.StatusNotFound, []byte("temporarily unavailable, please retry"), true},
		{http.StatusNotFound, []byte("retry later"), true},
		{http.StatusNotFound, []byte("endpoint not found"), false},
		{http.StatusBadRequest, nil, false},
		{http.StatusUnauthorized, nil, false},
		{http.StatusInternalServerError, nil, false},
		{http.StatusOK, nil, false},
	}

	for _, tc := range tests {
		got := isRetryableStatus(tc.code, tc.body)
		if got != tc.want {
			t.Errorf("isRetryableStatus(%d, %q) = %v, want %v", tc.code, tc.body, got, tc.want)
		}
	}
}

func TestIsRetryableTransportError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"timeout in message", fmt.Errorf("connection timeout after 30s"), true},
		{"temporarily unavailable", fmt.Errorf("service temporarily unavailable"), true},
		{"generic error", fmt.Errorf("connection refused"), false},
		{"other context error", context.Canceled, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isRetryableTransportError(tc.err)
			if got != tc.want {
				t.Errorf("isRetryableTransportError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestComplete(t *testing.T) {
	t.Run("successful completion returns text and metadata", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				t.Error("Authorization header missing")
			}
			w.Write(successResponse(t, "Hello from the model!"))
		}))
		defer srv.Close()

		client := NewOpenRouter("test-key", srv.URL)
		resp, err := client.Complete(context.Background(), CompletionRequest{
			Model:    "test-model",
			Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Text != "Hello from the model!" {
			t.Errorf("text: got %q, want %q", resp.Text, "Hello from the model!")
		}
		if resp.StopReason != "stop" {
			t.Errorf("stop_reason: got %q, want %q", resp.StopReason, "stop")
		}
		if resp.PromptTokens != 10 {
			t.Errorf("prompt_tokens: got %d, want 10", resp.PromptTokens)
		}
	})

	t.Run("system message is prepended before user messages", func(t *testing.T) {
		var captured oaiRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewDecoder(r.Body).Decode(&captured)
			w.Write(successResponse(t, "ok"))
		}))
		defer srv.Close()

		client := NewOpenRouter("key", srv.URL)
		client.Complete(context.Background(), CompletionRequest{
			System:   "you are a helpful assistant",
			Model:    "m",
			Messages: []ChatMessage{{Role: RoleUser, Content: "question"}},
		})

		if len(captured.Messages) < 2 {
			t.Fatalf("expected at least 2 wire messages (system + user), got %d", len(captured.Messages))
		}
		if captured.Messages[0].Role != RoleSystem {
			t.Errorf("first wire message should be system, got %q", captured.Messages[0].Role)
		}
		if captured.Messages[0].Content != "you are a helpful assistant" {
			t.Errorf("system content: got %q", captured.Messages[0].Content)
		}
	})

	t.Run("tool calls are translated from OAI wire format", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resp := oaiResponse{
				Model: "test-model",
				Choices: []struct {
					Message      oaiMessage `json:"message"`
					FinishReason string     `json:"finish_reason"`
				}{{
					Message: oaiMessage{
						Role: RoleAssistant,
						ToolCalls: []oaiToolCall{{
							ID:   "call-abc",
							Type: "function",
							Function: struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							}{Name: "read_file", Arguments: `{"path":"/etc/hosts"}`},
						}},
					},
					FinishReason: "tool_calls",
				}},
			}
			json.NewEncoder(w).Encode(resp)
		}))
		defer srv.Close()

		client := NewOpenRouter("key", srv.URL)
		resp, err := client.Complete(context.Background(), CompletionRequest{
			Model:    "test-model",
			Messages: []ChatMessage{{Role: RoleUser, Content: "read the file"}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(resp.ToolCalls) != 1 {
			t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
		}
		tc := resp.ToolCalls[0]
		if tc.ID != "call-abc" {
			t.Errorf("tool call ID: got %q, want %q", tc.ID, "call-abc")
		}
		if tc.Name != "read_file" {
			t.Errorf("tool name: got %q, want %q", tc.Name, "read_file")
		}
		if string(tc.Arguments) != `{"path":"/etc/hosts"}` {
			t.Errorf("arguments: got %q", tc.Arguments)
		}
	})

	t.Run("non-retryable API error is returned immediately", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid api key"}`))
		}))
		defer srv.Close()

		client := NewOpenRouter("bad-key", srv.URL)
		_, err := client.Complete(context.Background(), CompletionRequest{
			Model:    "m",
			Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
		})
		if err == nil {
			t.Error("expected error for 401, got nil")
		}
		if !strings.Contains(err.Error(), "401") {
			t.Errorf("error should mention status 401, got: %v", err)
		}
	})

	t.Run("empty choices returns error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(oaiResponse{Model: "m"})
		}))
		defer srv.Close()

		client := NewOpenRouter("key", srv.URL)
		_, err := client.Complete(context.Background(), CompletionRequest{
			Model:    "m",
			Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
		})
		if err == nil {
			t.Error("expected error for empty choices")
		}
		if !strings.Contains(err.Error(), "no choices") {
			t.Errorf("error should mention 'no choices', got: %v", err)
		}
	})

	t.Run("tool_choice is auto when tools are provided", func(t *testing.T) {
		var captured oaiRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewDecoder(r.Body).Decode(&captured)
			w.Write(successResponse(t, "ok"))
		}))
		defer srv.Close()

		client := NewOpenRouter("key", srv.URL)
		client.Complete(context.Background(), CompletionRequest{
			Model:    "m",
			Messages: []ChatMessage{{Role: RoleUser, Content: "use a tool"}},
			Tools: []Tool{{
				Name:        "read_file",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
		})

		if captured.ToolChoice != "auto" {
			t.Errorf("tool_choice: got %q, want %q", captured.ToolChoice, "auto")
		}
	})

	t.Run("cancelled context returns error immediately", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(successResponse(t, "should not reach here"))
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel before the request

		client := NewOpenRouter("key", srv.URL)
		_, err := client.Complete(ctx, CompletionRequest{
			Model:    "m",
			Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
		})
		if err == nil {
			t.Error("expected error for cancelled context")
		}
	})
}
