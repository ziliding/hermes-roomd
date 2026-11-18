// Package transcript persists each room's message log, every bot's "seen up
// to" cursor and every bot's Hermes session generation in SQLite.
//
// Cursor semantics: a bot's cursor is the highest transcript seq that has been
// delivered to its Hermes session (the upper bound of the delta it was sent),
// NOT the seq of its own reply. A message that lands while the bot is thinking
// therefore still reaches it on its next turn.
package transcript

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Entry is one recorded room message.
type Entry struct {
	Seq           int64
	RoomID        string
	EventID       string
	Sender        string // Matrix user id from the event, never inferred
	Body          string
	FormattedBody string
	Mentions      []string // m.mentions user ids
	TS            int64    // origin_server_ts, ms
	// MediaURL is the mxc:// URI of an image message (Body then holds its
	// text form, "[image: name] caption"); MediaMIME is its type.
	MediaURL  string
	MediaMIME string
}

// Store is the SQLite-backed transcript.
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS messages (
	seq            INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id        TEXT NOT NULL,
	event_id       TEXT NOT NULL UNIQUE,
	sender         TEXT NOT NULL,
	body           TEXT NOT NULL,
	formatted_body TEXT NOT NULL DEFAULT '',
	mentions       TEXT NOT NULL DEFAULT '',
	ts             INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_room_seq ON messages(room_id, seq);
CREATE TABLE IF NOT EXISTS cursors (
	room_id  TEXT NOT NULL,
	bot      TEXT NOT NULL,
	seen_seq INTEGER NOT NULL,
	PRIMARY KEY (room_id, bot)
);
CREATE TABLE IF NOT EXISTS retain_cursors (
	room_id      TEXT PRIMARY KEY,
	retained_seq INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS segment_breaks (
	room_id TEXT NOT NULL,
	seq     INTEGER NOT NULL,
	PRIMARY KEY (room_id, seq)
);
CREATE TABLE IF NOT EXISTS steered (
	room_id TEXT NOT NULL,
	bot     TEXT NOT NULL,
	seq     INTEGER NOT NULL,
	PRIMARY KEY (room_id, bot, seq)
);
CREATE TABLE IF NOT EXISTS generations (
	room_id TEXT NOT NULL,
	bot     TEXT NOT NULL,
	gen     INTEGER NOT NULL,
	PRIMARY KEY (room_id, bot)
);
`

// Open opens (creating if needed) the store at path. Use ":memory:" in tests.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection: the sync goroutine and room workers serialize here
	// instead of hitting SQLITE_BUSY (and ":memory:" stays one database).
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", schema} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("transcript init: %w", err)
		}
	}
	// Columns added after the first release: add them to older databases.
	for _, col := range []string{"media_url", "media_mime"} {
		_, err := db.ExecContext(ctx, `ALTER TABLE messages ADD COLUMN `+col+` TEXT NOT NULL DEFAULT ''`)
		if err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("transcript migrate %s: %w", col, err)
		}
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Append records e once (duplicate event ids are ignored) and returns its seq.
func (s *Store) Append(ctx context.Context, e Entry) (int64, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO messages(room_id, event_id, sender, body, formatted_body, mentions, ts, media_url, media_mime)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		e.RoomID, e.EventID, e.Sender, e.Body, e.FormattedBody, joinIDs(e.Mentions), e.TS, e.MediaURL, e.MediaMIME)
	if err != nil {
		return 0, err
	}
	var seq int64
	err = s.db.QueryRowContext(ctx, `SELECT seq FROM messages WHERE event_id = ?`, e.EventID).Scan(&seq)
	return seq, err
}

// Since returns room entries with seq > after, oldest first, keeping only the
// newest limit entries when there are more.
func (s *Store) Since(ctx context.Context, roomID string, after int64, limit int) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, room_id, event_id, sender, body, formatted_body, mentions, ts, media_url, media_mime FROM (
			SELECT * FROM messages WHERE room_id = ? AND seq > ? ORDER BY seq DESC LIMIT ?
		 ) ORDER BY seq ASC`, roomID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var mentions string
		if err := rows.Scan(&e.Seq, &e.RoomID, &e.EventID, &e.Sender, &e.Body, &e.FormattedBody, &mentions, &e.TS, &e.MediaURL, &e.MediaMIME); err != nil {
			return nil, err
		}
		e.Mentions = splitIDs(mentions)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Cursor returns the bot's seen-up-to seq in room (0 if it has never been called).
func (s *Store) Cursor(ctx context.Context, roomID, bot string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT seen_seq FROM cursors WHERE room_id = ? AND bot = ?`, roomID, bot).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

// SetCursor records that everything up to seq has been delivered to the bot.
// The cursor never moves back. Steered marks at or below it are dropped.
func (s *Store) SetCursor(ctx context.Context, roomID, bot string, seq int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cursors(room_id, bot, seen_seq) VALUES(?,?,?)
		 ON CONFLICT(room_id, bot) DO UPDATE SET seen_seq = MAX(seen_seq, excluded.seen_seq)`,
		roomID, bot, seq)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM steered WHERE room_id = ? AND bot = ?
		 AND seq <= (SELECT seen_seq FROM cursors WHERE room_id = ? AND bot = ?)`, roomID, bot, roomID, bot)
	return err
}

// MarkSteered records that the message at seq already reached the bot's
// session as a steer, beyond its cursor, so it is left out of later deltas.
func (s *Store) MarkSteered(ctx context.Context, roomID, bot string, seq int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO steered(room_id, bot, seq) VALUES(?,?,?)`, roomID, bot, seq)
	return err
}

// Steered returns the seqs above after that reached the bot as steers.
func (s *Store) Steered(ctx context.Context, roomID, bot string, after int64) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq FROM steered WHERE room_id = ? AND bot = ? AND seq > ?`, roomID, bot, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, err
		}
		out[seq] = true
	}
	return out, rows.Err()
}

// Generation returns the bot's Hermes session generation in room, starting at 1.
// /reset bumps it (Hermes retires deleted session ids for good, so a reset
// must mint a new id rather than reuse one).
func (s *Store) Generation(ctx context.Context, roomID, bot string) (int, error) {
	var gen int
	err := s.db.QueryRowContext(ctx, `SELECT gen FROM generations WHERE room_id = ? AND bot = ?`, roomID, bot).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO generations(room_id, bot, gen) VALUES(?,?,1)`, roomID, bot)
		return 1, err
	}
	return gen, err
}

// LatestSeq returns the newest seq recorded in room (0 if none).
func (s *Store) LatestSeq(ctx context.Context, roomID string) (int64, error) {
	var seq sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM messages WHERE room_id = ?`, roomID).Scan(&seq)
	return seq.Int64, err
}

// LastFrom returns the sender of the newest message in room whose sender is
// one of senders, or "" if none of them has spoken.
func (s *Store) LastFrom(ctx context.Context, roomID string, senders []string) (string, error) {
	if len(senders) == 0 {
		return "", nil
	}
	args := []any{roomID}
	for _, id := range senders {
		args = append(args, id)
	}
	var sender string
	err := s.db.QueryRowContext(ctx,
		`SELECT sender FROM messages WHERE room_id = ? AND sender IN (?`+strings.Repeat(",?", len(senders)-1)+`)
		 ORDER BY seq DESC LIMIT 1`, args...).Scan(&sender)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return sender, err
}

// Reset starts fresh Hermes sessions for bots in room, in one transaction:
// every bot's generation goes up by one (a bot never called counts as
// generation 1, so it moves to 2), and every cursor moves to upTo, so the
// new sessions never see messages from before the reset. Old session ids are
// never reused or deleted.
func (s *Store) Reset(ctx context.Context, roomID string, bots []string, upTo int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, bot := range bots {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO generations(room_id, bot, gen) VALUES(?,?,2)
			 ON CONFLICT(room_id, bot) DO UPDATE SET gen = MAX(gen, 1) + 1`, roomID, bot); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cursors(room_id, bot, seen_seq) VALUES(?,?,?)
			 ON CONFLICT(room_id, bot) DO UPDATE SET seen_seq = MAX(seen_seq, excluded.seen_seq)`,
			roomID, bot, upTo); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// After returns room entries with after < seq <= upTo, oldest first, at most
// limit of them (the oldest ones). upTo <= 0 means no upper bound.
func (s *Store) After(ctx context.Context, roomID string, after, upTo int64, limit int) ([]Entry, error) {
	if upTo <= 0 {
		upTo = 1<<63 - 1
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, room_id, event_id, sender, body, formatted_body, mentions, ts, media_url, media_mime FROM messages
		 WHERE room_id = ? AND seq > ? AND seq <= ? ORDER BY seq ASC LIMIT ?`, roomID, after, upTo, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var mentions string
		if err := rows.Scan(&e.Seq, &e.RoomID, &e.EventID, &e.Sender, &e.Body, &e.FormattedBody, &mentions, &e.TS, &e.MediaURL, &e.MediaMIME); err != nil {
			return nil, err
		}
		e.Mentions = splitIDs(mentions)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestTS returns the origin timestamp (ms) of the newest message in room,
// or 0 if the room has none.
func (s *Store) LatestTS(ctx context.Context, roomID string) (int64, error) {
	var ts sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT ts FROM messages WHERE room_id = ? ORDER BY seq DESC LIMIT 1`, roomID).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return ts.Int64, err
}

// RetainCursor returns the highest seq already written to the group memory
// bank for room, and whether a cursor exists at all.
func (s *Store) RetainCursor(ctx context.Context, roomID string) (int64, bool, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT retained_seq FROM retain_cursors WHERE room_id = ?`, roomID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return seq, err == nil, err
}

// SetRetainCursor records that everything up to seq has been retained (or
// deliberately skipped). It never moves the cursor back.
func (s *Store) SetRetainCursor(ctx context.Context, roomID string, seq int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO retain_cursors(room_id, retained_seq) VALUES(?,?)
		 ON CONFLICT(room_id) DO UPDATE SET retained_seq = MAX(retained_seq, excluded.retained_seq)`,
		roomID, seq)
	return err
}

// AddSegmentBreak records that a conversation segment ends at seq (/reset):
// the next memory document starts after it.
func (s *Store) AddSegmentBreak(ctx context.Context, roomID string, seq int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO segment_breaks(room_id, seq) VALUES(?,?)`, roomID, seq)
	return err
}

// NextSegmentBreak returns the smallest break seq greater than after, or 0
// if there is none.
func (s *Store) NextSegmentBreak(ctx context.Context, roomID string, after int64) (int64, error) {
	var seq sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MIN(seq) FROM segment_breaks WHERE room_id = ? AND seq > ?`, roomID, after).Scan(&seq)
	return seq.Int64, err
}

func joinIDs(ids []string) string { return strings.Join(ids, "\n") }

func splitIDs(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
