# zoekt-vanzelf

Search proxy that makes your uncommitted code searchable *instantly*. Sits in front of [zoekt-webserver](https://github.com/sourcegraph/zoekt) and adds working tree awareness — edits, new files, and deletions appear in search results within 2 seconds, no reindex required.

## The problem

Zoekt builds trigram indexes from git commits. That's great for searching across dozens of repos, but it means your working tree changes are invisible until you commit and wait for a reindex cycle. If you're iterating fast — renaming a function, adding a file, exploring unfamiliar code — the search results are always one step behind.

## How it works

```
neogrok :3000 → zoekt-vanzelf :6071 → zoekt-webserver :6070 → ~/.zoekt/*.zoekt
                     │
                     ├── delta index   (in-memory trigram index of dirty files)
                     ├── repo poller   (git status every 2s + fsnotify)
                     ├── remote refs   (for-each-ref every 60s, catches fetches)
                     └── reindex mgr   (zoekt-git-index on branch/HEAD/origin change)
```

zoekt-vanzelf merges results from two sources:

1. **Base index** — zoekt's on-disk trigram shards (the full corpus, built by `zoekt-git-index`)
2. **Delta index** — lightweight in-memory trigram index of files modified since the last commit

On every search request, zoekt-vanzelf forwards the query to zoekt, then **suppresses stale results** for dirty files and **injects fresh matches** from the delta index. The merge is transparent — clients see a single unified result set.

### What triggers updates

| Event | Detection | Latency |
|-------|-----------|---------|
| File edit/create/delete | fsnotify + git status | ~50ms |
| Branch switch | git status poll (2s) | ~2s |
| New commit (HEAD change) | git status poll → reindex | seconds–minutes |
| `origin/main` moved by a fetch | remote ref poll (60s) → reindex | ~60s + index time |
| New repo appears under `~/src` | discovery poll (60s) | ~60s |

### Branches

Shards carry two branches: `HEAD` (your checkout) and the remote default branch
(`origin/main`). So search finds work other people merged but you haven't pulled,
as well as your own unpushed commits. `branch:origin/main` narrows a query to
pushed state.

## Install

One command sets up the full code search stack:

```sh
git clone https://github.com/dvydra/zoekt-vanzelf
cd zoekt-vanzelf
./install.sh
```

The installer handles everything:

1. **zoekt** — installs `zoekt-webserver` and `zoekt-git-index` from [sourcegraph/zoekt](https://github.com/sourcegraph/zoekt)
2. **zoekt-vanzelf** — builds the proxy from source
3. **neogrok** — installs the [web UI](https://github.com/nicholasgasior/neogrok) (requires npm; skipped if unavailable)
4. **zoekt-search** — installs a CLI search tool to `~/.local/bin`
5. **Initial index** — indexes all git repos under `~/src` (first run only)
6. **launchd agents** — creates and starts background services that run on login
7. **Claude Code integration** — installs `/zoekt` slash command and auto-skill (if `~/.claude` exists)

After install, verify with:
```sh
zoekt-vanzelf status
```

### Prerequisites

- **Go 1.26+** — install from [go.dev](https://go.dev/dl/) or `brew install go`
- **macOS** — the installer uses launchd for service management
- **Node.js** (optional) — for the neogrok web UI

### Uninstall

```sh
./install.sh uninstall
```

Removes launchd agents, the `zoekt-search` symlink, and Claude Code skill files. Leaves binaries in GOBIN and index shards in `~/.zoekt` for manual cleanup.

### Manual install

If you prefer to set things up yourself:

```sh
# Install zoekt (the base search engine)
go install github.com/sourcegraph/zoekt/cmd/zoekt-webserver@latest
go install github.com/sourcegraph/zoekt/cmd/zoekt-git-index@latest

# Start zoekt-webserver
zoekt-webserver -index ~/.zoekt -listen :6070 -rpc

# Index your repos (repeat for each repo, or let zoekt-vanzelf handle it)
zoekt-git-index -index ~/.zoekt ~/src/myproject

# Install and start zoekt-vanzelf
go install github.com/dvydra/zoekt-vanzelf/cmd/zoekt-vanzelf@latest
zoekt-vanzelf serve
```

## Usage

### zoekt-search CLI

The fastest way to search from the terminal:

```sh
zoekt-search 'HandleRequest'              # full output with line matches
zoekt-search 'HandleRequest' -s           # compact spans (file:line-range)
zoekt-search 'HandleRequest' -f           # filenames only
zoekt-search 'sym:Config' -s -n 10        # symbol search, limit 10 results
zoekt-search 'pattern lang:go'            # filter by language
zoekt-search 'pattern repo:myproject'     # filter by repo
zoekt-search 'pattern' -r myproject       # same, via flag
```

Set `ZOEKT_URL` to override the default endpoint (`http://localhost:6071`).

### Claude Code

If Claude Code is installed, the installer adds:
- `/zoekt <query>` — slash command that spawns a search subagent
- Auto-triggered skill — Claude automatically prefers zoekt for code search tasks

### zoekt-vanzelf commands

```sh
zoekt-vanzelf serve                # start the proxy
zoekt-vanzelf status               # show tracked repos
zoekt-vanzelf status --live        # live dashboard with change highlighting
zoekt-vanzelf reindex myproject    # trigger reindex for a specific repo
zoekt-vanzelf reindex              # trigger reindex for all repos
zoekt-vanzelf rescan               # re-discover repos
zoekt-vanzelf version              # print version
```

### Flags

```sh
zoekt-vanzelf serve \
  --roots ~/src,~/work \     # directories to scan for git repos
  --port 6071 \              # proxy listen port
  --zoekt http://localhost:6070  # upstream zoekt URL
```

## Configuration defaults

| Setting | Default | Description |
|---------|---------|-------------|
| Roots | `~/src` | Directories to scan for git repos |
| Scan depth | 3 | How deep to look for `.git` directories |
| Proxy port | 6071 | HTTP listen port |
| Zoekt URL | `http://localhost:6070` | Upstream zoekt-webserver |
| Repo poll | 2s | How often to check each repo for changes |
| Discovery poll | 60s | How often to scan for new/removed repos |
| Full reindex | 1h | Periodic full reindex of all repos |
| Max concurrent reindex | 2 | Parallel `zoekt-git-index` jobs |
| Max dirty files | 500 | Per-repo delta threshold before early reindex |
| Max delta size | 50MB | Per-repo delta size threshold |

## How delta merge works

On each poll cycle, for each repo:
1. Run `git status --porcelain=v2` to get the dirty file list
2. Read file contents from the working tree
3. Build a trigram index of dirty files (rebuilt from scratch each cycle)

On search:
1. Forward the query to zoekt-webserver
2. For repos with deltas, suppress zoekt results for dirty file paths
3. Run the query against the delta index
4. Merge delta matches into the response

This means zoekt handles the heavy lifting (searching millions of lines across all repos) while zoekt-vanzelf patches in just the handful of files you've touched since your last commit.

## macOS launchd agents

The installer creates launchd agents in `~/Library/LaunchAgents/`:

| Agent | Service | Port | Log |
|-------|---------|------|-----|
| `com.zoekt.serve` | zoekt-webserver | 6070 | `/tmp/zoekt-serve.log` |
| `com.zoekt.vanzelf` | zoekt-vanzelf proxy | 6071 | `/tmp/zoekt-vanzelf.log` |
| `com.zoekt.neogrok` | neogrok web UI | 3000 | `/tmp/zoekt-neogrok.log` |

All are set to `RunAtLoad` + `KeepAlive` — they start on login and restart if they crash.

```sh
# Restart zoekt-vanzelf
launchctl kickstart -k gui/$(id -u)/com.zoekt.vanzelf

# Stop it
launchctl kill SIGTERM gui/$(id -u)/com.zoekt.vanzelf

# View logs
tail -f /tmp/zoekt-vanzelf.log

# Re-run installer to upgrade
./install.sh

# Remove everything
./install.sh uninstall
```

## Project layout

```
cmd/zoekt-vanzelf/        CLI entry point
internal/rapid/           Library code
  config.go               Configuration and defaults
  discovery.go            Git repo discovery under configured roots
  git.go                  Git subprocess helpers
  state.go                Thread-safe repo state table
  poller.go               Polling loop (2s repo poll, 60s discovery)
  watcher.go              fsnotify for instant file change detection
  trigram.go              Trigram extraction and posting lists
  delta.go                Delta index build and regex search
  proxy.go                Zoekt API proxy with delta merge
  server.go               HTTP server and API endpoints
  reindex.go              Reindex manager with concurrency limiting
  scheduler.go            Periodic full reindex scheduler
skill/                    CLI and editor integrations
  zoekt-search            Python CLI for searching from terminal
  SKILL.md                Claude Code auto-triggered skill
  zoekt.md                Claude Code /zoekt slash command
install.sh                One-command installer
```

## License

MIT
