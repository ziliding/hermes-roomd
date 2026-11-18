# roomd

roomd runs multi-bot **Matrix group rooms** whose characters are **Hermes
Agent** profiles. Deterministic code, not the models, decides who speaks and
what each bot sees. A bot is called through the Hermes Runs API only when it
is its turn, and roomd posts the reply as that bot.

```
Alex (Element X) ──► Matrix group room ◄── roomd ── 1. decide who speaks (or nobody)
                                              │     2. build the bot's attributed delta (+ recalled memory)
                                              ▼
                     Hermes: POST /p/<profile>/v1/runs
                             session_id: grp-<roomhash>-<bot>-g<N>
                                              │
                     roomd posts the reply as that bot (roomd's own device for it)
```

## How it works

### Who speaks

Only messages from the configured `user` start a turn chain. Bot messages are
recorded but never trigger anything on their own.

| Situation | Result |
|---|---|
| `max_bot_turns` consecutive bot replies | nobody speaks until the user does (hard cap) |
| router on: a bot asks the user a question | the chain stops |
| the user names a bot | that bot answers |
| the user names nobody | the router picks; with the router off, a random bot (never the one that just spoke) |
| a bot names another bot | router on: a hint only; router off: that bot is next |

- **Mentions are hints, not triggers.** Display names, `@handles`, Matrix
  ids, pills and `m.mentions` all count, but only the earliest addressee in a
  message.
- **The user interrupts.** A new user message resets the counters. A reply
  already being generated still posts, and the new message starts a fresh
  chain.
- **Silence.** A reply that is only a silence marker (`[SILENT]`, `NO_REPLY`,
  `(silent)`, `🔇`, `.`, `…`) posts nothing and ends the chain.

### Images

**Images you post** reach the bots as real images, so a vision model can
look at them:

- roomd reads unencrypted `m.image` messages. The transcript records
  `[image: name] caption` and the image's `mxc://` URL, never its data.
- On a bot's next turn, roomd downloads each image in its delta as that bot
  and attaches it to the run input as an `image_url` part (a data URL),
  after a label such as `Image from Alex (cat.png):`. Each image reaches
  each bot once, like the text around it.
- At most 4 images (the newest) and 10 MB each go into one run. Older ones,
  and ones that can't be downloaded or aren't images, become a one-line
  note in the input instead.
- An image is never steered into a busy bot's run (steer input is text
  only); it waits for that bot's next turn.
- The bots' model must accept images. A text-only model rejects the run
  or ignores them, depending on the provider.

**Images a bot posts** go out as real Matrix images: roomd uploads each
one to the homeserver as that bot and sends an `m.image`. roomd
recognises three shapes in a reply:

- **`MEDIA:/path/to/file.png` tags** and **bare absolute paths** ending in
  `.png`, `.jpg`, `.jpeg`, `.gif`, `.webp` or `.bmp`. Hermes' Runs API
  passes these through untouched, so roomd reads the file itself. It does
  this only when the file, after resolving symlinks, lies inside one of
  `media_dirs`. The file must also be a regular file of at most 10 MB
  whose bytes are an image. With no `media_dirs`, roomd reads no files.
- **Inline `![image](data:image/…;base64,…)`**, which Hermes' other
  endpoints produce.

How the reply is posted:

- **Order:** text and images go out in their original order, one event
  each. A background result's reply relation goes on the first event.
- **No host paths:** a tag or path that became an image is removed from the
  text. One that can't be sent (outside `media_dirs`, missing, too big, not
  an image) becomes "(image could not be sent)" and is logged. A `MEDIA:`
  tag for a non-image file is removed.
- **Transcript:** each image is recorded as `[image]`, never its data, so
  deltas and memory stay small.
- **Long text** is split into several messages, below Matrix's 64 KB event
  limit.

### Long tasks

While a bot's run is going, the bot shows as typing. A bot doing real tool
work doesn't hold up the room:

- **Hand-off.** A run still going after `background_after` (default 20s)
  continues in the background. The listener posts one notice ("Ada is
  working on it…"), the bot is **busy** in that room, and the room goes on.
  Quicker replies work as before.
- **Busy bots are skipped.** Neither the turn rules nor the router pick a
  busy bot. Busy is per room, so the bot still answers in other rooms.
- **The result** is posted by the bot as a Matrix reply to the message that
  asked for it. It starts no chain.
- **Failure.** If the run fails, is cancelled or exceeds `hermes_timeout`
  (roomd then stops it), the listener posts a short notice. The request
  stays in the bot's next delta.  A turn that ends unfinished (for example out of
  iterations) still posts the text it has.
- **Steering.** A message that names a busy bot is steered into its running
  task (Hermes `/v1/runs/{id}/steer`), attributed like any delta line. The
  bot acknowledges it with a ✓ reaction, and no chain starts. A steered
  message counts as seen only if the run consumed it. One the run never
  read, or that Hermes refused, reaches the bot in its next delta, never
  twice. Messages to others aren't steered; the busy bot sees them on its
  next turn.
- **`!reset` while busy:** the result is still posted, and the bot's next
  run uses the new session. **`!explain` on a busy bot:** a short notice
  that it is busy.
- **Restarting roomd** while a run is in the background leaves the run to
  Hermes; its result isn't posted.

### The router (optional)

With `router.enabled`, a separate cheap model (any OpenAI-compatible
endpoint) answers `{"next": "<name>"}` or `{"next": null}`. **null, meaning
wait for the user, is the default.** It sees the last `router.window`
messages, each character's `blurb`, and how often each one has spoken
recently. It picks someone only when that character is addressed, a question
is still open, or the character clearly has something new to add.

- Malformed output, an unknown name, or the bot that just spoke: nobody
  speaks.
- A failed or timed-out call: the plain turn rules decide that turn.
- **Write real blurbs.** The router tells characters apart only by them, and
  roomd warns at startup when one is missing.

### What a bot sees

Each bot has one Hermes session per room. Hermes keeps the history, so each
run carries only two things:

- **Instructions** (the system prompt): the room's group rules, filled in
  for that bot. They are byte-identical on every call, so the prompt cache
  stays warm.
- **Input:** what that bot hasn't seen yet, one element per message:
  ```
  <msg from="Cleo">…</msg>
  ```
  - Speakers come from the Matrix `sender`, never from the text.
  - Mentions are rewritten to `(to Ada)`.
  - Message text is escaped, so it can't fake another speaker.
  - The bot's own messages are left out, because Hermes already holds them.
  - Images posted in those messages are attached after the text (see
    [Images](#images)).

Before posting, roomd cleans the reply. It unwraps a reply the model wrapped
in its own `<msg>` element, drops text written for other speakers and echoed
`Name:` labels, and posts nothing if what's left is empty. Replies go out as
`msgtype` with an empty `m.mentions`, so they ping nobody.

State lives in SQLite (`state_db`): the transcript, a per-bot cursor (the
last message already delivered to that bot) and the session generations.
After a failed call the cursor stays put, so the same delta is offered again.

### Commands

Only the user can run commands. They are never shown to the bots. `!` is
the documented form, because Element blocks unknown `/` commands;
`/reset` and `/explain` also work when a client sends them.

- **`!reset`:** every bot in the room starts a fresh Hermes session
  (generation `N+1`; ids are never reused, because Hermes retires deleted
  ids). The new sessions start after the reset point. The room's listener
  posts a short notice.
- **`!explain [name]`:** the named bot, or the last speaker, explains in
  plain language what is going on and what it wants from the user. It is one
  turn, with no chain after it.

### Group memory (optional)

With `memory.enabled`, roomd keeps a Hindsight bank of past group chats
(default `roomd-group`; a room can have its own). **Only roomd reads and
writes it.** The bots never write to it, and it is separate from each
profile's own memory.

- **Recall:** at most once per user message, roomd queries the bank with the
  recent messages. Results above `recall.min_score` go into the bot's user
  message, never the system prompt, as a
  `<memory source="past group chats">` block that the rules tell the bots to
  treat as possibly wrong. Each memory reaches a session at most once. Any
  error or timeout means the turn goes ahead without memory.
- **Retain:** after `retain.idle_minutes` of quiet, or a `!reset`, roomd
  writes **one** document for that conversation.
  - **Summary mode (default):** the router's model writes notes that record
    what the user said and decided. Character statements are kept as
    opinions, and coined terms are dropped or marked.
  - **Transcript mode:** the attributed messages as they are.
  - **Skipped:** conversations shorter than `retain.min_messages`, or without
    a user message.
  - **Retries:** a failed write is retried with backoff, and a retry
    replaces the document rather than duplicating it.
  - **Bank:** created on the first write, with a retain mission saying the
    same thing. An existing bank is never modified.
  - **No backfill:** enabling memory doesn't write older history.
- **Profiles' own memory is unaffected.** A character whose profile has a
  memory provider (such as the Hindsight plugin) still auto-saves group
  turns into that profile's bank, and Hermes offers no per-request switch to
  stop it.
  Those entries are tagged `platform: api_server` with session ids starting
  `grp-`, so they are easy to find and delete.

## Configuration

Everything you deploy lives in `deploy/`, which is the source of truth:

```
deploy/roomd.example.yaml   config template (committed)
deploy/roomd.yaml           your config (git-ignored)
deploy/roomd.env.example    secrets template (committed)
deploy/roomd.env            your secrets (git-ignored)
deploy/rules/               group rules; {{name}} {{user}} {{others}} are filled in per bot
deploy/roomd.service        systemd user unit
```

`compose.yaml` at the repository root is the Docker Compose alternative.

`deploy/roomd.example.yaml` documents every key. The main ones:

- **Top level:**
  - `homeserver`, `user`, `user_display`;
  - `rooms`;
  - `max_bot_turns`, `max_delta_lines`;
  - `msgtype` (`m.text` or `m.notice`);
  - `state_db`, `group_rules_file`;
  - `hermes_timeout` (the most a run may take), `background_after` (when a
    run moves to the background; `0` means always wait);
  - `listener`: the bot whose roomd device runs `/sync` and posts notices;
  - `media_dirs`: absolute directories whose images bots may post by path
    (for example an image generator's output folder).
- **`bots.<key>`:**
  - `display`, `mxid`;
  - `access_token_env`: roomd's own device token, not the gateway's;
  - `hermes.base_url`: e.g. `http://127.0.0.1:8642/p/ada`; the URL, not
    `model`, selects the profile;
  - `hermes.key_env`: that profile's `API_SERVER_KEY`;
  - `blurb`.
- **`router`:** `enabled`, `base_url` (including `/v1`), `model`, `key_env`,
  `timeout`, `window`, `json_mode`.
- **`memory`:**
  - `enabled`;
  - `api_url`: Hindsight, without `/v1`;
  - `bank_id`;
  - `recall.*`: `max_tokens`, `budget`, `timeout`, `min_score`;
  - `retain.*`: `mode`, `idle_minutes`, `min_messages`, `timeout`.
  - Summary mode uses the router's `base_url`, `model` and `key_env`, even
    when the router is off.

Secrets never go in the YAML: every `*_env` key names an environment
variable. Relative paths resolve against the config file's folder.

### Per-room settings

A room is either a plain id or a mapping that overrides the top level:

```yaml
rooms:
  - "!abc:example.com"
  - id: "!def:example.com"
    name: lounge                   # label for logs
    bots: [ada, cleo]              # the cast (default: all bots)
    listener: cleo                 # must be in the cast
    max_bot_turns: 3
    background_after: 0s           # always wait for a bot's run here
    rules_file: rules/lounge.md
    memory: { bank_id: roomd-lounge, retain: { idle_minutes: 10 } }
```

- **Cast:** turns, the router, `!explain`, `!reset`, memory participants and
  each bot's list of others cover only the room's cast.
- **Listener:** roomd runs one `/sync` loop per distinct listener. A listener
  only has to be joined to its own rooms. If the top-level listener isn't in
  a room's cast, that room uses its first cast bot.
- **Memory:** rooms that name the same bank share it.
  - A room can override `recall.enabled`, `retain.enabled`,
    `retain.idle_minutes` and `retain.min_messages`.
  - The rest of the memory settings are global, and `memory.enabled` stays
    the global switch.

### Startup checks

roomd refuses to start on:

- unknown keys, missing env vars or rules files;
- duplicate display names or room ids, or a room alias instead of a room id;
- a listener outside its room's cast;
- a token that belongs to another user (`whoami`);
- an encrypted room, or a listener that isn't joined to its room;
- a `media_dirs` entry that doesn't exist on the host it runs on.

`roomd -check -config <file>` runs the same config validation and exits.
It doesn't contact Matrix or look for `media_dirs`, so it runs anywhere.

## Homelab setup

Do this once, before the first message in a group room.

1. **Hermes API per character.** Each profile needs the API server on and an
   `API_SERVER_KEY`; session continuation is refused without one. roomd
   calls the character profiles directly, so their own memory and tool
   settings apply to group turns. Smoke test with a throwaway session id,
   never a `grp-…` one:
   ```bash
   curl -s http://127.0.0.1:8642/p/ada/v1/runs \
     -H "Authorization: Bearer $ROOMD_ADA_API_KEY" -H 'Content-Type: application/json' \
     -d '{"input":"say hi in one line","session_id":"roomd-smoke-ada-1","instructions":"Be brief."}'
   # → {"run_id": "run_…", "status": "started"}; then:
   curl -s http://127.0.0.1:8642/p/ada/v1/runs/<run_id> -H "Authorization: Bearer $ROOMD_ADA_API_KEY"
   ```
2. **Keep the Hermes Matrix gateways out of group rooms,** or every message
   gets two replies.
   - Give each character profile a **non-empty** `MATRIX_ALLOWED_ROOMS` that
     excludes the group rooms; a placeholder room id is fine. Empty means
     "all rooms". DMs are exempt, so they keep working.
   - Invite every bot and name the room before anyone speaks. A room with
     two members or fewer counts as a DM and bypasses the allowlist.
3. **A roomd device per bot.** Don't reuse the gateway's token:
   ```bash
   curl -s -X POST http://127.0.0.1:6167/_matrix/client/v3/login -H 'Content-Type: application/json' \
     -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"ada"},
          "password":"<password>","device_id":"ROOMD","initial_device_display_name":"roomd"}'
   ```
   Put each `access_token` in `ROOMD_<BOT>_TOKEN`. roomd doesn't accept
   invites, so join each bot to its rooms yourself.
4. **Unencrypted rooms.** Encryption can't be turned off later. Create the
   room with encryption off in Element Web/Desktop, or through the API (the
   result is unencrypted):
   ```bash
   curl -s -X POST http://127.0.0.1:6167/_matrix/client/v3/createRoom \
     -H "Authorization: Bearer $ROOMD_ADA_TOKEN" -H 'Content-Type: application/json' \
     -d '{"name":"Group","preset":"private_chat","invite":["@alex:example.com","@cleo:example.com"]}'
   ```

## Deploy and operate

From the Mac, with [mise](https://mise.jdx.dev):

```bash
mise run test                    # go vet + race tests
mise run deploy                  # test, validate deploy/roomd.yaml, cross-build, upload, restart
mise run deploy -- --no-restart  # upload only
mise run logs                    # follow the service log
```

What `scripts/deploy.sh` does:

- **Builds:** cross-builds for the homelab's CPU.
- **Shows changes:** diffs `roomd.yaml` against the homelab's copy, and
  compares the variable names (never the values) in `roomd.env`.
- **Uploads:**
  - the binary to `~/.local/bin/roomd`, swapped in atomically;
  - `roomd.yaml`, `roomd.env` and `rules/` to `~/.config/roomd/`;
  - the unit to `~/.config/systemd/user/`.
- **Restarts:** restarts roomd. If roomd doesn't come back up, it prints the
  last log lines and exits non-zero.
- **Host:** `ROOMD_HOST` overrides the SSH host (default `homelab`).

Always edit in `deploy/`, never on the homelab, because the next deploy
overwrites the homelab's copies.

- **Rules changes:** editing a rules file changes every bot's system prompt,
  so each group session misses the prompt cache once. Run `!reset` if you
  want the bots to start clean.
- **Without mise:** build with `CGO_ENABLED=0 go build -o roomd ./cmd/roomd`
  and install the files as above. The unit expects the binary in
  `~/.local/bin`, the config in `~/.config/roomd`, and
  `loginctl enable-linger`.
- **`226/NAMESPACE` error:** if the unit fails with it, remove
  `PrivateTmp=yes`.
- **Docker:** `docker compose up -d --build` from the repository root. The
  container uses host networking, so it publishes no ports. It reads its
  secrets from `deploy/roomd.env`, and its config, rules and database live
  in `deploy/data/`. Bind-mount each `media_dirs` entry read-only at the
  same path as on the host (see `compose.yaml`).

roomd opens no ports. It only connects out to the homeserver, Hermes and,
optionally, Hindsight and the router endpoint, and everything stays on the
tailnet.

### Inspecting group memory

```bash
ssh homelab 'curl -s localhost:8888/v1/default/banks/roomd-group/documents | jq'                    # what roomd wrote
ssh homelab 'curl -s "localhost:8888/v1/default/banks/roomd-group/memories/list?limit=50" | jq'     # extracted facts
ssh homelab journalctl --user -u roomd | grep 'memory:'                                            # recalls, writes, skips
```

To delete a wrong memory, delete its document:
`DELETE /v1/default/banks/<bank>/documents/<document_id>`. That also removes
the facts extracted from it.

## Development

```bash
go vet ./... && go test -race ./...
```

Requires Go ≥ 1.26 and builds as pure Go (no cgo, no libolm).

```
cmd/roomd             wiring, config load, sync loops, signals
internal/config       YAML → validated config; secrets from env
internal/matrixio     per-bot senders, media download and the /sync listener
internal/transcript   SQLite: messages, cursors, generations, steered messages, retain cursors
internal/roster       who is who; addressee and pill detection
internal/contextbuild delta → <msg from="Name"> elements, mention rewriting
internal/silence      silence-marker detection
internal/turns        Decider interface and the plain turn rules
internal/router       LLM router (a turns.Decider that falls back to the rules)
internal/commands     !reset and !explain parsing
internal/media        inline images and MEDIA: tags out of replies; long-text splitting
internal/hermes       Runs API client (start, poll, steer, stop); hermestest is a fake server
internal/hindsight    Hindsight HTTP client (recall, retain, banks)
internal/memory       recall block and once-per-conversation retain
internal/room         the orchestrator: one worker per room, the turn chain, background runs
```

## License

MIT. See [LICENSE](LICENSE).
