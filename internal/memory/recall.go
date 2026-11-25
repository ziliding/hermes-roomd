// Package memory is roomd's shared memory of past group chats: a Hindsight
// bank (default "roomd-group") that only roomd reads and writes. The bots
// never write to it, and it is separate from each profile's own memory.
//
//   - Recall: before a called bot turn, roomd queries the bank with a short
//     query built from the recent messages since the last /reset (none
//     yet: no recall), and adds what comes back to that
//     turn's user message as a <memory source="past group chats"> block. The
//     system message is never touched (it stays byte-identical for the
//     prompt cache). Every failure is logged and the turn goes ahead without
//     memory.
//   - Retain: once a conversation has gone quiet (or a /reset ended it),
//     roomd writes ONE document for it: by default a filtered summary
//     written by the router's cheap model, or the attributed transcript.
//     A persisted cursor makes this happen once per conversation, across
//     restarts.
package memory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ziliding/hermes-roomd/internal/contextbuild"
	"github.com/ziliding/hermes-roomd/internal/hindsight"
	"github.com/ziliding/hermes-roomd/internal/roster"
	"github.com/ziliding/hermes-roomd/internal/silence"
	"github.com/ziliding/hermes-roomd/internal/transcript"
)

// Searcher is the part of the Hindsight client recall uses.
type Searcher interface {
	Recall(ctx context.Context, req hindsight.RecallRequest) (hindsight.RecallResponse, error)
}

// Transcript is the part of transcript.Store recall reads.
type Transcript interface {
	Since(ctx context.Context, roomID string, after int64, limit int) ([]transcript.Entry, error)
	LastSegmentBreak(ctx context.Context, roomID string) (int64, error)
}

// RecallOptions configures NewRecall.
type RecallOptions struct {
	Client     Searcher
	Transcript Transcript
	Roster     *roster.Roster
	MaxTokens  int
	Budget     hindsight.Budget
	Timeout    time.Duration // per recall; 0 means no extra timeout
	MinScore   float64       // results less relevant than this are dropped
	Window     int           // recent messages the query is built from (default 6)
	MaxQuery   int           // most characters in a query (default 800)
	Log        *slog.Logger
	Now        func() time.Time
}

// Recall builds the memory block for a bot turn.
//
// One Hindsight query is made per user message: the bots answering in the
// same chain share its results. Each bot's session gets a memory at most
// once (keyed by session id, so after /reset the new session can get it
// again), because Hermes keeps every earlier user message in the session.
type Recall struct {
	o       RecallOptions
	builder *contextbuild.Builder

	mu    sync.Mutex
	cache map[string]cached          // room id -> last query's results
	seen  map[string]map[string]bool // session id -> memory ids already given
}

type cached struct {
	key     int64 // seq of the newest user message the query was built for
	results []hindsight.RecallResult
}

// Types recalled: raw facts (Hindsight decides at extraction whether a fact
// is "world" or "experience") plus consolidated observations, with an
// observation replacing the facts it was built from. A new bank has no
// observations until Hindsight consolidates, so observation-only would
// return nothing at first. max_tokens bounds the cost either way.
var recallTypes = []string{"world", "experience", "observation"}

// NewRecall builds a Recall.
func NewRecall(o RecallOptions) *Recall {
	if o.Window <= 0 {
		o.Window = 6
	}
	if o.MaxQuery <= 0 {
		o.MaxQuery = 800
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Recall{
		o: o, builder: contextbuild.New(o.Roster),
		cache: map[string]cached{}, seen: map[string]map[string]bool{},
	}
}

// Block returns the memory block for one bot turn in roomID, or "" when
// there is nothing relevant and new for sessionID, or recall failed. It
// also returns the ids of the memories in the block: the caller passes them
// to Delivered once the turn reached Hermes, so a failed call offers them
// again on the retry.
func (r *Recall) Block(ctx context.Context, roomID, sessionID string) (string, []string) {
	log := r.o.Log.With("room", roomID, "session", sessionID)
	// Only messages since the last /reset: a query built from the chat the
	// user just reset would pull that same chat back in from its retained
	// summary. What came before still comes back when the new chat is about it.
	after, err := r.o.Transcript.LastSegmentBreak(ctx, roomID)
	if err != nil {
		log.Warn("memory: reading the last reset point failed; no recall this turn", "err", err)
		return "", nil
	}
	recent, err := r.o.Transcript.Since(ctx, roomID, after, r.o.Window)
	if err != nil {
		log.Warn("memory: reading recent messages failed; no recall this turn", "err", err)
		return "", nil
	}
	query := r.Query(recent)
	if query == "" {
		return "", nil
	}
	key := r.cacheKey(recent)

	r.mu.Lock()
	c, hit := r.cache[roomID]
	r.mu.Unlock()
	if !hit || c.key != key {
		c = cached{key: key, results: r.search(ctx, log, query)}
		r.mu.Lock()
		r.cache[roomID] = c
		r.mu.Unlock()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	given := r.seen[sessionID]
	var fresh []hindsight.RecallResult
	var ids []string
	for _, res := range c.results {
		if !given[res.ID] {
			fresh = append(fresh, res)
			ids = append(ids, res.ID)
		}
	}
	if len(fresh) == 0 {
		return "", nil
	}
	log.Info("memory: adding recollections to the turn", "count", len(fresh))
	return FormatBlock(r.o.Roster.User().Display, fresh), ids
}

// Delivered records that sessionID now holds the memories ids (its Hermes
// call succeeded), so later turns of that session don't repeat them. This
// is kept in memory only: after a restart a live session may get the same
// recollections once more.
func (r *Recall) Delivered(sessionID string, ids []string) {
	if len(ids) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	given := r.seen[sessionID]
	if given == nil {
		given = map[string]bool{}
		r.seen[sessionID] = given
	}
	for _, id := range ids {
		given[id] = true
	}
}

// search runs one bounded recall. Any failure yields no results.
func (r *Recall) search(ctx context.Context, log *slog.Logger, query string) []hindsight.RecallResult {
	if r.o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.o.Timeout)
		defer cancel()
	}
	start := r.o.Now()
	resp, err := r.o.Client.Recall(ctx, r.Request(query))
	took := r.o.Now().Sub(start).Round(time.Millisecond)
	switch {
	case errors.Is(err, hindsight.ErrBankNotFound):
		log.Debug("memory: the group bank does not exist yet (nothing retained so far)")
		return nil
	case err != nil:
		log.Warn("memory: recall failed; the turn goes ahead without memory", "err", err, "took", took)
		return nil
	}
	var kept []hindsight.RecallResult
	for _, res := range resp.Results {
		if strings.TrimSpace(res.Text) == "" || res.ID == "" {
			continue
		}
		if rel := res.Relevance(); rel >= 0 && rel < r.o.MinScore {
			continue
		}
		kept = append(kept, res)
	}
	log.Debug("memory: recall", "returned", len(resp.Results), "kept", len(kept), "took", took)
	return kept
}

// Request is the recall request for query.
func (r *Recall) Request(query string) hindsight.RecallRequest {
	return hindsight.RecallRequest{
		Query:              query,
		Types:              recallTypes,
		PreferObservations: true,
		Budget:             r.o.Budget,
		MaxTokens:          r.o.MaxTokens,
		QueryTimestamp:     r.o.Now().UTC().Format("2006-01-02T15:04:05"),
		Include:            hindsight.IncludeOptions{Entities: nil}, // no entity observations
	}
}

// Query renders recent messages as "Name: text" lines, newest last, cut from
// the front to at most MaxQuery characters. Silence markers are left out.
func (r *Recall) Query(recent []transcript.Entry) string {
	var lines []string
	for _, e := range recent {
		text := strings.Join(strings.Fields(r.builder.Rewrite(e.Body, e.FormattedBody)), " ")
		if text == "" || silence.Is(text) {
			continue
		}
		lines = append(lines, r.o.Roster.DisplayFor(e.Sender)+": "+text)
	}
	q := strings.Join(lines, "\n")
	if len(q) > r.o.MaxQuery {
		q = q[len(q)-r.o.MaxQuery:]
		if i := strings.IndexByte(q, '\n'); i >= 0 && i < len(q)-1 {
			q = q[i+1:] // start at a whole line
		}
		q = strings.ToValidUTF8(q, "")
	}
	return q
}

// cacheKey is the seq of the newest user message in recent (else the newest
// seq), so all bots answering one user message share one recall.
func (r *Recall) cacheKey(recent []transcript.Entry) int64 {
	user := r.o.Roster.User().MXID
	for i := len(recent) - 1; i >= 0; i-- {
		if recent[i].Sender == user {
			return recent[i].Seq
		}
	}
	if len(recent) == 0 {
		return 0
	}
	return recent[len(recent)-1].Seq
}

// FormatBlock renders memories as the block added to a turn's user message.
// Memory text is escaped like message text, so it cannot open or close a tag.
func FormatBlock(user string, results []hindsight.RecallResult) string {
	var b strings.Builder
	b.WriteString("Recollections from past group chats. They may be wrong or out of date, and " + user + "'s own words in this chat always win:\n")
	b.WriteString(`<memory source="past group chats">` + "\n")
	for _, res := range results {
		line := strings.Join(strings.Fields(res.Text), " ")
		if d := dateOf(res); d != "" {
			line += " (" + d + ")"
		}
		b.WriteString("- " + escape.Replace(line) + "\n")
	}
	b.WriteString("</memory>")
	return b.String()
}

var escape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func dateOf(r hindsight.RecallResult) string {
	for _, s := range []string{r.OccurredStart, r.MentionedAt} {
		if len(s) >= 10 {
			return s[:10]
		}
	}
	return ""
}
