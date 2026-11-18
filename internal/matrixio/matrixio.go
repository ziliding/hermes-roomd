// Package matrixio is roomd's only contact with Matrix: one mautrix client per
// bot (roomd's own device/token for that account, never the Hermes gateway's)
// for sending, and a listener that syncs as one of them and turns room events
// into Messages.
package matrixio

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ziliding/hermes-roomd/internal/media"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Message is a room text message. Sender always comes from the event.
type Message struct {
	RoomID        string
	EventID       string
	Sender        string
	Body          string
	FormattedBody string
	Mentions      []string // m.mentions.user_ids
	TS            int64    // origin_server_ts, ms
	// Control marks roomd's own status notices (e.g. the /reset
	// acknowledgement). They are never recorded as conversation.
	Control bool
}

// ControlKey is the custom content key roomd puts on its own status notices,
// so the listener can tell them apart from bot speech even when bots post
// replies as m.notice too.
const ControlKey = "roomd.control"

// Sender posts as one bot.
type Sender struct {
	cli     *mautrix.Client
	msgType event.MessageType
}

// NewSender creates the bot's client and verifies the token belongs to mxid.
func NewSender(ctx context.Context, homeserver, mxid, token, msgType string) (*Sender, error) {
	cli, err := mautrix.NewClient(homeserver, id.UserID(mxid), token)
	if err != nil {
		return nil, err
	}
	who, err := cli.Whoami(ctx)
	if err != nil {
		return nil, fmt.Errorf("whoami for %s: %w", mxid, err)
	}
	if string(who.UserID) != mxid {
		return nil, fmt.Errorf("token for %s belongs to %s", mxid, who.UserID)
	}
	return &Sender{cli: cli, msgType: event.MessageType(msgType)}, nil
}

// Send posts text as a plain message with an empty m.mentions, so a reply
// never pings anyone. It returns the new event id.
func (s *Sender) Send(ctx context.Context, roomID, text string) (string, error) {
	resp, err := s.cli.SendMessageEvent(ctx, id.RoomID(roomID), event.EventMessage, &event.MessageEventContent{
		MsgType:  s.msgType,
		Body:     text,
		Mentions: &event.Mentions{},
	})
	if err != nil {
		return "", err
	}
	return string(resp.EventID), nil
}

// SendReply posts text like Send, as a Matrix reply (m.in_reply_to) to the
// event inReplyTo.
func (s *Sender) SendReply(ctx context.Context, roomID, text, inReplyTo string) (string, error) {
	resp, err := s.cli.SendMessageEvent(ctx, id.RoomID(roomID), event.EventMessage, &event.MessageEventContent{
		MsgType:   s.msgType,
		Body:      text,
		Mentions:  &event.Mentions{},
		RelatesTo: &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: id.EventID(inReplyTo)}},
	})
	if err != nil {
		return "", err
	}
	return string(resp.EventID), nil
}

// SendImage uploads img to the homeserver's media repository as this bot
// and posts it as an m.image (a reply to inReplyTo when that is set).
func (s *Sender) SendImage(ctx context.Context, roomID string, img media.Image, inReplyTo string) (string, error) {
	up, err := s.cli.UploadBytesWithName(ctx, img.Data, img.MIME, img.Filename())
	if err != nil {
		return "", fmt.Errorf("uploading image: %w", err)
	}
	content := &event.MessageEventContent{
		MsgType:  event.MsgImage,
		Body:     img.Body(),
		FileName: img.Filename(),
		URL:      up.ContentURI.CUString(),
		Info:     &event.FileInfo{MimeType: img.MIME, Size: len(img.Data), Width: img.Width, Height: img.Height},
		Mentions: &event.Mentions{},
	}
	if inReplyTo != "" {
		content.RelatesTo = &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: id.EventID(inReplyTo)}}
	}
	resp, err := s.cli.SendMessageEvent(ctx, id.RoomID(roomID), event.EventMessage, content)
	if err != nil {
		return "", err
	}
	return string(resp.EventID), nil
}

// React annotates eventID with key (an m.reaction).
func (s *Sender) React(ctx context.Context, roomID, eventID, key string) error {
	_, err := s.cli.SendReaction(ctx, id.RoomID(roomID), id.EventID(eventID), key)
	return err
}

// Typing shows (or clears) this bot as typing in the room for up to timeout.
func (s *Sender) Typing(ctx context.Context, roomID string, typing bool, timeout time.Duration) error {
	_, err := s.cli.UserTyping(ctx, id.RoomID(roomID), typing, timeout)
	return err
}

// SendNotice posts roomd's own status text (not bot speech) as an m.notice
// tagged with ControlKey, so the listener drops its echo instead of recording
// it as something this bot said.
func (s *Sender) SendNotice(ctx context.Context, roomID, text string) (string, error) {
	resp, err := s.cli.SendMessageEvent(ctx, id.RoomID(roomID), event.EventMessage, map[string]any{
		"msgtype":    event.MsgNotice,
		"body":       text,
		"m.mentions": map[string]any{},
		ControlKey:   true,
	})
	if err != nil {
		return "", err
	}
	return string(resp.EventID), nil
}

// Client exposes the underlying mautrix client (the listener reuses one).
func (s *Sender) Client() *mautrix.Client { return s.cli }

// CheckRooms fails if a configured room is end-to-end encrypted (roomd cannot
// read or send encrypted messages) or the listener is not joined to it.
func CheckRooms(ctx context.Context, cli *mautrix.Client, rooms []string) error {
	var errs []error
	for _, r := range rooms {
		var enc event.EncryptionEventContent
		err := cli.StateEvent(ctx, id.RoomID(r), event.StateEncryption, "", &enc)
		switch {
		case err == nil && enc.Algorithm != "":
			errs = append(errs, fmt.Errorf("room %s is end-to-end encrypted (%s); roomd only works in unencrypted rooms, and encryption cannot be turned off, so create a new room with encryption disabled", r, enc.Algorithm))
		case err == nil, errors.Is(err, mautrix.MNotFound):
			// unencrypted
		case errors.Is(err, mautrix.MForbidden):
			errs = append(errs, fmt.Errorf("room %s: %s is not joined (or the room id is wrong): %w", r, cli.UserID, err))
		default:
			errs = append(errs, fmt.Errorf("room %s: checking encryption: %w", r, err))
		}
	}
	return errors.Join(errs...)
}

// Listen syncs on cli, filtered to rooms, and calls handle for every text
// message sent after startAt. Edits (m.replace) and non-text msgtypes are
// dropped. It blocks until ctx is done.
func Listen(ctx context.Context, cli *mautrix.Client, rooms []string, startAt time.Time, log *slog.Logger, handle func(Message)) error {
	want := map[id.RoomID]bool{}
	roomIDs := make([]id.RoomID, 0, len(rooms))
	for _, r := range rooms {
		want[id.RoomID(r)] = true
		roomIDs = append(roomIDs, id.RoomID(r))
	}
	syncer := mautrix.NewDefaultSyncer()
	syncer.FilterJSON = &mautrix.Filter{
		Room: &mautrix.RoomFilter{
			Rooms:    roomIDs,
			Timeline: &mautrix.FilterPart{Types: []event.Type{event.EventMessage, event.EventEncrypted}, Limit: 50},
		},
		Presence:    &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
		AccountData: &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
	}
	// Backlog from before startup (the first /sync returns recent history) is
	// dropped by timestamp rather than DontProcessOldEvents, which only acts
	// on join events this timeline filter excludes.
	startMS := startAt.UnixMilli()
	syncer.OnEventType(event.EventMessage, func(_ context.Context, evt *event.Event) {
		l := log.With("room", evt.RoomID, "sender", evt.Sender, "event", evt.ID)
		switch {
		case !want[evt.RoomID]:
			l.Debug("drop: room not in config")
			return
		case evt.Timestamp < startMS:
			l.Debug("drop: sent before startup")
			return
		}
		msg, ok := toMessage(evt)
		if !ok {
			l.Debug("drop: edit or non-text message")
			return
		}
		l.Debug("received", "chars", len(msg.Body))
		handle(msg)
	})
	syncer.OnSync(func(_ context.Context, resp *mautrix.RespSync, since string) bool {
		log.Debug("sync", "joined_rooms", len(resp.Rooms.Join), "invited_rooms", len(resp.Rooms.Invite), "initial", since == "")
		return true
	})
	syncer.OnEventType(event.EventEncrypted, func(_ context.Context, evt *event.Event) {
		if want[evt.RoomID] && evt.Timestamp >= startMS {
			log.Warn("ignoring an encrypted message: roomd cannot read encrypted rooms; use a room with encryption off",
				"room", evt.RoomID, "sender", evt.Sender)
		}
	})
	cli.Syncer = syncer
	// A fresh in-memory sync store each start: no persisted since-token, so
	// the first /sync returns a backlog that startAt filters out.
	cli.Store = mautrix.NewMemorySyncStore()
	log.Info("matrix sync starting", "as", cli.UserID, "rooms", len(rooms))
	err := cli.SyncWithContext(ctx)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func toMessage(evt *event.Event) (Message, bool) {
	c := evt.Content.AsMessage()
	if c == nil {
		return Message{}, false
	}
	if c.RelatesTo != nil && c.RelatesTo.GetReplaceID() != "" {
		return Message{}, false // edit
	}
	switch c.MsgType {
	case event.MsgText, event.MsgNotice, event.MsgEmote:
	default:
		return Message{}, false
	}
	m := Message{
		RoomID:  string(evt.RoomID),
		EventID: string(evt.ID),
		Sender:  string(evt.Sender),
		Body:    c.Body,
		TS:      evt.Timestamp,
	}
	if v, ok := evt.Content.Raw[ControlKey].(bool); ok && v {
		m.Control = true
	}
	if c.Format == event.FormatHTML {
		m.FormattedBody = c.FormattedBody
	}
	if c.Mentions != nil {
		for _, u := range c.Mentions.UserIDs {
			m.Mentions = append(m.Mentions, string(u))
		}
	}
	return m, true
}
