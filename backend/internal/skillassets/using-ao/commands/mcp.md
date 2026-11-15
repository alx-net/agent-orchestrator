# ao mcp

Connect, check, and manage the MCP servers Claude Code agents use.

When a server you need is not connected, connect it yourself: run
`ao mcp login <server>`. Only the browser approval is left for the user, and the
browser opens by itself. Do not ask the user to run `claude`, open `/mcp`, paste
commands into a terminal, or restart anything.

## Syntax

```text
ao mcp ls [--refresh] [--json]
ao mcp login <server> [--account <id>]
ao mcp test <server> [--account <id>] [--json]
ao mcp add <name> <url> [--header "Name: value"]... [--transport http|sse]
ao mcp add <name> [--env NAME=value]... -- <command> [args...]
ao mcp rm <name>
```

Inside a session, `ls`, `login` and `test` act for that session's subscription and
folder. Outside one they cover every subscription, or `--account <id>`.

## What each does

- `ao mcp ls`: Inside a session, what your running agent has right now, with each
  server's status, tool count, how it authenticates, and the last error. Outside one,
  every server on every subscription.
- `ao mcp login <server>`: Starts the sign-in, prints
  `Approve it in the browser that just opened`, and blocks until it finishes or five
  minutes pass. Then your session has the server's tools in the same turn, with no
  restart. If no browser opened, it prints the link: give it to the user.
- `ao mcp test <server>`: Really connects to that server and lists its tools. Prints
  `PASS` or `FAIL` with the reason, and exits 1 unless it passes. Use it to verify a
  server instead of asking the user. Costs no tokens.
- `ao mcp add`: Adds a server to this computer's Claude Code config, which every
  subscription shares. Running agents get it right away. It refuses a name that exists.
- `ao mcp rm`: Removes a server from that config. A server in a repo's `.mcp.json`
  belongs to the repo; edit that file instead.

## Which server needs what

The status says what to do. It depends on how the server authenticates, never on its
name.

| Status | Meaning | Do |
|---|---|---|
| `connected` | It works. | Nothing. |
| `needs sign-in` | A browser sign-in, per subscription. | `ao mcp login <server>` |
| `key rejected` | The server refused the key in its config. | Tell the user. They can change it in Settings, MCP servers. |
| `needs approval` | A repo `.mcp.json` server nobody approved. | `ao mcp login` approves it as it connects. Otherwise the user approves it in Settings. |
| `failed` | It did not connect. The detail is the real error. | Read it. A local server's error usually names what is missing. |
| anything else | Claude Code's own word for it. | Read it, and test with `ao mcp test <server>`. |

A `claude.ai` connector (Gmail, Drive) signs in on claude.ai; `ao mcp login` opens that
page and connects by itself when the user finishes there.

## Restarts

Nobody restarts anything. A sign-in, a new server, or a new key reaches running agents
in place. Removing a server needs a restart, so AO does it: the agent restarts with its
conversation kept as soon as it is idle, never during a turn, with one quiet line in the
chat.

## Examples

```bash
ao mcp login linear
ao mcp test linear
ao mcp add acme https://mcp.acme.dev/mcp --header "X-Api-Key: abc123"
ao mcp add files --env ROOT=/data -- npx -y @acme/files-mcp
```

Never put a key in a chat message or a commit. `--header` and `--env` values go only to
Claude Code's config, and AO does not log them.
