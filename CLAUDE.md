# zoekt-vanzelf

Self-contained local code search stack. Installs zoekt, adds live working tree awareness, and includes a CLI, web UI, and Claude Code integration — all from `./install.sh`.

## Architecture

```
neogrok :3000 → zoekt-vanzelf :6071 → zoekt-webserver :6070 → ~/.zoekt/*.zoekt
                     │
                     ├── delta index (in-memory trigram index of dirty files)
                     ├── repo poller (git status every 10s + fsnotify)
                     └── reindex manager (runs zoekt-git-index on branch/HEAD change)
```

zoekt-vanzelf merges results from two sources:
1. **Base index** — zoekt's on-disk trigram shards (built by `zoekt-git-index`)
2. **Delta index** — in-memory trigram index of files modified since the base index

For dirty files, zoekt results are suppressed and replaced with delta results.

## What the installer sets up

`./install.sh` handles the full stack:
- **zoekt** — `zoekt-webserver` + `zoekt-git-index` (from sourcegraph/zoekt)
- **zoekt-vanzelf** — the proxy (built from this repo)
- **neogrok** — web UI on :3000 (optional, requires npm)
- **zoekt-search** — Python CLI for terminal search (`~/.local/bin`)
- **Claude Code** — `/zoekt` slash command + auto-triggered skill
- **launchd agents** — background services that start on login
- **Initial index** — indexes all repos under `~/src` on first run

`./install.sh uninstall` removes agents, symlinks, and skill files.

## Development

```sh
make build                     # build binary to ./zoekt-vanzelf
make test                      # run tests
make install                   # install to GOBIN (needed for launchd)
make setup                     # install + run full installer
```

## Commands

```sh
zoekt-vanzelf serve              # start proxy (discovery + polling + HTTP)
zoekt-vanzelf status             # show all repo states
zoekt-vanzelf status --live      # live dashboard with change highlighting
zoekt-vanzelf reindex [repo]     # trigger reindex (all or specific)
zoekt-vanzelf rescan             # re-discover repos
zoekt-vanzelf discover           # one-shot repo discovery (debug)
zoekt-vanzelf poll               # run polling loop (debug)

zoekt-search 'pattern'           # search from terminal
zoekt-search 'pattern' -s        # compact span output
zoekt-search 'sym:Name lang:go'  # symbol search, filtered by language
```

## Project layout

```
cmd/zoekt-vanzelf/main.go        — CLI entry point and subcommand dispatch
internal/rapid/                  — library code (package rapid):
  config.go                      — configuration with defaults
  discovery.go                   — find git repos under configured roots
  git.go                         — git subprocess helpers
  state.go                       — thread-safe repo state table
  poller.go                      — polling loop (10s repo poll, 60s discovery)
  trigram.go                     — trigram extraction and posting list index
  delta.go                       — delta index build and regex search
  proxy.go                       — zoekt API proxy with delta merge
  server.go                      — HTTP server and API endpoints
  reindex.go                     — reindex manager with concurrency limiting
  scheduler.go                   — hourly full reindex scheduler
  watcher.go                     — fsnotify watcher for instant file change detection
                                   (skips .git, configured skip-dirs, and gitignored subtrees)
skill/                           — CLI and editor integrations:
  zoekt-search                   — Python CLI for terminal search
  SKILL.md                       — Claude Code auto-triggered skill
  zoekt.md                       — Claude Code /zoekt slash command
install.sh                       — one-command installer
```

## How delta merge works

On each poll cycle for each repo:
1. Run `git status --porcelain=v2 --branch` once to get branch, HEAD SHA, and dirty files (one git fork per repo per cycle)
2. Read dirty file contents from working tree
3. Build trigram index of dirty files (rebuild from scratch each cycle)

On search:
1. Forward query to zoekt-webserver
2. For each repo with a delta, query the delta index
3. Suppress zoekt results for dirty paths
4. Inject delta matches into the response

## Launchd agents

Managed via `~/Library/LaunchAgents/com.zoekt.*.plist`:
- `com.zoekt.serve` — zoekt-webserver on :6070
- `com.zoekt.vanzelf` — zoekt-vanzelf on :6071
- `com.zoekt.neogrok` — neogrok on :3000 (points at :6071)

## Configuration defaults

- Roots: `~/src`
- Scan depth: 3
- Proxy port: 6071
- Zoekt URL: `http://localhost:6070`
- Repo poll interval: 10s
- Discovery interval: 60s
- Reindex interval: 1h
- Max concurrent reindex: 2
- Max dirty files per repo: 500
- Max delta bytes per repo: 50MB
