package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ziliding/hermes-roomd/internal/hindsight"
	"github.com/ziliding/hermes-roomd/internal/transcript"
)

type fakeWriter struct {
	items    []hindsight.MemoryItem
	fail     error
	exists   bool
	created  []hindsight.CreateBankRequest
	attempts int
}

func (w *fakeWriter) Retain(_ context.Context, items []hindsight.MemoryItem) (hindsight.RetainResponse, error) {
	w.attempts++
	if w.fail != nil {
		return hindsight.RetainResponse{}, w.fail
	}
	w.items = append(w.items, items...)
	return hindsight.RetainResponse{Success: true, ItemsCount: len(items)}, nil
}

func (w *fakeWriter) BankExists(context.Context) (bool, error) { return w.exists, nil }

func (w *fakeWriter) CreateBank(_ context.Context, req hindsight.CreateBankRequest) error {
	w.created = append(w.created, req)
	w.exists = true
	return nil
}

type fakeSummarizer struct {
	answer string
	err    error
	got    []Segment
}

func (f *fakeSummarizer) Summarize(_ context.Context, s Segment) (string, error) {
	f.got = append(f.got, s)
	if f.answer == "" {
		return "- Alex said hi.", f.err
	}
	return f.answer, f.err
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time       { return c.t }
func (c *clock) tick(d time.Duration) { c.t = c.t.Add(d) }

type rig struct {
	t     *testing.T
	store *transcript.Store
	w     *fakeWriter
	sum   *fakeSummarizer
	clk   *clock
	r     *Retainer
}

func newRig(t *testing.T, mode Mode) *rig {
	rg := &rig{
		t: t, store: openStore(t), w: &fakeWriter{}, sum: &fakeSummarizer{},
		clk: &clock{t: time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC)},
	}
	rg.r = rg.retainer(mode)
	// First pass on an empty room initializes the cursor at 0.
	rg.r.RunOnce(context.Background())
	return rg
}

func (rg *rig) retainer(mode Mode) *Retainer {
	return NewRetainer(RetainOptions{
		Store: rg.store, Writer: rg.w, Summarizer: rg.sum, Roster: testRoster(), Rooms: []string{room},
		Mode: mode, Idle: 30 * time.Minute, MinMessages: 3, Timeout: time.Second,
		BackoffMin: time.Minute, BackoffMax: 10 * time.Minute, Now: rg.clk.now,
	})
}

func (rg *rig) cursor() int64 {
	c, _, err := rg.store.RetainCursor(context.Background(), room)
	if err != nil {
		rg.t.Fatal(err)
	}
	return c
}

// chat adds a short conversation (Alex, Ada, Alex, Cleo) one minute apart.
func (rg *rig) chat() (first, last int64) {
	first = add(rg.t, rg.store, "@alex:z", "I'm moving to Lisbon in March", rg.clk.t)
	rg.clk.tick(time.Minute)
	add(rg.t, rg.store, "@ada:z", "Lisbon is the city of saudade-light", rg.clk.t)
	rg.clk.tick(time.Minute)
	add(rg.t, rg.store, "@alex:z", "I decided on Alfama", rg.clk.t)
	rg.clk.tick(time.Minute)
	last = add(rg.t, rg.store, "@cleo:z", "nice choice", rg.clk.t)
	return first, last
}

func TestRetainWaitsForIdleThenWritesOnce(t *testing.T) {
	rg := newRig(t, ModeSummary)
	ctx := context.Background()
	first, last := rg.chat()

	rg.clk.tick(29 * time.Minute)
	rg.r.RunOnce(ctx)
	if len(rg.w.items) != 0 || rg.cursor() != 0 {
		t.Fatal("wrote before the room was idle")
	}
	rg.clk.tick(time.Minute)
	rg.r.RunOnce(ctx)
	if len(rg.w.items) != 1 || rg.cursor() != last {
		t.Fatalf("items %d cursor %d, want 1 document and cursor %d", len(rg.w.items), rg.cursor(), last)
	}
	it := rg.w.items[0]
	if it.DocumentID != DocumentID(room, first) || it.UpdateMode != "replace" {
		t.Fatalf("item %+v", it)
	}
	if it.Metadata["source"] != "roomd" || it.Metadata["participants"] != "Alex, Ada, Cleo" ||
		it.Metadata["start"] != "2026-11-18T14:00:00Z" || it.Metadata["end"] != "2026-11-18T14:03:00Z" || it.Metadata["room"] != room {
		t.Fatalf("metadata %v", it.Metadata)
	}
	if it.Timestamp != "2026-11-18T14:00:00Z" || !strings.Contains(strings.Join(it.Tags, ","), "source:roomd") {
		t.Fatalf("timestamp %s tags %v", it.Timestamp, it.Tags)
	}
	if !strings.HasPrefix(it.Content, "Notes from a group chat between Alex, Ada, Cleo on 2026-11-18, 14:00–14:03 UTC:\n- Alex said hi.") {
		t.Fatalf("content %q", it.Content)
	}
	if len(rg.w.created) != 1 || !strings.Contains(rg.w.created[0].RetainMission, "opinion") {
		t.Fatalf("bank not created with a retain mission: %+v", rg.w.created)
	}
	seg := rg.sum.got[0]
	if !strings.Contains(seg.Messages, `<msg from="Ada">Lisbon is the city of saudade-light</msg>`) {
		t.Fatalf("summarizer input %q", seg.Messages)
	}

	rg.clk.tick(time.Hour)
	rg.r.RunOnce(ctx)
	if len(rg.w.items) != 1 {
		t.Fatal("the same conversation was written twice")
	}
}

func TestRetainSkips(t *testing.T) {
	tests := []struct {
		name    string
		msgs    []string // senders
		summary string
	}{
		{"too few messages", []string{"@alex:z", "@ada:z"}, ""},
		{"no message from Alex", []string{"@ada:z", "@cleo:z", "@ada:z", "@cleo:z"}, ""},
		{"silence does not count", []string{"@alex:z", "@ada:z|[SILENT]", "@cleo:z|NO_REPLY"}, ""},
		{"summarizer finds nothing", []string{"@alex:z", "@ada:z", "@alex:z"}, Nothing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rg := newRig(t, ModeSummary)
			rg.sum.answer = tt.summary
			var last int64
			for _, m := range tt.msgs {
				sender, body, _ := strings.Cut(m, "|")
				if body == "" {
					body = "hello"
				}
				last = add(t, rg.store, sender, body, rg.clk.t)
			}
			rg.clk.tick(time.Hour)
			rg.r.RunOnce(context.Background())
			if len(rg.w.items) != 0 {
				t.Fatalf("wrote %d documents, want none", len(rg.w.items))
			}
			if rg.cursor() != last {
				t.Fatalf("cursor %d, want %d (a skipped conversation is still done)", rg.cursor(), last)
			}
		})
	}
}

func TestRetainFailureKeepsCursorAndBacksOff(t *testing.T) {
	rg := newRig(t, ModeSummary)
	ctx := context.Background()
	first, _ := rg.chat()
	rg.w.fail = errors.New("hindsight down")
	rg.clk.tick(time.Hour)

	rg.r.RunOnce(ctx)
	if rg.cursor() != 0 || rg.w.attempts != 1 {
		t.Fatalf("cursor %d attempts %d after a failure", rg.cursor(), rg.w.attempts)
	}
	rg.clk.tick(30 * time.Second)
	rg.r.RunOnce(ctx)
	if rg.w.attempts != 1 {
		t.Fatal("retried before the backoff elapsed")
	}
	rg.clk.tick(31 * time.Second) // 1m backoff passed
	rg.r.RunOnce(ctx)
	if rg.w.attempts != 2 {
		t.Fatalf("attempts %d, want a retry after 1m", rg.w.attempts)
	}
	rg.clk.tick(time.Minute + time.Second) // backoff doubled to 2m: not yet
	rg.r.RunOnce(ctx)
	if rg.w.attempts != 2 {
		t.Fatal("backoff did not double")
	}

	// More messages arrive meanwhile; the retry keeps the same document id.
	last := add(t, rg.store, "@alex:z", "also, I got the job", rg.clk.t)
	rg.clk.tick(time.Hour)
	rg.w.fail = nil
	rg.r.RunOnce(ctx)
	if len(rg.w.items) != 1 || rg.w.items[0].DocumentID != DocumentID(room, first) || rg.cursor() != last {
		t.Fatalf("items %+v cursor %d", rg.w.items, rg.cursor())
	}

	// The summarizer failing is a failure too.
	rg.chat()
	rg.sum.err = errors.New("model down")
	rg.clk.tick(time.Hour)
	rg.r.RunOnce(ctx)
	if rg.cursor() != last {
		t.Fatal("a failed summary moved the cursor")
	}
}

func TestRetainResetEndsSegment(t *testing.T) {
	rg := newRig(t, ModeTranscript)
	ctx := context.Background()
	first, brk := rg.chat()
	// !reset arrives: the conversation so far ends here.
	if err := rg.store.AddSegmentBreak(ctx, room, brk); err != nil {
		t.Fatal(err)
	}
	rg.clk.tick(time.Minute)
	second := add(t, rg.store, "@alex:z", "new topic: cats", rg.clk.t)

	// The pre-reset segment is closed and written at once, without waiting.
	rg.r.RunOnce(ctx)
	if len(rg.w.items) != 1 || rg.cursor() != brk {
		t.Fatalf("items %d cursor %d, want the pre-reset segment written up to %d", len(rg.w.items), rg.cursor(), brk)
	}
	if it := rg.w.items[0]; it.DocumentID != DocumentID(room, first) || strings.Contains(it.Content, "cats") ||
		!strings.Contains(it.Content, "Alex: I decided on Alfama\nCleo: nice choice") {
		t.Fatalf("pre-reset document %+v", it)
	}

	add(t, rg.store, "@cleo:z", "cats are great", rg.clk.t)
	last := add(t, rg.store, "@alex:z", "I have two", rg.clk.t)
	rg.clk.tick(time.Hour)
	rg.r.RunOnce(ctx)
	if len(rg.w.items) != 2 || rg.cursor() != last {
		t.Fatalf("items %d cursor %d", len(rg.w.items), rg.cursor())
	}
	if it := rg.w.items[1]; it.DocumentID != DocumentID(room, second) || strings.Contains(it.Content, "Lisbon") {
		t.Fatalf("post-reset document %+v", it)
	}
}

func TestRetainCatchesUpAfterRestart(t *testing.T) {
	rg := newRig(t, ModeSummary)
	_, last := rg.chat()
	rg.clk.tick(2 * time.Hour) // roomd was down while the room went quiet
	fresh := rg.retainer(ModeSummary)
	fresh.RunOnce(context.Background())
	if len(rg.w.items) != 1 || rg.cursor() != last {
		t.Fatalf("items %d cursor %d after restart", len(rg.w.items), rg.cursor())
	}
}

func TestRetainFirstRunStartsAtNewest(t *testing.T) {
	rg := &rig{t: t, store: openStore(t), w: &fakeWriter{}, sum: &fakeSummarizer{}, clk: &clock{t: time.Now()}}
	_, last := rg.chat()
	rg.clk.tick(time.Hour)
	rg.retainer(ModeSummary).RunOnce(context.Background())
	if len(rg.w.items) != 0 || rg.cursor() != last {
		t.Fatalf("items %d cursor %d: history from before memory was enabled must not be written", len(rg.w.items), rg.cursor())
	}
}

func TestRetainExistingBankIsNotTouched(t *testing.T) {
	rg := newRig(t, ModeSummary)
	rg.w.exists = true
	rg.chat()
	rg.clk.tick(time.Hour)
	rg.r.RunOnce(context.Background())
	if len(rg.w.created) != 0 || len(rg.w.items) != 1 {
		t.Fatalf("created %d items %d", len(rg.w.created), len(rg.w.items))
	}
}

func TestDocumentIDStable(t *testing.T) {
	tests := []struct {
		room string
		seq  int64
		want string
	}{
		{"!r:z", 1, "roomd-" + roomHash("!r:z") + "-s1"},
		{"!r:z", 42, "roomd-" + roomHash("!r:z") + "-s42"},
	}
	for _, tt := range tests {
		if got := DocumentID(tt.room, tt.seq); got != tt.want || got != DocumentID(tt.room, tt.seq) {
			t.Errorf("DocumentID(%q, %d) = %q, want %q", tt.room, tt.seq, got, tt.want)
		}
	}
	if DocumentID("!a:z", 1) == DocumentID("!b:z", 1) {
		t.Error("different rooms share a document id")
	}
	if h := roomHash("!r:z"); len(h) != 12 {
		t.Errorf("room hash %q", h)
	}
}
