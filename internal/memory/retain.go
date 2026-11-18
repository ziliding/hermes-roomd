package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ziliding/hermes-roomd/internal/contextbuild"
	"github.com/ziliding/hermes-roomd/internal/hindsight"
	"github.com/ziliding/hermes-roomd/internal/roster"
	"github.com/ziliding/hermes-roomd/internal/silence"
	"github.com/ziliding/hermes-roomd/internal/transcript"
)

// Mode is how a conversation is written.
type Mode string

const (
	ModeSummary    Mode = "summary"
	ModeTranscript Mode = "transcript"
)

// RetainStore is the part of transcript.Store the retainer uses.
type RetainStore interface {
	RetainCursor(ctx context.Context, roomID string) (int64, bool, error)
	SetRetainCursor(ctx context.Context, roomID string, seq int64) error
	After(ctx context.Context, roomID string, after, upTo int64, limit int) ([]transcript.Entry, error)
	LatestSeq(ctx context.Context, roomID string) (int64, error)
	LatestTS(ctx context.Context, roomID string) (int64, error)
	NextSegmentBreak(ctx context.Context, roomID string, after int64) (int64, error)
}

// Writer is the part of the Hindsight client the retainer uses.
type Writer interface {
	Retain(ctx context.Context, items []hindsight.MemoryItem) (hindsight.RetainResponse, error)
	BankExists(ctx context.Context) (bool, error)
	CreateBank(ctx context.Context, req hindsight.CreateBankRequest) error
}

// Summarizer turns a segment into memory notes (or Nothing).
type Summarizer interface {
	Summarize(ctx context.Context, seg Segment) (string, error)
}

// RetainOptions configures NewRetainer.
type RetainOptions struct {
	Store       RetainStore
	Writer      Writer
	Summarizer  Summarizer // required in ModeSummary
	Roster      *roster.Roster
	Rooms       []string
	Mode        Mode
	Idle        time.Duration // quiet time before an open conversation is written
	MinMessages int           // shorter conversations are skipped
	Timeout     time.Duration // per summarizer call and per retain call
	MaxSegment  int           // most messages in one document (default 300)
	BackoffMin  time.Duration // first retry delay after a failure (default 1m)
	BackoffMax  time.Duration // longest retry delay (default 1h)
	Log         *slog.Logger
	Now         func() time.Time
}

// Retainer writes one document per finished conversation into the group bank.
//
// A conversation (segment) is the run of messages after the room's retain
// cursor, up to the next /reset break or, if there is none, up to the newest
// message. A segment ended by /reset is written at once; an open one is
// written after the room has been quiet for Idle. Segments with fewer than
// MinMessages messages or no message from the user are skipped. Either way
// the cursor (in SQLite) moves past the segment, so nothing is written
// twice, and anything left over when roomd stops is caught up on the next
// start. A failed write leaves the cursor where it is and is retried with
// exponential backoff; the document id depends only on the room and the
// segment's first message, so a retry replaces rather than duplicates.
type Retainer struct {
	o         RetainOptions
	builder   *contextbuild.Builder
	bankReady bool

	mu    sync.Mutex
	retry map[string]backoff // room id -> retry state
}

type backoff struct {
	fails int
	next  time.Time
}

// NewRetainer builds a Retainer.
func NewRetainer(o RetainOptions) *Retainer {
	if o.MaxSegment <= 0 {
		o.MaxSegment = 300
	}
	if o.BackoffMin <= 0 {
		o.BackoffMin = time.Minute
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = time.Hour
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Retainer{o: o, builder: contextbuild.New(o.Roster), retry: map[string]backoff{}}
}

// Run calls RunOnce now (to catch up after a restart) and then every
// interval until ctx is done.
func (r *Retainer) Run(ctx context.Context, interval time.Duration) { RunRetainers(ctx, interval, r) }

// RunRetainers runs every retainer's pass now and then every interval, one
// after the other on one goroutine, until ctx is done. With no retainers it
// just waits for ctx.
func RunRetainers(ctx context.Context, interval time.Duration, rs ...*Retainer) {
	if len(rs) == 0 {
		<-ctx.Done()
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		for _, r := range rs {
			r.RunOnce(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce writes every room's finished segments that are due.
func (r *Retainer) RunOnce(ctx context.Context) {
	for _, room := range r.o.Rooms {
		if ctx.Err() != nil {
			return
		}
		r.room(ctx, room)
	}
}

// maxSegmentsPerPass bounds one room's work per pass (catch-up after a long
// outage proceeds over several passes).
const maxSegmentsPerPass = 10

func (r *Retainer) room(ctx context.Context, roomID string) {
	log := r.o.Log.With("room", roomID)
	r.mu.Lock()
	b := r.retry[roomID]
	r.mu.Unlock()
	if r.o.Now().Before(b.next) {
		return
	}
	cursor, ok, err := r.o.Store.RetainCursor(ctx, roomID)
	if err != nil {
		log.Error("memory: reading retain cursor", "err", err)
		return
	}
	if !ok {
		// First run with memory on: start from the newest message. Older
		// history is not backfilled (see README to change that).
		latest, err := r.o.Store.LatestSeq(ctx, roomID)
		if err != nil {
			log.Error("memory: reading latest seq", "err", err)
			return
		}
		if err := r.o.Store.SetRetainCursor(ctx, roomID, latest); err != nil {
			log.Error("memory: initializing retain cursor", "err", err)
			return
		}
		log.Info("memory: retain cursor starts at the newest message; earlier history is not written", "seq", latest)
		return
	}

	for range maxSegmentsPerPass {
		next, done := r.segment(ctx, log, roomID, cursor)
		if done {
			return
		}
		cursor = next
	}
}

// segment handles the segment starting after cursor. It returns the new
// cursor, or done=true when there is nothing more to do in this pass.
func (r *Retainer) segment(ctx context.Context, log *slog.Logger, roomID string, cursor int64) (int64, bool) {
	brk, err := r.o.Store.NextSegmentBreak(ctx, roomID, cursor)
	if err != nil {
		log.Error("memory: reading segment breaks", "err", err)
		return cursor, true
	}
	entries, err := r.o.Store.After(ctx, roomID, cursor, brk, r.o.MaxSegment)
	if err != nil {
		log.Error("memory: reading transcript", "err", err)
		return cursor, true
	}
	if len(entries) == 0 {
		if brk > 0 { // an empty segment before a /reset break: step over it
			return r.advance(ctx, log, roomID, brk)
		}
		return cursor, true
	}
	end := entries[len(entries)-1].Seq
	closed := len(entries) == r.o.MaxSegment
	if brk > 0 {
		closed, end = true, max(end, brk)
		if len(entries) == r.o.MaxSegment {
			end = entries[len(entries)-1].Seq // more of this segment follows in the next document
		}
	}
	if !closed {
		latest, err := r.o.Store.LatestTS(ctx, roomID)
		if err != nil {
			log.Error("memory: reading latest message time", "err", err)
			return cursor, true
		}
		if quiet := r.o.Now().Sub(time.UnixMilli(latest)); quiet < r.o.Idle {
			return cursor, true // still going
		}
	}

	if why := r.skipReason(entries); why != "" {
		log.Info("memory: conversation not written", "why", why, "messages", len(entries), "from", entries[0].Seq, "to", end)
		return r.advance(ctx, log, roomID, end)
	}
	if err := r.write(ctx, log, roomID, entries); err != nil {
		r.fail(log, roomID, err)
		return cursor, true
	}
	r.mu.Lock()
	delete(r.retry, roomID)
	r.mu.Unlock()
	return r.advance(ctx, log, roomID, end)
}

func (r *Retainer) advance(ctx context.Context, log *slog.Logger, roomID string, seq int64) (int64, bool) {
	if err := r.o.Store.SetRetainCursor(ctx, roomID, seq); err != nil {
		log.Error("memory: moving retain cursor", "err", err)
		return seq, true
	}
	return seq, false
}

func (r *Retainer) fail(log *slog.Logger, roomID string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.retry[roomID]
	delay := r.o.BackoffMin << min(b.fails, 20)
	if delay > r.o.BackoffMax || delay <= 0 {
		delay = r.o.BackoffMax
	}
	b.fails++
	b.next = r.o.Now().Add(delay)
	r.retry[roomID] = b
	log.Warn("memory: writing the conversation failed; will retry", "err", err, "attempt", b.fails, "retry_in", delay)
}

// skipReason says why a segment is not worth a document, or "".
func (r *Retainer) skipReason(entries []transcript.Entry) string {
	user := r.o.Roster.User().MXID
	n, fromUser := 0, false
	for _, e := range entries {
		if silence.Is(e.Body) {
			continue
		}
		n++
		fromUser = fromUser || e.Sender == user
	}
	switch {
	case !fromUser:
		return "no message from " + r.o.Roster.User().Display
	case n < r.o.MinMessages:
		return fmt.Sprintf("fewer than %d messages", r.o.MinMessages)
	}
	return ""
}

func (r *Retainer) write(ctx context.Context, log *slog.Logger, roomID string, entries []transcript.Entry) error {
	if err := r.ensureBank(ctx, log); err != nil {
		return err
	}
	item, skip, err := r.Item(ctx, roomID, entries)
	if err != nil {
		return err
	}
	if skip {
		log.Info("memory: the summarizer found nothing worth keeping", "messages", len(entries), "from", entries[0].Seq)
		return nil
	}
	cctx, cancel := r.timeout(ctx)
	defer cancel()
	start := r.o.Now()
	if _, err := r.o.Writer.Retain(cctx, []hindsight.MemoryItem{item}); err != nil {
		return err
	}
	log.Info("memory: conversation written to the group bank", "document", item.DocumentID, "mode", r.o.Mode,
		"messages", len(entries), "chars", len(item.Content), "took", r.o.Now().Sub(start).Round(time.Millisecond))
	return nil
}

// Item builds the document for a segment. skip is true when the summarizer
// answered Nothing.
func (r *Retainer) Item(ctx context.Context, roomID string, entries []transcript.Entry) (hindsight.MemoryItem, bool, error) {
	first, last := entries[0], entries[len(entries)-1]
	start, end := time.UnixMilli(first.TS).UTC(), time.UnixMilli(last.TS).UTC()
	participants := r.participants(entries)
	when := fmt.Sprintf("on %s, %s–%s UTC", start.Format("2006-01-02"), start.Format("15:04"), end.Format("15:04"))

	var content string
	switch r.o.Mode {
	case ModeTranscript:
		content = fmt.Sprintf("Transcript of a group chat between %s %s:\n%s",
			strings.Join(participants, ", "), when, r.TranscriptText(entries))
	default:
		cctx, cancel := r.timeout(ctx)
		defer cancel()
		notes, err := r.o.Summarizer.Summarize(cctx, Segment{
			Start: start, End: end, Participants: participants, Messages: r.builder.Build("", entries),
		})
		if err != nil {
			return hindsight.MemoryItem{}, false, err
		}
		if notes == Nothing {
			return hindsight.MemoryItem{}, true, nil
		}
		content = fmt.Sprintf("Notes from a group chat between %s %s:\n%s", strings.Join(participants, ", "), when, notes)
	}
	user := r.o.Roster.User().Display
	return hindsight.MemoryItem{
		Content:   content,
		Timestamp: start.Format(time.RFC3339),
		Context: "group chat between " + user + " (the human) and AI characters; only " + user +
			"'s own words are about " + user + ", and what a character says is that character's opinion",
		DocumentID: DocumentID(roomID, first.Seq),
		Tags:       []string{"source:roomd", "room:" + roomHash(roomID)},
		UpdateMode: "replace",
		Metadata: map[string]string{
			"source":        "roomd",
			"room":          roomID,
			"participants":  strings.Join(participants, ", "),
			"start":         start.Format(time.RFC3339),
			"end":           end.Format(time.RFC3339),
			"mode":          string(r.o.Mode),
			"first_seq":     strconv.FormatInt(first.Seq, 10),
			"last_seq":      strconv.FormatInt(last.Seq, 10),
			"message_count": strconv.Itoa(len(entries)),
		},
	}, false, nil
}

// TranscriptText renders a segment as plain "Name: text" lines (mentions
// rewritten, silence dropped).
func (r *Retainer) TranscriptText(entries []transcript.Entry) string {
	var lines []string
	for _, e := range entries {
		text := r.builder.Rewrite(e.Body, e.FormattedBody)
		if text == "" || silence.Is(text) {
			continue
		}
		lines = append(lines, r.o.Roster.DisplayFor(e.Sender)+": "+text)
	}
	return strings.Join(lines, "\n")
}

// participants lists who spoke, the user first, then bots in roster order.
func (r *Retainer) participants(entries []transcript.Entry) []string {
	spoke := map[string]bool{}
	for _, e := range entries {
		spoke[e.Sender] = true
	}
	out := []string{r.o.Roster.User().Display}
	for _, p := range r.o.Roster.Bots() {
		if spoke[p.MXID] {
			out = append(out, p.Display)
		}
	}
	return out
}

// ensureBank creates the group bank, with missions that steer Hindsight's
// own fact extraction, the first time it is needed. An existing bank is
// never touched, so hand-made changes to it survive.
func (r *Retainer) ensureBank(ctx context.Context, log *slog.Logger) error {
	if r.bankReady {
		return nil
	}
	cctx, cancel := r.timeout(ctx)
	defer cancel()
	ok, err := r.o.Writer.BankExists(cctx)
	if err != nil {
		return fmt.Errorf("checking the group bank: %w", err)
	}
	if !ok {
		if err := r.o.Writer.CreateBank(cctx, BankMissions(r.o.Roster)); err != nil {
			return fmt.Errorf("creating the group bank: %w", err)
		}
		log.Info("memory: created the group bank")
	}
	r.bankReady = true
	return nil
}

// BankMissions are set once, when roomd creates the group bank.
func BankMissions(r *roster.Roster) hindsight.CreateBankRequest {
	user := r.User().Display
	var names []string
	for _, p := range r.Bots() {
		names = append(names, p.Display)
	}
	chars := strings.Join(names, ", ")
	return hindsight.CreateBankRequest{
		RetainMission: fmt.Sprintf("These documents are notes and transcripts of group chats between %[1]s (the human user) and AI characters (%[2]s), written by roomd. "+
			"Extract facts about %[1]s only from %[1]s's own words: what %[1]s said, asked, decided, planned, likes or dislikes, plus concrete events. "+
			"Anything a character says is that character's opinion or suggestion: keep it attributed to that character, never as a fact about %[1]s or the world. "+
			"Never store terms, nicknames or concepts the characters invented as facts.", user, chars),
		ReflectMission: fmt.Sprintf("The shared memory of past group chats between %[1]s and AI characters (%[2]s). %[1]s's own words are authoritative; the characters' statements are opinions.", user, chars),
	}
}

func (r *Retainer) timeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.o.Timeout > 0 {
		return context.WithTimeout(ctx, r.o.Timeout)
	}
	return context.WithCancel(ctx)
}

// DocumentID is the stable id of the document for the segment that starts
// at firstSeq in roomID: a retry (even after more messages joined the
// segment) replaces the same document instead of adding a second one.
func DocumentID(roomID string, firstSeq int64) string {
	return fmt.Sprintf("roomd-%s-s%d", roomHash(roomID), firstSeq)
}

func roomHash(roomID string) string {
	sum := sha256.Sum256([]byte(roomID))
	return hex.EncodeToString(sum[:])[:12]
}
