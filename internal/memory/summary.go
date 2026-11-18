package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ziliding/hermes-roomd/internal/roster"
)

// Nothing is the summarizer's answer when a conversation holds nothing
// worth remembering; the segment is then skipped (the cursor still moves).
const Nothing = "NOTHING"

// Segment describes one conversation being summarized.
type Segment struct {
	Start, End   time.Time
	Participants []string // display names, the user first
	Messages     string   // <msg from="Name">…</msg> lines, oldest first
}

// SummaryPrompt is the summarizer's fixed system prompt.
func SummaryPrompt(r *roster.Roster) string {
	user := r.User().Display
	var names []string
	for _, p := range r.Bots() {
		names = append(names, p.Display)
	}
	return fmt.Sprintf(`You write memory notes about a group chat between %[1]s (a human) and AI characters (%[2]s). The notes go into a long-term memory that is read later as background, so they must be accurate and plain.

Write short bullet points ("- …"), one fact per bullet, in the past tense, each one understandable on its own, and always say who: "%[1]s said …", "%[1]s asked …", "%[1]s decided …", "Ada suggested …".

Keep:
- what %[1]s said, asked, decided, planned, preferred or disliked;
- concrete events: what happened, dates, places, names, plans and commitments.

Rules:
- Only %[1]s's own words say anything about %[1]s. What a character says is that character's opinion or suggestion, never a fact: attribute it ("Cleo thought …") and keep it only if %[1]s reacted to it or it matters later.
- Do not record terms, nicknames, metaphors or concepts the characters made up. If one must appear because %[1]s used it, mark it: (a term Ada coined).
- Do not invent, guess feelings, interpret, or give advice. No greetings, small talk or role-play.
- Use the date given, not words like "today" or "yesterday".

If nothing is worth remembering, answer exactly %[3]s. Otherwise answer with the bullet points only.`,
		user, strings.Join(names, ", "), Nothing)
}

// SummaryUserPrompt renders one segment for the summarizer.
func SummaryUserPrompt(s Segment) string {
	return fmt.Sprintf("Group chat on %s, from %s to %s UTC, between %s.\nMessages, oldest first:\n<messages>\n%s\n</messages>\n\nWrite the memory notes.",
		s.Start.UTC().Format("2006-01-02"), s.Start.UTC().Format("15:04"), s.End.UTC().Format("15:04"),
		strings.Join(s.Participants, ", "), s.Messages)
}

// ChatSummarizer summarizes with an OpenAI-compatible chat-completions
// endpoint (roomd uses the router's cheap model).
type ChatSummarizer struct {
	BaseURL string // includes /v1; /chat/completions is appended
	APIKey  string
	Model   string
	System  string // SummaryPrompt, built once
	HTTP    *http.Client
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// Summarize returns the cleaned notes, or Nothing.
func (s *ChatSummarizer) Summarize(ctx context.Context, seg Segment) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model:    s.Model,
		Messages: []chatMessage{{Role: "system", Content: s.System}, {Role: "user", Content: SummaryUserPrompt(seg)}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	hc := s.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("summarizer: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
		return "", fmt.Errorf("summarizer: HTTP %d: %s", resp.StatusCode, msg)
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("summarizer: bad response JSON: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", errors.New("summarizer: no choices")
	}
	out := CleanSummary(cr.Choices[0].Message.Content)
	if out == "" {
		return "", errors.New("summarizer: empty answer")
	}
	return out, nil
}

var (
	thinkRe = regexp.MustCompile(`(?is)<think>.*?</think>`)
	fenceRe = regexp.MustCompile("(?m)^\\s*```[a-zA-Z]*\\s*$")
)

// CleanSummary strips <think> blocks and code fences. An answer that is just
// the sentinel (in any case, with punctuation) becomes Nothing.
func CleanSummary(s string) string {
	s = strings.TrimSpace(fenceRe.ReplaceAllString(thinkRe.ReplaceAllString(s, ""), ""))
	if strings.EqualFold(strings.Trim(s, " .!*\"'`"), Nothing) {
		return Nothing
	}
	return s
}
