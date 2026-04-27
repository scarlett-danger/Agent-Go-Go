package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// slackSig computes the v0 HMAC-SHA256 signature Slack expects.
func slackSig(secret, ts, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "v0:%s:%s", ts, body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func newTestEngine(h *SlackHandler) *gin.Engine {
	r := gin.New()
	r.POST("/slack/events", h.HandleEvents)
	return r
}

func TestMatchesKeywords(t *testing.T) {
	tests := []struct {
		name     string
		keywords []string
		text     string
		want     bool
	}{
		{"no keywords passes everything", nil, "anything at all", true},
		{"empty keywords passes everything", []string{}, "anything", true},
		{"substring match", []string{"go-go"}, "hey agent go-go can you help", true},
		{"case insensitive", []string{"go-go"}, "Call Agent GO-GO please", true},
		{"no match", []string{"go-go"}, "hello world how are you", false},
		{"first keyword matches", []string{"go-go", "foo"}, "agent go-go", true},
		{"second keyword matches", []string{"foo", "go-go"}, "agent go-go", true},
		{"none of multiple match", []string{"foo", "bar"}, "baz qux", false},
		{"empty text no keywords passes", []string{}, "", true},
		{"empty text with keyword fails", []string{"go-go"}, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &SlackHandler{TriggerKeywords: tc.keywords}
			if got := h.matchesKeywords(tc.text); got != tc.want {
				t.Errorf("matchesKeywords(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestHandleEvents_BadSignature(t *testing.T) {
	const secret = "real-secret-abc123456789012345x"
	h := NewSlackHandler(secret, "U123", nil, "queue", nil)
	engine := newTestEngine(h)

	body := `{"type":"url_verification","challenge":"xyz"}`
	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	// Valid hex format but incorrect HMAC value — passes NewSecretsVerifier init,
	// then fails sv.Ensure(), returning 401 (not 400).
	req.Header.Set("X-Slack-Signature", "v0=0000000000000000000000000000000000000000000000000000000000000000")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("bad signature: got status %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHandleEvents_URLVerification(t *testing.T) {
	const secret = "test-signing-secret-1234567890ab"
	h := NewSlackHandler(secret, "U123", nil, "queue", nil)
	engine := newTestEngine(h)

	body := `{"token":"tok","type":"url_verification","challenge":"my-challenge-value"}`
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := slackSig(secret, ts, body)

	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", sig)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("url verification: got status %d, want %d", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "my-challenge-value") {
		t.Errorf("response should contain challenge, got: %s", w.Body.String())
	}
}

func TestHandleEvents_CallbackACKs200(t *testing.T) {
	const secret = "test-signing-secret-1234567890ab"
	h := NewSlackHandler(secret, "U123", nil, "queue", nil)
	engine := newTestEngine(h)

	// Empty user field — dispatch returns immediately at the first guard,
	// so startWorkflow (which needs a real Temporal client) is never called.
	body := `{"type":"event_callback","event":{"type":"message","user":"","text":"hello","ts":"1"}}`
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := slackSig(secret, ts, body)

	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", sig)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("callback event: got status %d, want %d", w.Code, http.StatusOK)
	}
}
