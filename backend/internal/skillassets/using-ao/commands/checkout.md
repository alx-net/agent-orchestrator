# ao checkout

Get this session's own up-to-date copy of one of the user's AO projects. Use it
whenever a task needs code from the user's repositories, instead of searching
the disk, package caches, or the network, or cloning by hand.

## Syntax

```text
ao checkout <project> [--ref <branch|tag|sha>] [--refresh] [--json]
```

Run it inside an AO session. `<project>` is a project id or name, matched
case-insensitively; an unknown name lists the available projects.
`ao project ls` shows them all.

## What it does

1. Fetches the project's `origin` (no submodules, no dependency installs, no
   project setup scripts).
2. Creates a detached Git worktree of the default branch, such as
   `origin/master`, at `repos/<project-id>` in this session's workspace.
   `--ref` picks a remote branch, a tag, or a commit SHA instead.
3. Prints the path, the ref, and the commit with its date and subject:

```text
Checked out KugelAudio (kugelaudio) at origin/master.
path:   /Users/me/.ao/data/worktrees/standalone/sessions/sa-12/repos/kugelaudio
commit: 1a2b3c4 2026-10-29 14:02 +0100 feat: add curb limiter
Work there (cd into the path). It is a detached checkout: run `git switch -c <branch>` before you commit.
```

Work in the printed path. Do not edit the user's own checkout of the project.
Read-only lookups there are fine. The user's working tree, branches, and index
are never changed.

## Running it again

Run it again to bring the copy up to date. A clean copy moves to the latest
commit of the ref. A copy with uncommitted changes, a branch, or commits no
branch contains is left untouched, and the output says why. With `--refresh`,
that case fails instead, so you know the copy is not current.

## Cleanup

AO removes the session's checkouts, and their entries in the source
repository, when the session is cleaned up. A checkout that still holds
uncommitted changes or unpushed detached commits is kept, together with the
session workspace. Commits on a branch you created stay in the source
repository.

Submodules are not initialized. If you need one, run
`git submodule update --init <path>` inside the checkout. `--setup` is not
supported: run only the commands the task needs.

**Examples:**

```bash
ao checkout kugelaudio
ao checkout KugelAudio --ref release/2.3
ao checkout kugelaudio --refresh   # fail if the copy cannot be brought up to date
```
