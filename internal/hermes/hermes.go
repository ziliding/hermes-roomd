// Package hermes is a minimal client for one Hermes profile's
// OpenAI-compatible chat-completions endpoint.
//
// Every call carries X-Hermes-Session-Id, so the server loads the
// conversation history from its own database and ignores earlier messages in
// the request: roomd sends only [system rules, attributed delta].
package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ziliding/hermes-roomd/internal/silence"
)

// Client talks to one profile (the profile is chosen by BaseURL).
type Client struct {
	BaseURL string // e.g. http://127.0.0.1:8642/p/ada-group
	APIKey  string
	Model   string // optional; omitted from the request when empty
	HTTP    *http.Client

	MaxRetries429 int                                        // default 3
	MaxBackoff    time.Duration                              // cap on a single 429 wait, default 60s
	Sleep         func(context.Context, time.Duration) error // injectable for tests
}

// New returns a client with a per-request timeout (agent runs can take minutes).
func New(baseURL, apiKey, model string, timeout time.Duration) *Client {
	return &Client{BaseURL: baseURL, APIKey: apiKey, Model: model, HTTP: &http.Client{Timeout: timeout}}
}

// Reply is the outcome of a completion that produced something postable or
// an intentional silence.
type Reply struct {
	Text         string // trimmed; "" when Silent
	Silent       bool   // only a silence marker (or empty): post nothing, no turn
	FinishReason string // "stop", or "length" for a soft partial (text is still posted)
}

// ErrNoReply wraps every failure that means "post nothing": finish_reason
// error/cancelled, HTTP 5xx (Hermes answers a hard agent failure with 502),
// exhausted 429 retries, auth errors.
var ErrNoReply = errors.New("hermes: no reply")

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model,omitempty"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
}

type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// Complete sends the fixed system prompt and the attributed delta to sessionID.
func (c *Client) Complete(ctx context.Context, sessionID, system, delta string) (Reply, error) {
	body, err := json.Marshal(chatRequest{
		Model:    c.Model,
		Messages: []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: delta}},
	})
	if err != nil {
		return Reply{}, err
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/v1/chat/completions"
	maxRetries := c.MaxRetries429
	if maxRetries == 0 {
		maxRetries = 3
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return Reply{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		req.Header.Set("X-Hermes-Session-Id", sessionID)

		resp, err := c.HTTP.Do(req)
		if err != nil {
			return Reply{}, fmt.Errorf("%w: %v", ErrNoReply, err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if readErr != nil {
			return Reply{}, fmt.Errorf("%w: reading response: %v", ErrNoReply, readErr)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			if attempt >= maxRetries {
				return Reply{}, fmt.Errorf("%w: HTTP 429 after %d retries", ErrNoReply, attempt)
			}
			if err := c.sleep(ctx, c.backoff(resp.Header.Get("Retry-After"), attempt)); err != nil {
				return Reply{}, err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return Reply{}, fmt.Errorf("%w: HTTP %d: %s%s", ErrNoReply, resp.StatusCode, errorMessage(raw), statusHint(resp.StatusCode))
		}
		return parse(raw)
	}
}

func parse(raw []byte) (Reply, error) {
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return Reply{}, fmt.Errorf("%w: bad JSON: %v", ErrNoReply, err)
	}
	if len(cr.Choices) == 0 {
		return Reply{}, fmt.Errorf("%w: no choices", ErrNoReply)
	}
	ch := cr.Choices[0]
	switch ch.FinishReason {
	case "error", "cancelled":
		return Reply{}, fmt.Errorf("%w: finish_reason=%s", ErrNoReply, ch.FinishReason)
	}
	text := strings.TrimSpace(ch.Message.Content)
	if silence.Is(text) {
		return Reply{Silent: true, FinishReason: ch.FinishReason}, nil
	}
	return Reply{Text: text, FinishReason: ch.FinishReason}, nil
}

func (c *Client) backoff(retryAfter string, attempt int) time.Duration {
	limit := c.MaxBackoff
	if limit == 0 {
		limit = 60 * time.Second
	}
	d := time.Duration(1<<attempt) * 2 * time.Second
	if s, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && s >= 0 {
		d = time.Duration(s) * time.Second
	} else if t, err := http.ParseTime(retryAfter); err == nil {
		d = time.Until(t)
	}
	return max(0, min(d, limit))
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func errorMessage(raw []byte) string {
	var env errorEnvelope
	if json.Unmarshal(raw, &env) == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func statusHint(code int) string {
	switch code {
	case http.StatusUnauthorized:
		return " (wrong API key? check this bot's hermes.key_env)"
	case http.StatusForbidden:
		return " (session continuation needs API_SERVER_KEY set on that profile)"
	case http.StatusNotFound:
		return " (wrong hermes.base_url? it must point at the profile, e.g. .../p/<profile>)"
	}
	return ""
}
