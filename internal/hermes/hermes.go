// Package hermes is a minimal client for one Hermes profile's Runs API.
//
// A bot turn is one run: POST /v1/runs starts it with the attributed delta
// as input, the room's group rules as instructions (the ephemeral system
// prompt) and the bot's session id, so Hermes loads the conversation history
// from its own database. roomd then polls GET /v1/runs/{id} until the run is
// terminal, and can steer (POST .../steer) or stop (POST .../stop) it while
// it runs.
package hermes

import (
	"bytes"
	"context"
	"encoding/base64"
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
	BaseURL string // e.g. http://127.0.0.1:8642/p/ada
	APIKey  string
	Model   string       // optional; omitted from the request when empty
	HTTP    *http.Client // per-request timeout; the run's own deadline is the caller's

	MaxRetries429 int                                        // default 3
	MaxBackoff    time.Duration                              // cap on a single 429 wait, default 60s
	Sleep         func(context.Context, time.Duration) error // injectable for tests
}

// RequestTimeout bounds each HTTP request. Runs are asynchronous, so no
// request waits for the agent; the run's overall deadline is the caller's.
const RequestTimeout = 30 * time.Second

// New returns a client for one profile.
func New(baseURL, apiKey, model string) *Client {
	return &Client{BaseURL: baseURL, APIKey: apiKey, Model: model, HTTP: &http.Client{Timeout: RequestTimeout}}
}

// Run statuses. completed, failed, cancelled and interrupted are terminal;
// unknown means the run's owner restarted mid-turn and its outcome is lost.
const (
	StatusCompleted   = "completed"
	StatusFailed      = "failed"
	StatusCancelled   = "cancelled"
	StatusInterrupted = "interrupted"
	StatusUnknown     = "unknown"
)

// Run is the pollable state of one run.
type Run struct {
	ID     string
	Status string
	Output string // the final answer (also on a partial run)
	Error  string // set on a hard failure
	// Partial: the turn ended unfinished (cut short, or out of its iteration
	// budget). Hermes reports it as failed, with output and no error.
	Partial        bool
	TurnExitReason string
	// PendingSteer is steer text the agent never consumed ("" when every
	// steer reached the model). Hermes joins several with newlines.
	PendingSteer string
	// AdmissionID and ExecutionGeneration identify an unknown run for
	// ResolveUnknown.
	AdmissionID         string
	ExecutionGeneration int64
}

// Terminal reports whether the run has ended (unknown counts: its outcome
// is lost and it will never complete on its own).
func (r Run) Terminal() bool {
	switch r.Status {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted, StatusUnknown:
		return true
	}
	return false
}

// Reply is the outcome of a run that produced something postable or an
// intentional silence.
type Reply struct {
	Text    string // trimmed; "" when Silent
	Silent  bool   // only a silence marker (or empty): post nothing, no turn
	Partial bool   // an unfinished turn's text, still posted
}

// Reply maps a terminal run to its reply. A completed run, or a failed one
// that still produced text and no error (a partial turn), is a reply;
// anything else wraps ErrNoReply.
func (r Run) Reply() (Reply, error) {
	partial := r.Status == StatusFailed && r.Error == "" && strings.TrimSpace(r.Output) != ""
	if r.Status != StatusCompleted && !partial {
		msg := r.Status
		if r.Error != "" {
			msg += ": " + r.Error
		}
		return Reply{}, fmt.Errorf("%w: run %s", ErrNoReply, msg)
	}
	text := strings.TrimSpace(r.Output)
	if silence.Is(text) {
		return Reply{Silent: true}, nil
	}
	return Reply{Text: text, Partial: partial || r.Partial}, nil
}

// ErrNoReply wraps every failure that means "post nothing": a failed,
// cancelled, interrupted or lost run, HTTP errors, exhausted 429 retries.
var ErrNoReply = errors.New("hermes: no reply")

// ErrRejected means Hermes refused the request itself (a 4xx other than
// 429), so sending it again unchanged won't help.
var ErrRejected = fmt.Errorf("%w: request rejected", ErrNoReply)

// ErrRunNotFound means Hermes no longer knows the run (HTTP 404): its
// outcome is lost. Other polling errors are transient.
var ErrRunNotFound = fmt.Errorf("%w: run not found", ErrNoReply)

// ErrSteerRefused means the run no longer accepts steer input (HTTP 409:
// it finished, is stopping, or is not running yet).
var ErrSteerRefused = errors.New("hermes: steer refused")

type runRequest struct {
	Model        string `json:"model,omitempty"`
	Input        any    `json:"input"` // a string, or one user message with content parts
	SessionID    string `json:"session_id"`
	Instructions string `json:"instructions"`
}

// Image is a picture attached to a run's input. Label is a line of text put
// right before it (who posted it, its name), so the model can refer to it.
type Image struct {
	Label string
	MIME  string
	Data  []byte
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type inputMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

// runInput is the plain text when there are no images; otherwise one user
// message whose content is the text, then each image's label and the image
// as a data URL (the OpenAI shape Hermes passes on to a vision model).
func runInput(text string, images []Image) any {
	if len(images) == 0 {
		return text
	}
	parts := []contentPart{{Type: "text", Text: text}}
	for _, img := range images {
		if img.Label != "" {
			parts = append(parts, contentPart{Type: "text", Text: img.Label})
		}
		url := "data:" + img.MIME + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
		parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: url}})
	}
	return []inputMessage{{Role: "user", Content: parts}}
}

type runStatus struct {
	RunID               string          `json:"run_id"`
	Status              string          `json:"status"`
	Output              string          `json:"output"`
	Error               json.RawMessage `json:"error"`
	PendingSteer        json.RawMessage `json:"pending_steer"`
	Partial             bool            `json:"partial"`
	TurnExitReason      string          `json:"turn_exit_reason"`
	AdmissionID         string          `json:"admission_id"`
	ExecutionGeneration int64           `json:"execution_generation"`
}

// Start creates a run for sessionID and returns its id. instructions is the
// fixed system prompt (byte-identical on every call); input is the delta,
// and images (if any) are attached after it.
func (c *Client) Start(ctx context.Context, sessionID, instructions, input string, images ...Image) (string, error) {
	body, err := json.Marshal(runRequest{Model: c.Model, Input: runInput(input, images), SessionID: sessionID, Instructions: instructions})
	if err != nil {
		return "", err
	}
	maxRetries := c.MaxRetries429
	if maxRetries == 0 {
		maxRetries = 3
	}
	for attempt := 0; ; attempt++ {
		code, hdr, raw, err := c.do(ctx, http.MethodPost, "/v1/runs", body)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrNoReply, err)
		}
		if code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable && hdr.Get("Retry-After") != "" {
			if attempt >= maxRetries {
				return "", fmt.Errorf("%w: HTTP %d after %d retries", ErrNoReply, code, attempt)
			}
			if err := c.sleep(ctx, c.backoff(hdr.Get("Retry-After"), attempt)); err != nil {
				return "", err
			}
			continue
		}
		if code != http.StatusOK && code != http.StatusAccepted {
			kind := ErrNoReply
			if code >= 400 && code < 500 {
				kind = ErrRejected
			}
			return "", fmt.Errorf("%w: HTTP %d: %s%s", kind, code, errorMessage(raw), statusHint(code))
		}
		var st runStatus
		if err := json.Unmarshal(raw, &st); err != nil || st.RunID == "" {
			return "", fmt.Errorf("%w: no run_id in %s", ErrNoReply, truncate(string(raw), 200))
		}
		return st.RunID, nil
	}
}

// Status polls one run. A run Hermes no longer knows (404) returns
// ErrRunNotFound; any other error (429, 5xx while Hermes drains, transport)
// is worth retrying.
func (c *Client) Status(ctx context.Context, runID string) (Run, error) {
	code, _, raw, err := c.do(ctx, http.MethodGet, "/v1/runs/"+runID, nil)
	if err != nil {
		return Run{}, err
	}
	if code == http.StatusNotFound {
		return Run{}, fmt.Errorf("%w: %s", ErrRunNotFound, errorMessage(raw))
	}
	if code != http.StatusOK {
		return Run{}, fmt.Errorf("run status: HTTP %d: %s", code, errorMessage(raw))
	}
	var st runStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return Run{}, fmt.Errorf("bad run status JSON: %v", err)
	}
	return Run{
		ID: runID, Status: st.Status, Output: st.Output, Error: looseString(st.Error),
		PendingSteer: looseString(st.PendingSteer), Partial: st.Partial, TurnExitReason: st.TurnExitReason, AdmissionID: st.AdmissionID, ExecutionGeneration: st.ExecutionGeneration,
	}, nil
}

// Steer injects text into a running run. A 409 (the run is not accepting
// steer input) returns ErrSteerRefused.
func (c *Client) Steer(ctx context.Context, runID, text string) error {
	body, _ := json.Marshal(map[string]string{"input": text})
	code, _, raw, err := c.do(ctx, http.MethodPost, "/v1/runs/"+runID+"/steer", body)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusOK:
		return nil
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrSteerRefused, errorMessage(raw))
	}
	return fmt.Errorf("steer: HTTP %d: %s", code, errorMessage(raw))
}

// Stop asks Hermes to interrupt a run; it settles as cancelled.
func (c *Client) Stop(ctx context.Context, runID string) error {
	code, _, raw, err := c.do(ctx, http.MethodPost, "/v1/runs/"+runID+"/stop", []byte("{}"))
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("stop: HTTP %d: %s", code, errorMessage(raw))
	}
	return nil
}

// ResolveUnknown acknowledges that an unknown run will never finish, so
// Hermes stops holding later turns in that session behind it.
func (c *Client) ResolveUnknown(ctx context.Context, r Run) error {
	body, _ := json.Marshal(map[string]any{"admission_id": r.AdmissionID, "execution_generation": r.ExecutionGeneration})
	code, _, raw, err := c.do(ctx, http.MethodPost, "/v1/runs/"+r.ID+"/resolve-unknown", body)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("resolve-unknown: HTTP %d: %s", code, errorMessage(raw))
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (int, http.Header, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: RequestTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("reading response: %w", err)
	}
	return resp.StatusCode, resp.Header, raw, nil
}

// looseString decodes a field that is normally a string but may be null or
// another JSON value (kept as its JSON text, so it is never lost).
func looseString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
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
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	return truncate(strings.TrimSpace(string(raw)), 200)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
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
	case http.StatusConflict:
		return " (the session belongs to another Hermes surface, or a run conflict)"
	}
	return ""
}
