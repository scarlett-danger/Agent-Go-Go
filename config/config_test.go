package config

import (
	"testing"
)

func TestParseKeywords(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"empty string returns nil", "", nil},
		{"whitespace only returns nil", "   ", nil},
		{"single keyword", "agent go-go", []string{"agent go-go"}},
		{"multiple keywords", "agent go-go,go-go,help", []string{"agent go-go", "go-go", "help"}},
		{"trims surrounding whitespace", " agent go-go , go-go ", []string{"agent go-go", "go-go"}},
		{"lowercases all keywords", "Agent Go-Go,GO-GO", []string{"agent go-go", "go-go"}},
		{"skips empty parts from double comma", "one,,two", []string{"one", "two"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseKeywords(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("len: got %v want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] got %q want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("TEST_ENVOR_SET", "hello")

	if got := envOr("TEST_ENVOR_SET", "default"); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
	if got := envOr("TEST_ENVOR_MISSING", "default"); got != "default" {
		t.Errorf("got %q, want %q", got, "default")
	}
}

func TestEnvInt(t *testing.T) {
	t.Setenv("TEST_INT_VALID", "42")
	t.Setenv("TEST_INT_BAD", "not-a-number")

	if got := envInt("TEST_INT_VALID", 0); got != 42 {
		t.Errorf("got %d, want 42", got)
	}
	if got := envInt("TEST_INT_MISSING", 99); got != 99 {
		t.Errorf("got %d, want 99 (fallback)", got)
	}
	if got := envInt("TEST_INT_BAD", 5); got != 5 {
		t.Errorf("got %d, want 5 (fallback on bad value)", got)
	}
}

func TestLoad(t *testing.T) {
	t.Run("succeeds with all required vars set", func(t *testing.T) {
		t.Setenv("SLACK_USER_TOKEN", "xoxp-test-token")
		t.Setenv("SLACK_SIGNING_SECRET", "test-signing-secret")
		t.Setenv("OPENROUTER_API_KEY", "sk-or-test-key")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.SlackUserToken != "xoxp-test-token" {
			t.Errorf("SlackUserToken: got %q", cfg.SlackUserToken)
		}
		if cfg.OpenRouterURL == "" {
			t.Error("OpenRouterURL should have a default")
		}
		if cfg.TemporalTaskQueue == "" {
			t.Error("TemporalTaskQueue should have a default")
		}
	})

	t.Run("fails when required vars are missing", func(t *testing.T) {
		t.Setenv("SLACK_USER_TOKEN", "")
		t.Setenv("SLACK_SIGNING_SECRET", "")
		t.Setenv("OPENROUTER_API_KEY", "")

		_, err := Load()
		if err == nil {
			t.Error("expected error for missing required vars, got nil")
		}
	})

	t.Run("TRIGGER_KEYWORDS is parsed into slice", func(t *testing.T) {
		t.Setenv("SLACK_USER_TOKEN", "xoxp-test")
		t.Setenv("SLACK_SIGNING_SECRET", "secret")
		t.Setenv("OPENROUTER_API_KEY", "key")
		t.Setenv("TRIGGER_KEYWORDS", "agent go-go,go-go")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.TriggerKeywords) != 2 {
			t.Errorf("TriggerKeywords: got %v, want 2 entries", cfg.TriggerKeywords)
		}
	})
}

func TestEnvFloat(t *testing.T) {
	t.Setenv("TEST_FLOAT_VALID", "0.25")
	t.Setenv("TEST_FLOAT_BAD", "not-a-float")

	if got := envFloat("TEST_FLOAT_VALID", 0); got != 0.25 {
		t.Errorf("got %f, want 0.25", got)
	}
	if got := envFloat("TEST_FLOAT_MISSING", 0.5); got != 0.5 {
		t.Errorf("got %f, want 0.5 (fallback)", got)
	}
	if got := envFloat("TEST_FLOAT_BAD", 1.0); got != 1.0 {
		t.Errorf("got %f, want 1.0 (fallback on bad value)", got)
	}
}
