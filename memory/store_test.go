package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/scarlett-danger/Agent-Go-Go/llm"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpen(t *testing.T) {
	s := openTestStore(t)
	if s == nil {
		t.Fatal("expected non-nil store")
	}
}

func TestAppendUserMessage(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	t.Run("new message returns created=true", func(t *testing.T) {
		created, err := s.AppendUserMessage(ctx, "conv-1", "hello", "ts-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !created {
			t.Error("expected created=true for new message")
		}
	})

	t.Run("duplicate slack_ts is idempotent", func(t *testing.T) {
		created, err := s.AppendUserMessage(ctx, "conv-1", "hello again", "ts-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if created {
			t.Error("expected created=false for duplicate (ts-1 already exists in conv-1)")
		}
	})

	t.Run("same ts in different conversation is allowed", func(t *testing.T) {
		created, err := s.AppendUserMessage(ctx, "conv-2", "hello", "ts-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !created {
			t.Error("expected created=true: ts-1 is new for conv-2")
		}
	})
}

func TestAppendAssistantMessage(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	err := s.AppendAssistantMessage(ctx, "conv-a", "I can help with that.", "openrouter", "gpt-4o")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	n, err := s.CountMessages(ctx, "conv-a")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 message, got %d", n)
	}
}

func TestLoadContext(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	t.Run("empty conversation returns empty slice", func(t *testing.T) {
		msgs, err := s.LoadContext(ctx, "empty-conv", 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("expected 0 messages, got %d", len(msgs))
		}
	})

	t.Run("messages are returned in chronological order", func(t *testing.T) {
		s.AppendUserMessage(ctx, "conv-order", "first", "ts-a")
		s.AppendAssistantMessage(ctx, "conv-order", "second", "", "")
		s.AppendUserMessage(ctx, "conv-order", "third", "ts-b")

		msgs, err := s.LoadContext(ctx, "conv-order", 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(msgs) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(msgs))
		}
		if msgs[0].Content != "first" {
			t.Errorf("msgs[0]: got %q, want %q", msgs[0].Content, "first")
		}
		if msgs[1].Content != "second" {
			t.Errorf("msgs[1]: got %q, want %q", msgs[1].Content, "second")
		}
		if msgs[2].Content != "third" {
			t.Errorf("msgs[2]: got %q, want %q", msgs[2].Content, "third")
		}
	})

	t.Run("maxTurns limits to the most recent N messages", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			s.AppendUserMessage(ctx, "conv-limit", "msg", fmt.Sprintf("ts-%d", i))
		}
		msgs, err := s.LoadContext(ctx, "conv-limit", 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(msgs) != 3 {
			t.Errorf("expected 3 messages with maxTurns=3, got %d", len(msgs))
		}
	})

	t.Run("summary is prepended as system message", func(t *testing.T) {
		s.AppendUserMessage(ctx, "conv-summ", "hello", "ts-s1")
		s.UpsertSummary(ctx, "conv-summ", "A prior summary of the conversation.", 999)

		msgs, err := s.LoadContext(ctx, "conv-summ", 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(msgs) < 1 {
			t.Fatal("expected at least 1 message")
		}
		if msgs[0].Role != llm.RoleSystem {
			t.Errorf("first message should be system (summary), got %q", msgs[0].Role)
		}
		if !strings.Contains(msgs[0].Content, "A prior summary") {
			t.Errorf("system message should contain summary text, got %q", msgs[0].Content)
		}
	})
}

func TestCountMessages(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	n, err := s.CountMessages(ctx, "empty-count")
	if err != nil {
		t.Fatalf("count on empty conv: %v", err)
	}
	if n != 0 {
		t.Errorf("empty conv: got %d, want 0", n)
	}

	s.AppendUserMessage(ctx, "counting", "a", "ts-c1")
	s.AppendUserMessage(ctx, "counting", "b", "ts-c2")
	s.AppendAssistantMessage(ctx, "counting", "reply", "", "")

	n, err = s.CountMessages(ctx, "counting")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 messages, got %d", n)
	}
}

func TestOldestUnsummarized(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	for i := 1; i <= 5; i++ {
		s.AppendUserMessage(ctx, "conv-unsumm", fmt.Sprintf("msg%d", i), fmt.Sprintf("ts-%d", i))
	}

	t.Run("no existing summary returns all messages", func(t *testing.T) {
		msgs, lastID, err := s.OldestUnsummarized(ctx, "conv-unsumm", 10)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(msgs) != 5 {
			t.Errorf("expected 5 messages, got %d", len(msgs))
		}
		if lastID == 0 {
			t.Error("lastID should be non-zero")
		}
	})

	t.Run("existing summary advances the window", func(t *testing.T) {
		// Summarise the first 3 messages.
		first3, lastID, _ := s.OldestUnsummarized(ctx, "conv-unsumm", 3)
		s.UpsertSummary(ctx, "conv-unsumm", "summary of first 3", lastID)

		remaining, _, err := s.OldestUnsummarized(ctx, "conv-unsumm", 10)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(remaining) >= len(first3) {
			t.Errorf("expected fewer messages after summarising first 3, got %d", len(remaining))
		}
	})

	t.Run("limit is respected", func(t *testing.T) {
		msgs, _, err := s.OldestUnsummarized(ctx, "conv-unsumm", 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(msgs) > 1 {
			t.Errorf("limit=1 should return at most 1 message, got %d", len(msgs))
		}
	})
}

func TestUpsertSummary(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	t.Run("insert new summary", func(t *testing.T) {
		if err := s.UpsertSummary(ctx, "conv-ups", "first summary text", 5); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("update existing summary", func(t *testing.T) {
		if err := s.UpsertSummary(ctx, "conv-ups", "updated summary text", 10); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Verify update was applied by reading it back via LoadContext.
		s.AppendUserMessage(ctx, "conv-ups", "a message", "ts-ups-1")
		msgs, err := s.LoadContext(ctx, "conv-ups", 20)
		if err != nil {
			t.Fatalf("load context: %v", err)
		}
		if len(msgs) < 1 || msgs[0].Role != llm.RoleSystem {
			t.Fatal("expected summary as first system message")
		}
		if !strings.Contains(msgs[0].Content, "updated summary text") {
			t.Errorf("expected updated summary, got %q", msgs[0].Content)
		}
	})
}

func TestConversationIDFor(t *testing.T) {
	tests := []struct {
		channelID string
		threadTS  string
		want      string
	}{
		{"C123", "", "channel:C123"},
		{"C123", "1234567890.123456", "thread:1234567890.123456"},
		{"C123", "   ", "channel:C123"},
		{"C456", "T789", "thread:T789"},
	}

	for _, tc := range tests {
		got := ConversationIDFor(tc.channelID, tc.threadTS)
		if got != tc.want {
			t.Errorf("ConversationIDFor(%q, %q) = %q, want %q",
				tc.channelID, tc.threadTS, got, tc.want)
		}
	}
}
