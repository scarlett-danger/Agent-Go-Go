package router

import (
	"os"
	"strings"
	"testing"
)

func testConfig() *Config {
	return &Config{
		Default: "general",
		Categories: map[string]CategoryRule{
			"general": {Model: "model-a", Description: "General messages"},
			"code":    {Model: "model-b", Description: "Code questions", Tools: []string{"read_file", "write_file"}},
		},
		Overrides: map[string]string{
			"code": "code",
		},
	}
}

func TestResolve(t *testing.T) {
	cfg := testConfig()

	t.Run("known category returns its rule", func(t *testing.T) {
		r := cfg.Resolve("code", "it's code")
		if r.Category != "code" {
			t.Errorf("category: got %q, want %q", r.Category, "code")
		}
		if r.Model != "model-b" {
			t.Errorf("model: got %q, want %q", r.Model, "model-b")
		}
		if r.Provider != "openrouter" {
			t.Errorf("provider: got %q, want openrouter", r.Provider)
		}
		if len(r.Tools) != 2 {
			t.Errorf("tools: got %v, want 2 entries", r.Tools)
		}
	})

	t.Run("unknown category falls back to default", func(t *testing.T) {
		r := cfg.Resolve("nonexistent", "unknown")
		if r.Category != "general" {
			t.Errorf("expected default category, got %q", r.Category)
		}
		if r.Model != "model-a" {
			t.Errorf("expected default model, got %q", r.Model)
		}
	})

	t.Run("reason is preserved on the route", func(t *testing.T) {
		r := cfg.Resolve("general", "my specific reason")
		if r.Reason != "my specific reason" {
			t.Errorf("reason: got %q, want %q", r.Reason, "my specific reason")
		}
	})

	t.Run("no-tool category returns nil tools", func(t *testing.T) {
		r := cfg.Resolve("general", "")
		if r.Tools != nil {
			t.Errorf("expected nil tools for general, got %v", r.Tools)
		}
	})
}

func TestCheckOverride(t *testing.T) {
	cfg := testConfig()

	tests := []struct {
		name     string
		input    string
		wantCat  string
		wantText string
	}{
		{"no prefix — passthrough", "hello world", "", "hello world"},
		{"valid override strips prefix and text", "[!code] write tests", "code", "write tests"},
		{"override is case-insensitive", "[!CODE] debug this", "code", "debug this"},
		{"no trailing text after override", "[!code]", "code", ""},
		{"unknown tag — passthrough", "[!nemotron] hello", "", "[!nemotron] hello"},
		{"empty tag — passthrough", "[!] hello", "", "[!] hello"},
		{"missing closing bracket — passthrough", "[!code write more", "", "[!code write more"},
		{"leading spaces on input", "  [!code] test", "code", "test"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotCat, gotText := cfg.CheckOverride(tc.input)
			if gotCat != tc.wantCat {
				t.Errorf("category: got %q, want %q", gotCat, tc.wantCat)
			}
			if gotText != tc.wantText {
				t.Errorf("text: got %q, want %q", gotText, tc.wantText)
			}
		})
	}
}

func TestCategorySummary(t *testing.T) {
	cfg := testConfig()
	summary := cfg.CategorySummary()

	if !strings.Contains(summary, "general") {
		t.Error("summary missing 'general' category name")
	}
	if !strings.Contains(summary, "code") {
		t.Error("summary missing 'code' category name")
	}
	if !strings.Contains(summary, "General messages") {
		t.Error("summary missing 'general' description")
	}
}

func TestLoadConfig(t *testing.T) {
	t.Run("valid yaml loads correctly", func(t *testing.T) {
		f := writeTempYAML(t, `
default: general
categories:
  general:
    model: gpt-4o-mini
    description: General chat
  code:
    model: gpt-4o
    description: Code help
    tools: [read_file]
overrides:
  code: code
`)
		cfg, err := LoadConfig(f)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Default != "general" {
			t.Errorf("default: got %q, want %q", cfg.Default, "general")
		}
		if len(cfg.Categories) != 2 {
			t.Errorf("categories: got %d, want 2", len(cfg.Categories))
		}
		if cfg.Categories["code"].Model != "gpt-4o" {
			t.Errorf("code model: got %q, want gpt-4o", cfg.Categories["code"].Model)
		}
		if len(cfg.Categories["code"].Tools) != 1 {
			t.Errorf("code tools: got %v, want 1", cfg.Categories["code"].Tools)
		}
	})

	t.Run("missing file returns error", func(t *testing.T) {
		_, err := LoadConfig("/nonexistent/routing.yaml")
		if err == nil {
			t.Error("expected error for missing file, got nil")
		}
	})

	t.Run("empty categories returns error", func(t *testing.T) {
		f := writeTempYAML(t, "default: general\ncategories: {}\n")
		_, err := LoadConfig(f)
		if err == nil {
			t.Error("expected error for empty categories, got nil")
		}
	})

	t.Run("default not in categories returns error", func(t *testing.T) {
		f := writeTempYAML(t, `
default: missing
categories:
  general:
    model: gpt-4o-mini
    description: General
`)
		_, err := LoadConfig(f)
		if err == nil {
			t.Error("expected error when default category not in categories, got nil")
		}
	})

	t.Run("invalid yaml returns error", func(t *testing.T) {
		f := writeTempYAML(t, "default: [not valid yaml structure\n")
		_, err := LoadConfig(f)
		if err == nil {
			t.Error("expected error for invalid yaml, got nil")
		}
	})
}

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "routing-*.yaml")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	f.Close()
	return f.Name()
}
