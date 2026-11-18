# Router evals

`cmd/routereval` replays router calls against one or more models on an
OpenAI-compatible endpoint. It scores the answers and reports, per model:

- accuracy for each labelled field;
- latency;
- prompt, completion and reasoning tokens;
- estimated cost.

Each case is sent exactly as it was logged: the same system prompt and
input. The tool scores the model's raw answer, before roomd's deterministic
checks run. Those checks keep owed bots and drop busy or unknown names, so a
wrong raw answer is sometimes harmless in the room. The eval still shows
which model gets it right on its own.

## Eval set format

An eval set is JSONL. Each line is one line of `router.log_file` (a
`router.CallRecord`) with two extra fields:

- `"expect"`: the right answer. Lines without it are skipped.
- `"id"`: optional, used in reports instead of `file:line`.

```json
{"id": "group-question", "model": "...", "system": "...", "input": "...", "output": "...",
 "expect": {
   "addressed_to": "all",
   "user_question_pending": false,
   "plan": ["Noir", "Uta", "Shiori", "Velvet"],
   "first": "Uta",
   "note": "why this is the right answer"}}
```

Every `expect` field is optional, and only the ones you give are scored.

| field | right when |
|---|---|
| `addressed_to` | `"all"`, `"user"`, `"none"`, a name, or a list of these. Compared as a set. |
| `user_question_pending` | `true` or `false`. Equal. |
| `plan` | The answer's plan holds exactly these names. Case and order are ignored, and `[]` means nobody. |
| `first` | `plan[0]` is this name. |

Names are display names, as the model sees them. An answer that isn't
usable JSON gets every labelled field wrong. `all_correct` in the report
means every labelled field of that call was right.

`plan` is strict: if you label a group question with all four characters,
a plan that names three counts as wrong. Leave `plan` out and label
`addressed_to` instead when only the classification matters.

Calls the router settles without a model never reach the log, so they
can't be evaluated. That covers a user message naming bots, a single owed
bot, and the bot-turn cap.

## Building an eval set from the call log

1. Turn the log on in `deploy/roomd.yaml` and deploy. A relative path is
   relative to the config file, so on the homelab it lands in
   `~/.config/roomd/`:

   ```yaml
   router:
     log_file: ./router-calls.jsonl
   ```

2. Copy it off the homelab, keeping the calls that got an answer:

   ```bash
   scp homelab:.config/roomd/router-calls.jsonl /tmp/
   jq -c 'select(.error == null)' /tmp/router-calls.jsonl > evals/mine.jsonl
   ```

   To find one you remember, filter by time or by text, for example
   `jq -c 'select(.input | contains("你们觉得呢"))'`.

3. For each line the router got wrong, add an `"expect"` object (and an
   `"id"`). Add a few lines it got right as controls, so a model that always
   says the same thing doesn't score well. Unlabelled lines can stay in the
   file; they are skipped.

The call log holds real conversation text, so keep `evals/mine.jsonl` out of
git. `evals/*.jsonl` other than `examples.jsonl` is ignored.

## The examples

`examples.jsonl` holds hand-written cases with a made-up cast (Alex, Ada,
Cleo, Mira, Rex). Their system and input come from the current router
prompt. They cover both original failure modes:

- **Under-selection.** `group-question-zh` and `group-question-en` are a
  question to the whole group, where every character should answer, not
  one.
- **False "waiting for the user".** `question-in-passing-owed` and
  `question-in-passing-continuation` are a bot's question to the user in
  passing, which must not stop the others.
- **Control.** `real-question-to-user` is a direct question that should
  stop the bots.

## Running it

The key comes from `-key-env` (default `ROOMD_ROUTER_API_KEY`). Prices are
USD per 1M input and output tokens, from the provider's model list. Cost
counts reasoning tokens at the output price and ignores cached-input
discounts.

```bash
set -a; . deploy/roomd.env; set +a
go run ./cmd/routereval \
  -models deepseek-v4-1-flash-1017-uncensored,z-ai-glm-5-3-flash,z-ai-glm-5-3 \
  -price deepseek-v4-1-flash-1017-uncensored=0.33/1.32 \
  -price z-ai-glm-5-3-flash=0.15/0.50 \
  -price z-ai-glm-5-3=1.75/5.50 \
  -n 3 -v -out /tmp/routereval.jsonl \
  evals/examples.jsonl evals/mine.jsonl
```

Flags:

| flag | what it does |
|---|---|
| `-models` | Model ids to compare. Default: each case's logged model. |
| `-n` | Replay each case this many times. Answers vary between runs. |
| `-reasoning-effort none` | Sends `reasoning_effort`. Only for models that support it. Without the flag, each model uses its default. |
| `-json-mode=false` | For endpoints that reject `response_format`. |
| `-out FILE` | Writes every call (raw output, usage, which fields were wrong) as JSONL. |
| `-v` | Lists every wrong answer. |
| `-concurrency` | Calls in flight per model. |
| `-timeout` | Per-call timeout. |
| `-base-url` | The endpoint. Default: Venice. |

The report has a `reasoning tok/call` column for models that report
reasoning tokens. `reasoning chars/call` counts reasoning text returned
beside the answer, for endpoints that send it without a token count.
