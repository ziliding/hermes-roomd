// Package hindsight is a small typed client for the parts of the Hindsight
// HTTP API (v1.2) that roomd uses on its group bank: recall, a synchronous
// retain of one document, and creating the bank if it does not exist yet.
//
// Shapes follow the server's own OpenAPI spec (GET /openapi.json):
//
//	POST /v1/default/banks/{bank}/memories/recall   RecallRequest  → RecallResponse
//	POST /v1/default/banks/{bank}/memories          RetainRequest  → RetainResponse
//	GET  /v1/default/banks                          → {"banks": [{"bank_id": …}]}
//	PUT  /v1/default/banks/{bank}                   CreateBankRequest
//
// Recall on a bank nothing was ever retained to answers 404; Recall reports
// that as ErrBankNotFound so callers can treat it as "no memories".
package hindsight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ErrBankNotFound is returned by Recall when the bank does not exist yet.
var ErrBankNotFound = errors.New("hindsight: bank not found")

// Client talks to one Hindsight bank.
type Client struct {
	BaseURL string // e.g. http://localhost:8888 (no /v1)
	Bank    string
	APIKey  string // optional; sent as a Bearer token when set
	HTTP    *http.Client
}

// New returns a client for bank at baseURL.
func New(baseURL, bank, apiKey string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Bank: bank, APIKey: apiKey, HTTP: &http.Client{}}
}

// Budget is Hindsight's recall effort level.
type Budget string

const (
	BudgetLow  Budget = "low"
	BudgetMid  Budget = "mid"
	BudgetHigh Budget = "high"
)

// Valid reports whether b is a level the server accepts.
func (b Budget) Valid() bool { return b == BudgetLow || b == BudgetMid || b == BudgetHigh }

// IncludeOptions controls extra data in recall results. Entities is always
// serialized: nil sends "entities": null, which turns the server's default
// (about 500 tokens of entity observations) off.
type IncludeOptions struct {
	Entities *EntityInclude `json:"entities"`
}

// EntityInclude enables entity observations in recall results.
type EntityInclude struct {
	MaxTokens int `json:"max_tokens"`
}

// RecallRequest is the body of POST …/memories/recall.
type RecallRequest struct {
	Query              string         `json:"query"`
	Types              []string       `json:"types,omitempty"`
	PreferObservations bool           `json:"prefer_observations,omitempty"`
	Budget             Budget         `json:"budget,omitempty"`
	MaxTokens          int            `json:"max_tokens,omitempty"`
	QueryTimestamp     string         `json:"query_timestamp,omitempty"`
	Include            IncludeOptions `json:"include"`
}

// RecallScores are the per-stage scores of one result. Final is what results
// are ranked by; Reranker is nil when the server's reranker is a passthrough.
type RecallScores struct {
	Final    float64  `json:"final"`
	Reranker *float64 `json:"reranker"`
	Semantic *float64 `json:"semantic"`
	Keyword  *float64 `json:"keyword"`
}

// RecallResult is one recalled memory.
type RecallResult struct {
	ID            string            `json:"id"`
	Text          string            `json:"text"`
	Type          string            `json:"type"`
	DocumentID    string            `json:"document_id"`
	OccurredStart string            `json:"occurred_start"`
	MentionedAt   string            `json:"mentioned_at"`
	Tags          []string          `json:"tags"`
	Metadata      map[string]string `json:"metadata"`
	Scores        *RecallScores     `json:"scores"`
}

// Relevance is the reranker score when the server reports one, else the
// final ranking score, else -1 (unknown).
func (r RecallResult) Relevance() float64 {
	switch {
	case r.Scores == nil:
		return -1
	case r.Scores.Reranker != nil:
		return *r.Scores.Reranker
	default:
		return r.Scores.Final
	}
}

// RecallResponse is the body of a recall answer.
type RecallResponse struct {
	Results []RecallResult `json:"results"`
}

// MemoryItem is one document to retain.
type MemoryItem struct {
	Content    string            `json:"content"`
	Timestamp  string            `json:"timestamp,omitempty"` // ISO 8601; when the content happened
	Context    string            `json:"context,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"` // string values only (server schema)
	DocumentID string            `json:"document_id,omitempty"`
	Tags       []string          `json:"tags,omitempty"`
	// UpdateMode "replace" (the server default) deletes an existing document
	// with the same id and reprocesses it, so a retried retain is idempotent.
	UpdateMode string `json:"update_mode,omitempty"`
}

// RetainRequest is the body of POST …/memories. roomd always retains
// synchronously (Async false): a 200 then means the facts were extracted
// and stored, so the caller may advance its cursor.
type RetainRequest struct {
	Items []MemoryItem `json:"items"`
	Async bool         `json:"async"`
}

// RetainResponse is the body of a retain answer.
type RetainResponse struct {
	Success    bool   `json:"success"`
	BankID     string `json:"bank_id"`
	ItemsCount int    `json:"items_count"`
}

// CreateBankRequest is the body of PUT /v1/default/banks/{bank}.
type CreateBankRequest struct {
	ReflectMission string `json:"reflect_mission,omitempty"`
	RetainMission  string `json:"retain_mission,omitempty"`
}

// StatusError is a non-2xx answer.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("hindsight: HTTP %d: %s", e.Code, e.Body) }

func (c *Client) bankPath(suffix string) string {
	return c.BaseURL + "/v1/default/banks/" + url.PathEscape(c.Bank) + suffix
}

// Recall queries the bank. A missing bank is ErrBankNotFound.
func (c *Client) Recall(ctx context.Context, req RecallRequest) (RecallResponse, error) {
	var out RecallResponse
	err := c.do(ctx, http.MethodPost, c.bankPath("/memories/recall"), req, &out)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return out, ErrBankNotFound
	}
	return out, err
}

// Retain stores items synchronously. It fails unless the server answers 200
// with success: true.
func (c *Client) Retain(ctx context.Context, items []MemoryItem) (RetainResponse, error) {
	var out RetainResponse
	if err := c.do(ctx, http.MethodPost, c.bankPath("/memories"), RetainRequest{Items: items}, &out); err != nil {
		return out, err
	}
	if !out.Success {
		return out, errors.New("hindsight: retain answered success: false")
	}
	return out, nil
}

// BankExists reports whether the bank is listed by GET /v1/default/banks.
// (GET …/profile on a missing bank answers 410, so it is not used here.)
func (c *Client) BankExists(ctx context.Context) (bool, error) {
	var out struct {
		Banks []struct {
			BankID string `json:"bank_id"`
		} `json:"banks"`
	}
	if err := c.do(ctx, http.MethodGet, c.BaseURL+"/v1/default/banks", nil, &out); err != nil {
		return false, err
	}
	for _, b := range out.Banks {
		if b.BankID == c.Bank {
			return true, nil
		}
	}
	return false, nil
}

// CreateBank creates (or, if it exists, updates) the bank. Callers check
// BankExists first so a user's hand-edited bank config is never overwritten.
func (c *Client) CreateBank(ctx context.Context, req CreateBankRequest) error {
	return c.do(ctx, http.MethodPut, c.bankPath(""), req, nil)
}

func (c *Client) do(ctx context.Context, method, u string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("hindsight: reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return &StatusError{Code: resp.StatusCode, Body: msg}
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("hindsight: bad response JSON: %w", err)
	}
	return nil
}
