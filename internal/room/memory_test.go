package room

import (
	"context"
	"strings"
	"sync"
	"testing"
)

type fakeMemory struct {
	mu        sync.Mutex
	block     string
	calls     []string // session ids
	delivered []string // session ids
}

func (m *fakeMemory) Block(_ context.Context, _, sid string) (string, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, sid)
	if m.block == "" {
		return "", nil
	}
	return m.block, []string{"m1"}
}

func (m *fakeMemory) Delivered(sid string, ids []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(ids) > 0 {
		m.delivered = append(m.delivered, sid)
	}
}

const memBlock = "Recollections from past group chats. They may be wrong or out of date, and Alex's own words in this chat always win:\n" +
	"<memory source=\"past group chats\">\n- Alex said they prefer tea.\n</memory>"

func TestMemoryBlockGoesIntoUserMessageOnly(t *testing.T) {
	h := newHarness(t, 1, func(bot string, n int) string { return "reply " + bot })
	mem := &fakeMemory{block: memBlock}
	h.o.rooms[roomID].memory = mem
	h.runOne("Cleo, tea or coffee?")
	h.runOne("Ada, and you?")

	if len(h.calls) != 4 || len(mem.calls) != 4 {
		t.Fatalf("hermes calls %d, memory calls %d; every called turn asks for memory", len(h.calls), len(mem.calls))
	}
	for i, c := range h.calls {
		if c.system != "SYS-"+c.bot {
			t.Fatalf("call %d: system message changed: %q", i, c.system)
		}
		if strings.Contains(c.system, "memory") {
			t.Fatalf("call %d: memory leaked into the system message", i)
		}
		if mem.calls[i] != c.session {
			t.Fatalf("call %d: memory asked for session %q, turn used %q", i, mem.calls[i], c.session)
		}
		mi, ni := strings.Index(c.raw, "</memory>"), strings.Index(c.raw, "<messages>")
		if !strings.HasPrefix(c.raw, memBlock+"\n\n") || mi < 0 || ni < mi {
			t.Fatalf("call %d: memory block must come first, before the messages:\n%s", i, c.raw)
		}
		if !strings.HasSuffix(c.raw, "don't write messages for anyone else.") {
			t.Fatalf("call %d: frame no longer ends with the reply instruction", i)
		}
	}
	if !strings.Contains(h.calls[0].delta, `<msg from="Alex">Cleo, tea or coffee?</msg>`) {
		t.Fatalf("delta %q", h.calls[0].delta)
	}
}

func TestNoMemoryBlockWhenNothingRecalled(t *testing.T) {
	h := newHarness(t, 0, func(bot string, n int) string { return "reply" })
	h.o.rooms[roomID].memory = &fakeMemory{}
	h.runOne("Cleo, hi")
	if raw := h.calls[0].raw; strings.Contains(raw, "memory") || !strings.HasPrefix(raw, "New messages in the group chat") {
		t.Fatalf("frame without recollections changed: %q", raw)
	}
}

func TestMemoryOffFrameUnchanged(t *testing.T) {
	if FrameMemory("d", "Cleo", "", "") != Frame("d", "Cleo") {
		t.Fatal("an empty memory block changes the frame")
	}
}

func TestResetRecordsSegmentBreak(t *testing.T) {
	h := newHarness(t, 1, func(bot string, n int) string { return "reply " + bot })
	h.runOne("Cleo, hello")
	upTo := h.latestSeq()
	h.runCmd(h.user("!reset"))
	brk, err := h.o.store.NextSegmentBreak(context.Background(), roomID, 0)
	if err != nil || brk != upTo {
		t.Fatalf("segment break %d (err %v), want %d", brk, err, upTo)
	}
}

func TestMemoryDeliveredOnlyAfterSuccessfulCall(t *testing.T) {
	fail := true
	h := newHarness(t, 0, func(bot string, n int) string {
		if fail {
			return "ERR"
		}
		return "reply"
	})
	mem := &fakeMemory{block: memBlock}
	h.o.rooms[roomID].memory = mem
	h.runOne("Cleo, tea?")
	if len(mem.delivered) != 0 {
		t.Fatal("memories marked delivered although the Hermes call failed")
	}
	fail = false
	h.runOne("Cleo, tea?")
	if len(h.calls) != 2 || !strings.HasPrefix(h.calls[1].raw, memBlock) {
		t.Fatalf("the retry must carry the block again: %+v", h.calls)
	}
	if len(mem.delivered) != 1 || mem.delivered[0] != h.calls[1].session {
		t.Fatalf("delivered %v", mem.delivered)
	}
}
