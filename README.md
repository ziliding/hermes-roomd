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

A user message is **owed** a reply by a set of bots. That set has no fixed
order: whenever nobody is generating a reply, the decider (router or turn
rules) picks the next one from the bots that still owe a reply.

| The user's message | Who owes a reply |
|---|---|
| names bots ("Cleo and Ada, thoughts?") | each bot it names |
| addresses the group (a `group_words` entry: "你们", "大家", "everyone", …) | every bot |
| names nobody | router on: whoever the router plans (every bot when it says the message is for everyone; nobody for "ok", "thanks"); router off: one random bot |

- **Owed replies are always delivered.** A bot that stays silent, fails or
  hands its run to the background has met its obligation, and the next one
  goes on. A question a bot asks the user in passing doesn't cancel the
  others' replies. `max_bot_turns` doesn't count owed replies.
- **Then the bots may talk among themselves**, at most `max_bot_turns` times
  until the user speaks again (`!continue` grants another round). With the
  router on, a bot speaks only if it is addressed or has something new to
  add, and the exchange stops when the conversation is waiting on the user's answer. With the
  router off, a bot the last message names goes next, otherwise a random
  bot (never the one that just spoke). A bot that stays silent ends the
  exchange.
- **Mentions are hints, not triggers.** Display names, `@handles`, Matrix
  ids, pills and `m.mentions` all count.
- **The user interrupts.** A new user message resets the counters. A reply
  already being generated still posts, and the new message starts a fresh
  chain.
- **Silence.** A reply that is only a silence marker (`[SILENT]`, `NO_REPLY`,
  `(silent)`, `🔇`, `.`, `…`) posts nothing.

### Images

**Images you post** reach the bots as real images, so a vision model can
look at them:

- roomd reads unencrypted `m.image` messages. The transcript records
  `[image: name] caption` and the image's `mxc://` URL, never its data.
- On a bot's next turn, roomd downloads each image in its delta as that bot
  and attaches it to the run input as an `image_url` part (a data URL),
  after a label such as `Image from Alex (cat.png):`. Each image reaches
  each bot once, like the text around it.
- Images are scaled down to at most 1568 px on the longest side. Anything
  other than a small PNG or JPEG is re-encoded as JPEG, because Hermes
  refuses request bodies over 10 MB.
- A run takes at most 4 images (the newest), each downloaded at most 10 MB
  and 5 MB in all after scaling. Older images, and ones that can't be
  downloaded or decoded, become a one-line note in the input instead.
- If Hermes refuses a run with images, or the run fails while carrying
  them, roomd sends that turn again without the images, with a note in
  their place.
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
  The hand-off meets the bot's obligation, and the chain continues with the
  other bots. Quicker replies work as before.
- **Busy bots are skipped.** Neither the turn rules nor the router pick a
  busy bot. Busy is per room, so the bot still answers in other rooms.
- **The others know.** While a bot is busy, every other bot's run input
  gets a status line after the messages: who is working in the background
  and on whose message, so they don't do that task or answer for it. The
  line isn't recorded in the transcript, so it disappears when the bot is
  done.
- **The result** is posted by the bot as a Matrix reply to the message that
  asked for it, but only when the floor is free. If another bot is mid-reply,
  the result waits in the room's queue until that turn ends, then posts
  before the next turn. The bot stays busy until its result is in the room.
- **The result is a new event.** The router (or the turn rules) may let the
  others react to it. That is bot-to-bot continuation: it shares
  `max_bot_turns` with any other bot-to-bot replies since the user last
  wrote (the result itself isn't counted), and it never creates new
  obligations to the user. If the user has written since the run ended,
  nobody reacts; their message is answered instead.
- **Failure.** If the run fails, is cancelled or exceeds `hermes_timeout`
  (roomd then stops it), the listener posts a short notice. The request
  stays in the bot's next delta.  A turn that ends unfinished (for example out of
  iterations) still posts the text it has.
- **Steering.** A message that names a busy bot is steered into its running
  task (Hermes `/v1/runs/{id}/steer`), attributed like any delta line. The
  bot acknowledges it with a ✓ reaction. No chain starts unless the message
  also names a free bot or addresses the group; those still answer. A steered
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
endpoint) answers with a response plan:

```json
{"addressed_to": ["Ada"] | "all" | "user" | "none",
 "user_question_pending": false,
 "plan": ["Cleo", "Ada"],
 "reason": "a few words"}
```

The analysis fields come first, so the model classifies the message before
it picks anyone. It sees the last `router.window` messages, each character's
`blurb`, who still owes the user a reply, who is busy, and how often each
one has spoken recently. Few-shot examples in its prompt cover a group
question and a question asked in passing.

Code, not the model, keeps the rules:

- **The user names bots:** they answer, with no call.
- **The user addresses the group** (`group_words`, or the model says
  `"all"`): every bot owes a reply, even if the model's `plan` lists fewer.
- **Bots still owe the user:** the model only orders them and can't drop
  any. With one left, there is no call.
- **`user_question_pending`** only stops bots from talking among
  themselves. The prompt counts it only when the message's main point is a
  direct question to the user, not a question in passing or a rhetorical
  one.
- **Names are validated:** unknown names, busy bots and (between bots) the
  bot that just spoke are dropped.
- **Failures:** a failed or timed-out call lets the plain turn rules decide
  that turn. So does an unusable answer, except between bots with nobody
  owed a reply, where nobody speaks.
- **Call log:** with `router.log_file`, every call is appended as one JSON
  line (system prompt, input, raw output, parsed plan, model, latency and
  the token usage the endpoint reports, including reasoning tokens), to
  build an eval set from.
- **Eval replay:** `cmd/routereval` re-runs labelled call-log lines against
  candidate models and reports accuracy per field, latency, tokens and cost.
  See [evals/README.md](evals/README.md).
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
the `/` forms (`/reset`, `/explain`, `/continue`) also work when a client
sends them.

- **`!reset`:** every bot in the room starts a fresh Hermes session
  (generation `N+1`; ids are never reused, because Hermes retires deleted
  ids). The new sessions start after the reset point. The room's listener
  posts a short notice.
- **`!explain [name]`:** the named bot, or the last speaker, explains in
  plain language what is going on and what it wants from the user. It is one
  turn, with no chain after it.
- **`!continue [n]`:** the bots carry on talking among themselves after they
  stopped at `max_bot_turns`: up to `n` more replies (default: the room's
  `max_bot_turns`; at most 20). It picks up from the latest message, which
  must be a bot's. A bot other than the last speaker always gets the first
  turn, even if the router thinks the exchange is over (it can still stay
  silent); after that the usual rules apply, so the round can end early.
  The extra allowance lasts until you next speak, and your message stops
  the round like any chain.

### Check-ins (optional)

With `check_in.enabled`, a room that has been quiet for a while comes back
to life on its own: one bot is asked to start something, the way a friend
would text a group chat.

- **When:** once nobody (you or a bot) has written for `after` (default
  3h). Quiet counts from the newest message, your last message or command,
  the last check-in, or roomd's start, whichever is latest. A restart
  therefore never sets the bots off at once.
- **Randomness:** `jitter` moves each check-in up to that much earlier or
  later, drawn once per quiet spell.
- **Quiet hours:** no check-in starts inside `quiet_hours` (e.g.
  `"23:00-07:00"`, read in `timezone`). One that falls due inside them waits
  for their end, plus a random delay of at most `jitter`.
- **Who:** a random free bot, not the one that spoke last. Its run input says
  how long the chat has been quiet and the local time, and lets it reply
  `[SILENT]` if nothing comes to mind. The instruction goes in the user
  message, never the system prompt.
- **Then:** the others may react, as bot-to-bot continuation under
  `max_bot_turns` (the opener isn't counted). Your message stops it like any
  chain, and a check-in queued before your message is dropped.
- **Unanswered:** after `max_unanswered` check-ins in a row (default 2; `0`
  means no limit) with no message or command from you in between, roomd
  waits for you. The count lives in memory, so a restart clears it.

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
  - **Summary mode (default):** the summary model (by default the router's
    model; see `retain.summary`) writes notes that record
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
  - `max_bot_turns` (bot-to-bot replies after the owed ones), `max_delta_lines`;
  - `group_words`: words that address every bot at once (default: 你们, 大家,
    各位, everyone, everybody, all of you, you all, y'all, you guys; `[]`
    turns it off);
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
  `timeout`, `window`, `json_mode`, `log_file`.
- **`check_in`:** `enabled`, `after`, `jitter`, `quiet_hours`, `timezone`,
  `max_unanswered` (see [Check-ins](#check-ins-optional)). `timezone`
  defaults to the host's local time, which is UTC in Docker.
- **`memory`:**
  - `enabled`;
  - `api_url`: Hindsight, without `/v1`;
  - `bank_id`;
  - `recall.*`: `max_tokens`, `budget`, `timeout`, `min_score`;
  - `retain.*`: `mode`, `idle_minutes`, `min_messages`, `timeout`,
    `summary`.
  - `retain.summary`: the model that writes summary-mode notes, as
    `base_url`, `model` and `key_env`. Each field you leave out takes the
    router's value, even when the router is off, so without the block summary
    mode uses the router's model. Use it to run the infrequent summary on a
    stronger model than the per-turn router.

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
    check_in: { enabled: true, after: 90m }
```

- **Cast:** turns, the router, `!explain`, `!reset`, `!continue`, memory
  participants and each bot's list of others cover only the room's cast.
- **Listener:** roomd runs one `/sync` loop per distinct listener. A listener
  only has to be joined to its own rooms. If the top-level listener isn't in
  a room's cast, that room uses its first cast bot.
- **Check-ins:** a room's `check_in` can override any of its keys; the rest
  come from the top level. A room can turn check-ins on while the top level
  leaves them off, or the other way round.
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
cmd/routereval        replays labelled router calls against candidate models (evals/)
internal/config       YAML → validated config; secrets from env
internal/matrixio     per-bot senders, media download and the /sync listener
internal/transcript   SQLite: messages, cursors, generations, steered messages, retain cursors
internal/roster       who is who; addressee and pill detection
internal/contextbuild delta → <msg from="Name"> elements, mention rewriting
internal/silence      silence-marker detection
internal/turns        Decider interface and the plain turn rules
internal/router       LLM router (a turns.Decider that falls back to the rules)
internal/commands     !reset, !explain and !continue parsing
internal/checkin      check-in schedule: quiet hours, jitter, the opener's instruction
internal/media        inline images and MEDIA: tags out of replies; long-text splitting
internal/hermes       Runs API client (start, poll, steer, stop); hermestest is a fake server
internal/hindsight    Hindsight HTTP client (recall, retain, banks)
internal/memory       recall block and once-per-conversation retain
internal/room         the orchestrator: one worker per room, the turn chain, background runs, check-ins
```

## License

MIT. See [LICENSE](LICENSE).
