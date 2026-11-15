# ao wait

Wait for a PR's CI, merge, or review, or for a timer, without polling. Register
what you are waiting for, then end your turn. AO messages this session once,
with the complete result, when it happens.

Every poll (`sleep 30; gh pr view --json statusCheckRollup`, `gh run view`,
`gh pr checks --watch`) is a full-context model call. A wait costs nothing
until it fires.

## Syntax

```text
ao wait pr <number|url> [--for checks|merge|review|any] [--note "<why>"] [--timeout <duration>]
ao wait time <duration> [--note "<why>"]
ao wait ls
ao wait cancel <wait-id>
```

Run it inside an AO session. The wait always wakes the session that registered
it. `<number>` resolves in this session's project repository; pass the full PR
or MR URL for another repository or a standalone session.

| `--for` | Wakes when |
|---|---|
| `checks` (default) | Every check on the PR's current head has finished. A push while you wait moves the wait to the new head. |
| `merge` | The PR is merged or closed. |
| `review` | A new review or review comment arrives (your own thread replies do not count). |
| `any` | The first of the above. |

Every PR wait also wakes when the PR is merged or closed. A PR wait expires
after 24 hours unless `--timeout` sets another limit, such as `6h` or `2d`.
The wake says when it expired. Durations accept `90s`, `20m`, `1h30m`, or `2d`.

## What to do

1. Run the command. It returns at once:
   `Waiting (w-1a2b3c4d): AO will message this session when CI on #5807 finishes. End your turn now.`
2. End your turn. Do not sleep, poll, or re-check the PR yourself.
3. The wake starts your next turn. It begins with `[AO wait w-1a2b3c4d]` and
   carries what polling would have told you. That includes the PR title and
   head SHA, the overall result, and each failed check with its log tail or
   summary. It also has the merge and review state, new review comments
   (author, `file:line`, excerpt), and your `--note`. Act on it directly. Fetch
   full logs only if the tail is not enough.

Use `--note` to remind your future turn why it waited, for example
`--note "then merge #5810 on top"`.

A wait resolves once. After you push a fix, run `ao wait pr <n>` again.
Repeating an identical pending wait returns the existing one
(`Already waiting (...)`) instead of adding a second wake.

`ao wait ls` lists this session's pending waits. `ao wait cancel <wait-id>`
removes one without waking the session.

**Examples:**

```bash
git push
ao wait pr 5807 --note "if green, report done"
# end the turn here

ao wait pr 5807 --for merge --note "rebase #5810 onto main after this lands"
ao wait pr https://github.com/acme/api/pull/412 --for review --timeout 6h
ao wait time 20m --note "check whether the canary deploy finished"
```
