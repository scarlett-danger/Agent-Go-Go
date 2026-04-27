package router

import (
	"context"
	"strings"
	"testing"

	"github.com/scarlett-danger/Agent-Go-Go/llm"
)

// mockProvider implements llm.Provider for testing Classify without a live LLM.
type mockProvider struct {
	text string
	err  error
}

func (m *mockProvider) Name() string { return "mock" }

func (m *mockProvider) Complete(_ context.Context, _ llm.CompletionRequest) (*llm.CompletionResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &llm.CompletionResponse{Text: m.text}, nil
}

func TestNewJudge(t *testing.T) {
	cfg := testConfig()
	p := &mockProvider{text: `{"category":"general","reason":"test"}`}
	j := NewJudge(p, "mock-model", cfg)
	if j == nil {
		t.Fatal("expected non-nil Judge")
	}
}

func TestClassify(t *testing.T) {
	cfg := testConfig()

	t.Run("valid JSON response returns correct route", func(t *testing.T) {
		p := &mockProvider{text: `{"category":"code","reason":"it's code"}`}
		j := NewJudge(p, "mock-model", cfg)

		route, err := j.Classify(context.Background(), "write a function", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if route.Category != "code" {
			t.Errorf("category: got %q, want %q", route.Category, "code")
		}
	})

	t.Run("unknown category falls back to default", func(t *testing.T) {
		p := &mockProvider{text: `{"category":"nonexistent","reason":"?"}`}
		j := NewJudge(p, "mock-model", cfg)

		route, _ := j.Classify(context.Background(), "anything", nil)
		if route.Category != cfg.Default {
			t.Errorf("expected default category %q, got %q", cfg.Default, route.Category)
		}
	})

	t.Run("bad JSON falls back to default without error", func(t *testing.T) {
		p := &mockProvider{text: "this is not json at all"}
		j := NewJudge(p, "mock-model", cfg)

		route, err := j.Classify(context.Background(), "anything", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if route.Category != cfg.Default {
			t.Errorf("expected default fallback, got %q", route.Category)
		}
	})

	t.Run("provider error returns default route and error", func(t *testing.T) {
		p := &mockProvider{err: context.DeadlineExceeded}
		j := NewJudge(p, "mock-model", cfg)

		route, err := j.Classify(context.Background(), "anything", nil)
		if err == nil {
			t.Error("expected error to be propagated")
		}
		if route.Category != cfg.Default {
			t.Errorf("expected default fallback on error, got %q", route.Category)
		}
	})
}

func TestParseJudgeJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantCat string
		wantErr bool
	}{
		{
			name:    "clean JSON",
			input:   `{"category":"code","reason":"it's about code"}`,
			wantCat: "code",
		},
		{
			name:    "JSON with code fence and json tag",
			input:   "```json\n{\"category\":\"research\",\"reason\":\"needs lookup\"}\n```",
			wantCat: "research",
		},
		{
			name:    "JSON with plain code fence",
			input:   "```\n{\"category\":\"creative\",\"reason\":\"poem\"}\n```",
			wantCat: "creative",
		},
		{
			name:    "JSON embedded in prose",
			input:   `Sure, here's my classification: {"category":"general","reason":"casual"} Hope that helps!`,
			wantCat: "general",
		},
		{
			name:    "extra whitespace around JSON",
			input:   "   \n  {\"category\":\"reasoning\",\"reason\":\"math\"}  \n  ",
			wantCat: "reasoning",
		},
		{
			name:    "no JSON object in response",
			input:   "I cannot determine the category",
			wantErr: true,
		},
		{
			name:    "malformed JSON",
			input:   `{"category":}`,
			wantErr: true,
		},
		{
			name:    "only opening brace",
			input:   "{incomplete",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseJudgeJSON(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got result: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Category != tc.wantCat {
				t.Errorf("category: got %q, want %q", got.Category, tc.wantCat)
			}
		})
	}
}

func TestBuildJudgeSystemPrompt(t *testing.T) {
	cfg := testConfig()
	prompt := buildJudgeSystemPrompt(cfg)

	if !strings.Contains(prompt, "general") {
		t.Error("prompt should reference 'general' category")
	}
	if !strings.Contains(prompt, "code") {
		t.Error("prompt should reference 'code' category")
	}
	if !strings.Contains(prompt, `"category"`) {
		t.Error("prompt should include JSON output format")
	}
	if !strings.Contains(prompt, cfg.Default) {
		t.Errorf("prompt should mention default category %q", cfg.Default)
	}
}
