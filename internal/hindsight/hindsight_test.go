package hindsight

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type seen struct {
	method, path, auth string
	body               map[string]any
}

func server(t *testing.T, status int, answer string, got *seen) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got.method, got.path, got.auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		got.body = nil
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &got.body); err != nil {
				t.Errorf("request body is not JSON: %s", raw)
			}
		}
		w.WriteHeader(status)
		io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", "roomd-group", "")
}

func TestRecallRequestShape(t *testing.T) {
	var got seen
	c := server(t, 200, `{"results":[{"id":"m1","text":"Alex likes tea","type":"world","occurred_start":"2026-11-12T10:00:00Z","scores":{"final":0.7,"reranker":0.4}}]}`, &got)
	resp, err := c.Recall(context.Background(), RecallRequest{
		Query: "tea?", Types: []string{"world", "observation"}, PreferObservations: true,
		Budget: BudgetLow, MaxTokens: 400,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.method != "POST" || got.path != "/v1/default/banks/roomd-group/memories/recall" || got.auth != "" {
		t.Fatalf("request %s %s auth=%q", got.method, got.path, got.auth)
	}
	b := got.body
	if b["query"] != "tea?" || b["budget"] != "low" || b["max_tokens"] != float64(400) || b["prefer_observations"] != true {
		t.Fatalf("body %v", b)
	}
	inc, ok := b["include"].(map[string]any)
	if !ok {
		t.Fatalf("include missing: %v", b)
	}
	if v, present := inc["entities"]; !present || v != nil {
		t.Fatalf(`include must send "entities": null to turn entity observations off, got %v`, inc)
	}
	if len(resp.Results) != 1 || resp.Results[0].Relevance() != 0.4 {
		t.Fatalf("results %+v", resp.Results)
	}
}

func TestRelevanceFallsBackToFinal(t *testing.T) {
	if got := (RecallResult{Scores: &RecallScores{Final: 0.3}}).Relevance(); got != 0.3 {
		t.Fatalf("relevance %v, want final 0.3", got)
	}
	if got := (RecallResult{}).Relevance(); got != -1 {
		t.Fatalf("relevance without scores %v, want -1", got)
	}
}

func TestRecallMissingBank(t *testing.T) {
	var got seen
	c := server(t, 404, `{"detail":"Bank 'roomd-group' not found"}`, &got)
	if _, err := c.Recall(context.Background(), RecallRequest{Query: "x"}); !errors.Is(err, ErrBankNotFound) {
		t.Fatalf("err %v, want ErrBankNotFound", err)
	}
}

func TestRetain(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		answer  string
		wantErr bool
	}{
		{"ok", 200, `{"success":true,"bank_id":"roomd-group","items_count":1,"async":false}`, false},
		{"success false", 200, `{"success":false,"bank_id":"roomd-group","items_count":0,"async":false}`, true},
		{"server error", 500, `boom`, true},
		{"validation", 422, `{"detail":[]}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got seen
			c := server(t, tt.status, tt.answer, &got)
			c.APIKey = "k"
			_, err := c.Retain(context.Background(), []MemoryItem{{
				Content: "notes", DocumentID: "roomd-x-s1", Tags: []string{"source:roomd"},
				Metadata: map[string]string{"source": "roomd"}, UpdateMode: "replace", Timestamp: "2026-11-18T10:00:00Z",
			}})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err %v, wantErr %v", err, tt.wantErr)
			}
			if got.path != "/v1/default/banks/roomd-group/memories" || got.auth != "Bearer k" {
				t.Fatalf("request %s auth=%q", got.path, got.auth)
			}
			if got.body["async"] != false {
				t.Fatalf("retain must be synchronous: %v", got.body)
			}
			item := got.body["items"].([]any)[0].(map[string]any)
			if item["document_id"] != "roomd-x-s1" || item["update_mode"] != "replace" || item["content"] != "notes" {
				t.Fatalf("item %v", item)
			}
		})
	}
}

func TestBankExistsAndCreate(t *testing.T) {
	var got seen
	c := server(t, 200, `{"banks":[{"bank_id":"hermes-cleo"},{"bank_id":"roomd-group"}],"total":2}`, &got)
	ok, err := c.BankExists(context.Background())
	if err != nil || !ok || got.method != "GET" || got.path != "/v1/default/banks" {
		t.Fatalf("exists=%v err=%v %s %s", ok, err, got.method, got.path)
	}
	c.Bank = "other"
	if ok, _ := c.BankExists(context.Background()); ok {
		t.Fatal("a bank that is not listed exists")
	}
	if err := c.CreateBank(context.Background(), CreateBankRequest{RetainMission: "m"}); err != nil {
		t.Fatal(err)
	}
	if got.method != "PUT" || got.path != "/v1/default/banks/other" || got.body["retain_mission"] != "m" {
		t.Fatalf("create %s %s %v", got.method, got.path, got.body)
	}
}
