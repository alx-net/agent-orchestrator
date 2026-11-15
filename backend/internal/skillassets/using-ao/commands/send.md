# ao send

Send a message to a running agent session without killing and respawning it.

Plain `ao send` delivers a separate message. If a Chat session is busy with a
running turn, the message is queued and delivered as its own turn after the
current turn ends; it does not reach the running turn. To correct or redirect
an agent mid-turn, use `--steer`. For information the recipient does not need
to act on now, use `--fyi`: it does not wake the recipient.

Every delivered message starts a full turn for the recipient, so send one only
when it must act, and then include everything it needs: what to do, the
evidence (failing output, file, PR, or commit references), and the expected
result. Never send a message that only acknowledges, thanks, or confirms
receipt, and do not relay CI or PR news: AO already delivers CI failures and
review comments to the session that owns the PR.

## Syntax

```
ao send [flags]
```

## Flags

| Flag | Meaning | Default / Required |
|---|---|---|
| `--message string` | Message body | Required unless `--recover-only` |
| `--session string` | Session id | Required |
| `--steer` | Add guidance to the active Chat turn, or start a normal turn when idle | Optional |
| `--fyi` | Hold the message for the session's next turn instead of starting one | Optional; not with `--steer` |
| `--client-message-id string` | Stable delivery handle for steering and safe retries | Generated automatically with `--steer` |
| `--recover-only` | Recover the outcome for an existing delivery handle without resending | Requires `--steer` and `--client-message-id` |

## Informational messages (`--fyi`)

`--fyi` stores the message durably and never starts a turn. AO appends every
waiting FYI, in arrival order, to the next turn the recipient starts, whatever
starts it: a user message, a plain `ao send`, a worker report, or a CI or
review event. Each one is labeled `[fyi]` with its sender and the time it was
sent, and is delivered once. The command prints how many FYIs are waiting for
that session.

Use it for context the recipient should have before its next decision but
need not act on now, such as "PR #88 merged; the API change is on main". An
`[fyi]` entry you receive needs no reply; act on it only if it changes your
work. If the recipient must act, or a blocker or decision cannot wait, send a
normal message instead.

## Steering and recovery

`--steer` uses one atomic daemon operation. If the session has an active Chat
turn, the message is added to that turn. If the session is idle, the message
starts a normal Chat turn. The state cannot change between those decisions and
leave the message queued as a separate correction.

Every steering request has a client message id. Supply one explicitly when a
caller must retain the handle across retries:

```bash
ao send --session mer-3 --steer --client-message-id correction-42 --message "Focus only on the backend."
```

If the command reports an uncertain outcome or a transport failure, preserve
the delivery handle printed in the error. Recover the existing result without
contacting the provider again:

```bash
ao send --session mer-3 --steer --recover-only --client-message-id correction-42
```

Do not invent a new client message id after an uncertain result. Reusing the
original id lets the daemon return the durable steering receipt or normal turn
without delivering the message twice. Recovery confirms the recorded delivery
outcome, not that the agent acted on the guidance.

## Examples

```bash
# Correct a running agent mid-turn (starts a normal turn if it is idle)
ao send --session mer-3 --steer --message "Focus only on the backend; ignore frontend files."
```

```bash
# Give the agent new instructions mid-task
ao send --session mer-3 --steer --message "The issue is in session_manager.go line 142, not in the CLI. Investigate there."
```

```bash
# Send a separate message (queued behind a running Chat turn)
ao send --session mer-3 --message "When you finish, also update the changelog."
```

```bash
# Share context without waking the recipient; it arrives with its next turn
ao send --session mer-orchestrator --fyi --message "Rebased onto main after #88 merged; no conflicts."
```
