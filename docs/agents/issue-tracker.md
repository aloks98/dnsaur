# Issue tracker: Forgejo

Issues and specs live as Forgejo issues on the repo at git.nexus.e412.in (`origin`). Use the `fj` CLI (`~/.cargo/bin/fj`), which reads the repo from the git remote. GitHub is a mirror only; never file there.

## Conventions

- **Create**: `fj issue create "<title>" --body-file <file>` (`--body` for one-liners). Titles follow conventional-commit style, like PR titles.
- **Read**: `fj issue view <n>` for title and body, `fj issue view <n> comments` for the thread.
- **List**: `fj issue search --state open`, with `-l <label>` to filter by label.
- **Comment**: `fj issue comment <n> --body-file <file>`.
- **Labels**: `fj issue edit <n> labels`. A label missing from the repo is created first, in the Forgejo UI or with `POST /api/v1/repos/aloks98/dnsaur/labels`.
- **Close**: `fj issue close <n> --with-msg "<comment>"`.

Where `fj` has no subcommand for something, use the Forgejo REST API at `https://git.nexus.e412.in/api/v1/repos/aloks98/dnsaur/…` with the token `fj` stores.

## Pull requests as a triage surface

**PRs as a request surface: no.** _(Set to `yes` if this repo treats external PRs as feature requests; `/triage` reads this flag.)_

## When a skill says "publish to the issue tracker"

Create a Forgejo issue.

## When a skill says "fetch the relevant ticket"

Run `fj issue view <n>` and `fj issue view <n> comments`.

## Wayfinding operations

Used by `/wayfinder`. The **map** is one issue with **child** issues as tickets.

- **Map**: an issue labelled `wayfinder:map`, holding the Notes / Decisions-so-far / Fog body.
- **Child ticket**: an issue with `Part of #<map>` at the top of its body and a `wayfinder:<type>` label (`research` / `prototype` / `grilling` / `task`); the map body keeps a task list of its children. Once claimed, the ticket is assigned to the driving dev.
- **Blocking**: a `Blocked by: #<n>, #<n>` line at the top of the child body (Forgejo's UI dependencies are welcome too, but the line is what scripts read). A ticket is unblocked when every blocker is closed.
- **Frontier query**: the map's open children with no open blocker and no assignee; first in map order wins.
- **Claim**: `fj issue assign <n> <user>`, the session's first write.
- **Resolve**: comment the answer, `fj issue close <n>`, then append a context pointer to the map's Decisions-so-far.
