// Package hermestest is a fake Hermes Runs API for tests: one HTTP server
// for every profile (/p/<profile>/v1/runs…), with runs whose outcome and
// timing the test controls.
package hermestest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Run is one run as the fake server saw it.
type Run struct {
	ID           string
	Profile      string
	SessionID    string
	Instructions string
	Input        string
	Steers       []string // accepted steer texts, in order
	Stopped      bool     // /stop was called while it ran
	Status       string
	Output       string
	PendingSteer string

	partial bool
}

// Server is the fake. Set Handle (and the optional knobs) before the first
// run starts.
type Server struct {
	URL string

	// Handle runs in its own goroutine for every new run and decides its
	// outcome by calling Finish (or never, for a run that hangs). Default:
	// complete with "ok".
	Handle func(s *Server, r Run)
	// StartStatus, if set, answers POST /v1/runs attempt n (1-based) with
	// that status instead of starting a run (0 means start normally).
	StartStatus func(n int) int
	// SteerStatus, if nonzero, answers every steer with it (no steer kept).
	SteerStatus int
	// StatusFailures answers that many GET /v1/runs/{id} with 503 first.
	StatusFailures int

	mu       sync.Mutex
	runs     map[string]*Run
	order    []string
	starts   int
	requests []string // "METHOD path"
}

// New starts a fake server, closed when the test ends.
func New(t testing.TB) *Server {
	s := &Server{runs: map[string]*Run{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// BaseURL is the base_url of one profile.
func (s *Server) BaseURL(profile string) string { return s.URL + "/p/" + profile }

// Finish ends a running run. consumeSteers=false returns every accepted
// steer in pending_steer, as Hermes does when the agent never drained them.
// Finishing a run that has already ended does nothing.
func (s *Server) Finish(id, status, output string, consumeSteers bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	if r == nil || r.Status != "running" {
		return
	}
	r.Status, r.Output = status, output
	if !consumeSteers {
		r.PendingSteer = strings.Join(r.Steers, "\n")
	}
}

// Get returns a copy of one run.
func (s *Server) Get(id string) Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.runs[id]; r != nil {
		return clone(r)
	}
	return Run{}
}

// Runs returns copies of every run, oldest first.
func (s *Server) Runs() []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Run, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, clone(s.runs[id]))
	}
	return out
}

// Requests lists every request as "METHOD path".
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func clone(r *Run) Run {
	c := *r
	c.Steers = append([]string(nil), r.Steers...)
	return c
}

func (s *Server) serve(w http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, req.Method+" "+req.URL.Path)
	s.mu.Unlock()
	if req.Header.Get("Authorization") == "" {
		writeErr(w, http.StatusUnauthorized, "missing key")
		return
	}
	// /p/<profile>/v1/runs[/<id>[/<action>]]
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "p" || parts[2] != "v1" || parts[3] != "runs" {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	profile, rest := parts[1], parts[4:]
	body, _ := io.ReadAll(req.Body)
	switch {
	case len(rest) == 0 && req.Method == http.MethodPost:
		s.start(w, profile, body)
	case len(rest) == 1 && req.Method == http.MethodGet:
		s.status(w, rest[0])
	case len(rest) == 2 && req.Method == http.MethodPost && rest[1] == "steer":
		s.steer(w, rest[0], body)
	case len(rest) == 2 && req.Method == http.MethodPost && rest[1] == "stop":
		s.stop(w, rest[0])
	case len(rest) == 2 && req.Method == http.MethodPost && rest[1] == "resolve-unknown":
		writeJSON(w, http.StatusOK, map[string]any{"outcome": "interrupted", "run_id": rest[0]})
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) start(w http.ResponseWriter, profile string, body []byte) {
	var in struct {
		Input        string `json:"input"`
		SessionID    string `json:"session_id"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Input == "" {
		writeErr(w, http.StatusBadRequest, "Missing 'input' field")
		return
	}
	s.mu.Lock()
	s.starts++
	if s.StartStatus != nil {
		if code := s.StartStatus(s.starts); code != 0 {
			s.mu.Unlock()
			w.Header().Set("Retry-After", "1")
			writeErr(w, code, "refused")
			return
		}
	}
	id := "run_" + itoa(len(s.order)+1)
	r := &Run{ID: id, Profile: profile, SessionID: in.SessionID, Instructions: in.Instructions, Input: in.Input, Status: "running"}
	s.runs[id] = r
	s.order = append(s.order, id)
	snap := clone(r)
	handle := s.Handle
	s.mu.Unlock()
	if handle == nil {
		handle = func(s *Server, r Run) { s.Finish(r.ID, "completed", "ok", true) }
	}
	go handle(s, snap)
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": id, "status": "started", "replayed": false})
}

// FinishPartial ends a running run as Hermes reports an unfinished turn:
// failed, with output and no error.
func (s *Server) FinishPartial(id, output string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.runs[id]; r != nil && r.Status == "running" {
		r.Status, r.Output, r.partial = "failed", output, true
	}
}

func (s *Server) status(w http.ResponseWriter, id string) {
	s.mu.Lock()
	if s.StatusFailures > 0 {
		s.StatusFailures--
		s.mu.Unlock()
		writeErr(w, http.StatusServiceUnavailable, "runtime_draining")
		return
	}
	r := s.runs[id]
	if r == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "Run not found: "+id)
		return
	}
	out := map[string]any{"object": "hermes.run", "run_id": id, "status": r.Status, "session_id": r.SessionID,
		"admission_id": "adm_" + id, "execution_generation": 1}
	switch {
	case r.Status == "completed":
		out["output"] = r.Output
	case r.partial:
		out["output"], out["partial"], out["completed"], out["turn_exit_reason"] = r.Output, true, false, "max_iterations"
	case r.Status == "failed":
		out["error"] = r.Output
	}
	if r.PendingSteer != "" {
		out["pending_steer"] = r.PendingSteer
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) steer(w http.ResponseWriter, id string, body []byte) {
	var in struct {
		Input string `json:"input"`
	}
	_ = json.Unmarshal(body, &in)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.SteerStatus != 0 {
		writeErr(w, s.SteerStatus, "steer refused by test")
		return
	}
	r := s.runs[id]
	if r == nil || r.Status != "running" {
		writeErr(w, http.StatusConflict, "Run is not currently accepting steer input: "+id)
		return
	}
	r.Steers = append(r.Steers, in.Input)
	writeJSON(w, http.StatusOK, map[string]any{"object": "hermes.run.steer", "run_id": id, "accepted": true})
}

func (s *Server) stop(w http.ResponseWriter, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	if r == nil {
		writeErr(w, http.StatusNotFound, "Run not found: "+id)
		return
	}
	if r.Status == "running" {
		r.Stopped, r.Status = true, "cancelled"
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "status": "stopping"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error"}})
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
