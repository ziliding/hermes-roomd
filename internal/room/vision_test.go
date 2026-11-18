package room

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/ziliding/hermes-roomd/internal/matrixio"
	"github.com/ziliding/hermes-roomd/internal/media"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// userImage is an m.image from Alex, as the listener reports it.
func (h *harness) userImage(name, caption, mxc string) matrixio.Message {
	m := h.user(caption)
	m.Image = &matrixio.ImageRef{URL: mxc, MIME: "image/png", Name: name}
	return m
}

// runMsg handles m and runs its chain synchronously.
func (h *harness) runMsg(m matrixio.Message) {
	h.o.Handle(h.ctx, m)
	w := h.o.rooms[roomID]
	q := <-w.inbox
	w.state.UserSpoke()
	h.o.chain(h.ctx, w, q)
}

func dataURL(data []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
}

func TestPostedImageReachesEachBotOnce(t *testing.T) {
	h := newHarness(t, 2, func(bot string, n int) string { return "nice, from " + bot })
	pic := testPNG(t)
	h.media = map[string][]byte{"mxc://example.com/cat": pic}
	h.runMsg(h.userImage("cat.png", "Ada, what is this?", "mxc://example.com/cat"))

	if len(h.calls) != 2 {
		t.Fatalf("calls %+v", h.calls)
	}
	for i, bot := range []string{"ada", "cleo"} {
		c := h.calls[i]
		if c.bot != bot || len(c.images) != 1 || c.images[0] != dataURL(pic) {
			t.Fatalf("call %d: bot %s images %v", i, c.bot, c.images)
		}
		if !strings.Contains(c.delta, `<msg from="Alex">[image: cat.png] Ada, what is this?</msg>`) {
			t.Errorf("%s delta %q", bot, c.delta)
		}
		if !strings.Contains(c.raw, "The image posted above is attached") || !strings.HasSuffix(c.raw, "Image from Alex (cat.png):") {
			t.Errorf("%s input does not introduce the image:\n%s", bot, c.raw)
		}
	}
	if strings.Join(h.downloads, "|") != "ada: mxc://example.com/cat|cleo: mxc://example.com/cat" {
		t.Errorf("downloads %v (each bot downloads as itself)", h.downloads)
	}

	// The next turn doesn't send it again.
	h.runOne("Ada, and now?")
	if c := h.calls[2]; len(c.images) != 0 || strings.Contains(c.raw, "attached") {
		t.Fatalf("image resent: %+v", c)
	}
}

func TestImageWithoutCaptionIsNotACommand(t *testing.T) {
	h := newHarness(t, 1, func(bot string, n int) string { return "ok" })
	h.media = map[string][]byte{"mxc://example.com/r": testPNG(t)}
	m := h.user("")
	m.Image = &matrixio.ImageRef{URL: "mxc://example.com/r", Name: "/reset"} // a file named like a command
	h.runMsg(m)
	if len(h.calls) != 1 || len(h.calls[0].images) != 1 || len(h.notices) != 0 {
		t.Fatalf("calls %+v notices %v", h.calls, h.notices)
	}
}

func TestUnusableImagesBecomeNotes(t *testing.T) {
	h := newHarness(t, 1, func(bot string, n int) string { return "ok" })
	pic := testPNG(t)
	h.media = map[string][]byte{
		"mxc://example.com/text": []byte("just some text, not an image"),
		"mxc://example.com/big":  make([]byte, media.MaxFileBytes+1),
		"mxc://example.com/ok":   pic,
	}
	w := h.o.rooms[roomID]
	for _, m := range []matrixio.Message{
		h.userImage("gone.png", "", "mxc://example.com/missing"),
		h.userImage("notes.png", "", "mxc://example.com/text"),
		h.userImage("huge.png", "", "mxc://example.com/big"),
	} {
		h.o.Handle(h.ctx, m)
		<-w.inbox
	}
	h.runMsg(h.userImage("fine.png", "Ada?", "mxc://example.com/ok"))
	c := h.calls[0]
	if len(c.images) != 1 || c.images[0] != dataURL(pic) {
		t.Fatalf("images %v", c.images)
	}
	for _, name := range []string{"gone.png", "notes.png", "huge.png"} {
		if !strings.Contains(c.raw, "Image from Alex ("+name+") could not be loaded.") {
			t.Errorf("no note for %s:\n%s", name, c.raw)
		}
	}
}

func TestAtMostFourImagesPerTurn(t *testing.T) {
	h := newHarness(t, 1, func(bot string, n int) string { return "ok" })
	pic := testPNG(t)
	h.media = map[string][]byte{"mxc://example.com/p": pic}
	w := h.o.rooms[roomID]
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		h.o.Handle(h.ctx, h.userImage("p"+n+".png", "", "mxc://example.com/p"))
		<-w.inbox
	}
	h.runOne("Ada, which one?")
	c := h.calls[0]
	if len(c.images) != MaxImages || !strings.Contains(c.raw, "Image from Alex (p1.png) is not attached") || strings.Contains(c.raw, "(p2.png) is not") {
		t.Fatalf("images %d input:\n%s", len(c.images), c.raw)
	}
	if !strings.Contains(c.raw, "The 4 images posted above are attached") {
		t.Errorf("input:\n%s", c.raw)
	}
}

// An image addressed to a busy bot is not steered (steer input is text
// only): it reaches the bot, image attached, on its next turn.
func TestImageForBusyBotWaitsForItsNextTurn(t *testing.T) {
	release := make(chan struct{})
	h := newHarnessWith(t, harnessOpts{maxTurns: 1, backgroundAfter: 20 * time.Millisecond}, slowAda(release))
	pic := testPNG(t)
	h.media = map[string][]byte{"mxc://example.com/cat": pic}
	startBusyAda(t, h)
	runID := h.waitCalls(1)[0].run

	h.o.Handle(h.ctx, h.userImage("cat.png", "Ada, use this one", "mxc://example.com/cat"))
	time.Sleep(30 * time.Millisecond)
	if _, _, r, _, c := h.snap(); len(c) != 1 || len(r) != 0 || len(h.srv.Get(runID).Steers) != 0 {
		t.Fatalf("image steered or chained: calls %d reacts %v", len(c), r)
	}

	close(release)
	h.waitPosts(1)
	h.waitIdle()
	h.o.Handle(h.ctx, h.user("Ada, done?"))
	c := h.waitCalls(2)[1]
	if len(c.images) != 1 || c.images[0] != dataURL(pic) || !strings.Contains(c.delta, "[image: cat.png] Ada, use this one") {
		t.Fatalf("next call %+v", c)
	}
}

// Hermes refusing a run with images (413: body too large) doesn't cost the
// turn: roomd starts it again without them, and never retries them.
func TestRejectedImagesAreDropped(t *testing.T) {
	h := newHarness(t, 1, func(bot string, n int) string { return "ok" })
	h.media = map[string][]byte{"mxc://example.com/cat": testPNG(t)}
	h.srv.StartStatus = func(n int) int {
		if n == 1 {
			return 413
		}
		return 0
	}
	h.runMsg(h.userImage("cat.png", "Ada?", "mxc://example.com/cat"))
	if len(h.calls) != 1 || len(h.calls[0].images) != 0 || !strings.Contains(h.calls[0].raw, "Image from Alex (cat.png) could not be sent to the model.") {
		t.Fatalf("calls %+v", h.calls)
	}
	if len(h.posts) != 1 {
		t.Fatalf("posts %v", h.posts)
	}
}

// A run that fails while carrying images is retried on the next turn
// without them, so one image the model rejects can't jam the bot.
func TestFailedRunWithImagesRetriesWithoutThem(t *testing.T) {
	h := newHarness(t, 1, func(bot string, n int) string {
		if n == 1 {
			return "ERR"
		}
		return "ok"
	})
	h.media = map[string][]byte{"mxc://example.com/cat": testPNG(t)}
	h.runMsg(h.userImage("cat.png", "Ada?", "mxc://example.com/cat"))
	if len(h.calls) != 1 || len(h.calls[0].images) != 1 || len(h.posts) != 0 {
		t.Fatalf("calls %+v posts %v", h.calls, h.posts)
	}
	h.runOne("Ada, again?")
	c := h.calls[1]
	if len(c.images) != 0 || !strings.Contains(c.delta, "[image: cat.png] Ada?") || !strings.Contains(c.raw, "(cat.png) could not be sent to the model.") {
		t.Fatalf("retry %+v", c)
	}
	if len(h.posts) != 1 {
		t.Fatalf("posts %v", h.posts)
	}
}
