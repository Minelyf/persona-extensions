# Model routes

Since 0.2.0-beta.30, enkeep memory can pick its model per call: a fast primary model first, and when it does not
answer, a fallback chosen by how long the call is. Every text step goes this way: the daily extraction and triage, the
weekly consolidation and synthesis, people cards, signals, and the rewriting of what goes to the assistant (exits).
The exits' format does not change.

## What a call does

1. **Primary models** (`model_primary`, in order; none by default: set it to a fast, cheap model of your connection,
   a Flash-class one, say). The first that answers is used.
2. **One fallback by size.** When no primary model answered, the call goes to `model_long` (default
   `claude-sonnet-5-5`) if its estimated size is above `model_long_above` (default 16000), else to `model_short`
   (default `claude-haiku-5-5`).

An attempt **fails**, and the next model is tried, when:

| reason | what the hub said |
| --- | --- |
| `error` | `model_failed` (an upstream error, a gateway refusing the account, a network error) |
| `timeout` | `timeout`: a primary model's attempt took longer than `model_primary_timeout` (120 s), a fallback's longer than the step's limit (300 s; 600 s for medium-tier steps) |
| `empty` | an answer with no text |
| `format` | `invalid_output`: the answer did not match the step's JSON schema after the hub's second try |
| `refused` | `model_refused`, or `model_failed` saying the model declined |
| `unavailable` | `model_paused`: the hub holds that model for quota or load |
| `not_offered` | `model_unavailable`: the extension's connection has no such model |

What is not a model's failure stops the call at once, as before: the hub cannot run any model for the extension
(`assistant_offline`, `permission_denied`, a hub without `model.complete`), or the run itself was cancelled.

When no model answers, the caller gets what it got before routes: a refusal (the batch is split into smaller ones), a
pause (the day waits for the model), or the error. When every model failed on the schema, the last reply the hub
returned anyway goes to the step's own checks of each item, as before. When **none** of the route's models is on the
connection (`not_offered` for each, as on a gateway that names its models differently), the call goes to the purpose's
own model: the route never leaves a new user without a model.

## Long and short

A call's size is estimated before it is sent, from what it sends and what it asks for:

- **input**: the system prompt, the user turn and the answer's JSON schema (the hub writes the schema into the system
  prompt). ASCII counts a quarter token a byte; every other character counts one token. Chinese runs at about a token
  a character, so the estimate errs on the long side for Chinese text and is close for English.
- **output**: the room the step asks for its answer (`max_tokens`; 4096, the hub's default, when unset). The steps
  size it to their answers: 6000 for daily extraction, consolidation and people cards, 3000 to 4000 for triage,
  signals and exit rewriting.

`estimate = input + output`. Above `model_long_above` the call is long.

Why 16000 (decided as recommended): the room for the answer is 3000 to 6000 tokens on every step, so the line falls on
the input. With 16000, the calls that read a lot at once (a busy chat's daily extraction, the corrections pass of the
signals, both around 10k to 20k tokens of input) go to the larger model, where a small model loses detail; triage,
exit rewriting, weekly synthesis and small daily units stay short. The line is a setting: a lower one sends more to
the larger model, a higher one more to the smaller.

## The hub has to take "models"

A call names its model in model.complete's `models` field (a list; the route sends one model at a time, so it can
judge and count each attempt itself). Hubs before the field turn such a call down as invalid params
(`json: unknown field "models"`): the route then sends the call the old way, on the model of the extension's purpose
in Settings › Model configuration, and keeps doing so for an hour before it tries `models` again (a hub upgrade
restarts the extension anyway). Those calls are counted as "sent the old way".

## Settings

| setting | default | |
| --- | --- | --- |
| `model_route` | `chain` | `purpose`: back to the behaviour before 0.2.0-beta.30, every call on the purpose's model |
| `model_primary` | `[]` | tried in order; empty: the size's fallback only |
| `model_long` | `claude-sonnet-5-5` | fallback of a long call |
| `model_short` | `claude-haiku-5-5` | fallback of a short call |
| `model_long_above` | `16000` | the line between short and long, in estimated tokens |
| `model_primary_timeout` | `120` | seconds a primary model's attempt may take before the fallback gets the call |

With no model set at all the route is off.

## Where to see it

- **`enkeep_rerun_status`** (the `enkeep` connector) ends with the route and its counts since they started: calls,
  calls a fallback answered and the fallback rate, calls no model answered, calls sent the old way, and per model the
  attempts, answers (as fallback), failures by reason and the average time of an attempt.
- **`model-routes.json`** in the extension's data directory: the same counts and the last 50 calls.
- **`model-calls.jsonl`** beside it: one line per call: time, step, estimate, size, each model's attempt with its
  reason and seconds, the model that answered, whether it was a fallback, and why a call went the old way. It never
  holds what was sent or answered. At 2 MiB it moves to `model-calls.jsonl.1`.
- The extension also logs one line per call (`enkeep-memory: model call daily: est 14210 tokens (short): answered by
  claude-haiku-5-5 after fallback; tried <primary model> timeout 120s, claude-haiku-5-5 ok 9s`).
