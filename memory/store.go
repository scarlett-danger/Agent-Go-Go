package memory

import (
	"context"

	"database/sql"

	"errors"

	"fmt"

	"strings"

	"time"

	// Pure-Go SQLite driver — no CGO required (important for Windows users).

	_ "modernc.org/sqlite"

	"github.com/scarlett-danger/Agent-Go-Go/llm"
)

// Message is a stored conversation turn.

type Message struct {
	ID int64

	ConversationID string

	Role string // user | assistant | system (for summaries)

	Content string

	Provider string // which provider produced this (empty for user msgs)

	Model string // which model produced this (empty for user msgs)

	SlackTS string // empty for system/summary rows

	CreatedAt time.Time
}

// Store wraps the SQLite handle. Safe for concurrent use; the underlying

// *sql.DB has its own pool. modernc.org/sqlite uses a single writer

// internally, so don't expect huge write parallelism — fine at our scale.

type Store struct {
	db *sql.DB
}

// Open creates/opens the database file and runs migrations.

// path is a filesystem path; use ":memory:" for tests.

func Open(path string) (*Store, error) {

	// _pragma=foreign_keys(1) enforces FK constraints (off by default in SQLite).

	// _pragma=journal_mode(WAL) gives much better concurrent read/write behavior.

	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)", path)

	db, err := sql.Open("sqlite", dsn)

	if err != nil {

		return nil, fmt.Errorf("open sqlite: %w", err)

	}

	if err := db.Ping(); err != nil {

		return nil, fmt.Errorf("ping sqlite: %w", err)

	}

	s := &Store{db: db}

	if err := s.migrate(); err != nil {

		return nil, fmt.Errorf("migrate: %w", err)

	}

	return s, nil

}

func (s *Store) Close() error { return s.db.Close() }

// migrate runs the schema. Idempotent — safe to call on every startup.

// Embedding the SQL inline keeps the migration self-contained without

// pulling in a migration framework.

func (s *Store) migrate() error {

	const schema = `

	CREATE TABLE IF NOT EXISTS messages (

		id              INTEGER PRIMARY KEY AUTOINCREMENT,

		conversation_id TEXT    NOT NULL,

		role            TEXT    NOT NULL,

		content         TEXT    NOT NULL,

		provider        TEXT    NOT NULL DEFAULT '',

		model           TEXT    NOT NULL DEFAULT '',

		slack_ts        TEXT    NOT NULL DEFAULT '',

		created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP

	);

	-- Idempotency: same Slack message can't be inserted twice into the

	-- same conversation. Activity retries become no-ops via INSERT OR IGNORE.

	-- The empty-string slack_ts case (for summaries) is allowed multiple

	-- times because the partial unique index excludes empty strings.

	CREATE UNIQUE INDEX IF NOT EXISTS idx_msg_unique_slack

		ON messages(conversation_id, slack_ts)

		WHERE slack_ts != '';

	-- Fast retrieval of recent turns per conversation.

	CREATE INDEX IF NOT EXISTS idx_msg_conv_created

		ON messages(conversation_id, created_at);

	-- Summaries are a separate table so we can replace them atomically

	-- when re-summarizing without touching the raw message log.

	CREATE TABLE IF NOT EXISTS summaries (

		conversation_id TEXT PRIMARY KEY,

		content         TEXT NOT NULL,

		covers_until_id INTEGER NOT NULL,  -- the highest message.id that this summary subsumes

		created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP

	);

	`

	_, err := s.db.ExecContext(context.Background(), schema)

	return err

}

// AppendUserMessage records an incoming user turn. Idempotent on (conv, slack_ts).

// Returns true if the row was new, false if it was a duplicate (which is

// fine — Slack retries are why we deduplicate).

func (s *Store) AppendUserMessage(ctx context.Context, convID, content, slackTS string) (bool, error) {

	res, err := s.db.ExecContext(ctx, `

		INSERT OR IGNORE INTO messages(conversation_id, role, content, slack_ts)

		VALUES (?, ?, ?, ?)`,

		convID, llm.RoleUser, content, slackTS)

	if err != nil {

		return false, fmt.Errorf("append user msg: %w", err)

	}

	n, _ := res.RowsAffected()

	return n > 0, nil

}

// AppendAssistantMessage records the bot's reply. We don't have a Slack TS

// at insert time (that comes back from chat.postMessage), so we use a

// synthetic key: the inserted row's autoincrement ID guarantees uniqueness.

// Activity retries are protected at the workflow level via deterministic

// workflow IDs, so this row writing once-per-workflow is safe enough.

func (s *Store) AppendAssistantMessage(ctx context.Context, convID, content, provider, model string) error {

	_, err := s.db.ExecContext(ctx, `

		INSERT INTO messages(conversation_id, role, content, provider, model)

		VALUES (?, ?, ?, ?, ?)`,

		convID, llm.RoleAssistant, content, provider, model)

	if err != nil {

		return fmt.Errorf("append assistant msg: %w", err)

	}

	return nil

}

// LoadContext returns the messages to send to the LLM for the next turn.

// Layout: [optional summary as system msg] + last `maxTurns` real turns,

// ordered oldest → newest. The summary (if present) gives the model

// long-term memory beyond the sliding window.

func (s *Store) LoadContext(ctx context.Context, convID string, maxTurns int) ([]llm.ChatMessage, error) {

	out := []llm.ChatMessage{}

	// 1. Pull summary, if any.

	var summary string

	err := s.db.QueryRowContext(ctx, `

		SELECT content FROM summaries WHERE conversation_id = ?`, convID).Scan(&summary)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {

		return nil, fmt.Errorf("load summary: %w", err)

	}

	if summary != "" {

		out = append(out, llm.ChatMessage{

			Role: llm.RoleSystem,

			Content: "Earlier in this conversation: " + summary,
		})

	}

	// 2. Pull the most recent `maxTurns` messages, then reverse to chronological.

	rows, err := s.db.QueryContext(ctx, `

		SELECT role, content FROM messages

		WHERE conversation_id = ?

		ORDER BY id DESC

		LIMIT ?`, convID, maxTurns)

	if err != nil {

		return nil, fmt.Errorf("load messages: %w", err)

	}

	defer rows.Close()

	recent := []llm.ChatMessage{}

	for rows.Next() {

		var m llm.ChatMessage

		if err := rows.Scan(&m.Role, &m.Content); err != nil {

			return nil, fmt.Errorf("scan message: %w", err)

		}

		recent = append(recent, m)

	}

	if err := rows.Err(); err != nil {

		return nil, fmt.Errorf("iter messages: %w", err)

	}

	// Reverse to chronological order.

	for i := len(recent) - 1; i >= 0; i-- {

		out = append(out, recent[i])

	}

	return out, nil

}

// CountMessages reports how many real turns exist in a conversation.

// Used to decide whether summarization is due.

func (s *Store) CountMessages(ctx context.Context, convID string) (int, error) {

	var n int

	err := s.db.QueryRowContext(ctx,

		`SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, convID).Scan(&n)

	if err != nil {

		return 0, fmt.Errorf("count messages: %w", err)

	}

	return n, nil

}

// OldestUnsummarized returns the N oldest messages that haven't been folded

// into a summary yet. Used as input to the summarization activity.

func (s *Store) OldestUnsummarized(ctx context.Context, convID string, n int) ([]Message, int64, error) {

	// Find the high-water mark of the existing summary (covers_until_id).

	var coversUntil int64

	err := s.db.QueryRowContext(ctx,

		`SELECT covers_until_id FROM summaries WHERE conversation_id = ?`, convID).Scan(&coversUntil)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {

		return nil, 0, fmt.Errorf("read covers_until_id: %w", err)

	}

	rows, err := s.db.QueryContext(ctx, `

		SELECT id, role, content FROM messages

		WHERE conversation_id = ? AND id > ?

		ORDER BY id ASC

		LIMIT ?`, convID, coversUntil, n)

	if err != nil {

		return nil, 0, fmt.Errorf("query oldest unsummarized: %w", err)

	}

	defer rows.Close()

	out := []Message{}

	var lastID int64

	for rows.Next() {

		var m Message

		if err := rows.Scan(&m.ID, &m.Role, &m.Content); err != nil {

			return nil, 0, fmt.Errorf("scan: %w", err)

		}

		out = append(out, m)

		lastID = m.ID

	}

	return out, lastID, rows.Err()

}

// UpsertSummary replaces (or inserts) the rolling summary, advancing the

// covers_until_id so the same messages aren't re-summarized.

func (s *Store) UpsertSummary(ctx context.Context, convID, content string, coversUntilID int64) error {

	_, err := s.db.ExecContext(ctx, `

		INSERT INTO summaries(conversation_id, content, covers_until_id)

		VALUES (?, ?, ?)

		ON CONFLICT(conversation_id) DO UPDATE SET

			content = excluded.content,

			covers_until_id = excluded.covers_until_id,

			created_at = CURRENT_TIMESTAMP`,

		convID, content, coversUntilID)

	if err != nil {

		return fmt.Errorf("upsert summary: %w", err)

	}

	return nil

}

// ConversationIDFor builds the conversation key from a Slack event.

// Threaded messages get the thread_ts; root-channel messages get a

// synthetic channel-level key. This keeps the bot's "memory" coherent with

// how humans perceive Slack threads.

func ConversationIDFor(channelID, threadTS string) string {

	if strings.TrimSpace(threadTS) != "" {

		return "thread:" + threadTS

	}

	return "channel:" + channelID

}
