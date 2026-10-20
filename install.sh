#!/usr/bin/env bash
set -euo pipefail

# zoekt-vanzelf installer
# Installs the full code search stack and sets up macOS launchd agents:
#   - zoekt (webserver + git-index)
#   - zoekt-vanzelf (proxy with live working tree search)
#   - neogrok (web UI, optional — requires npm)
#   - zoekt-search CLI (for terminal / Claude Code)
#   - Claude Code skill integration (optional — if ~/.claude exists)

ZOEKT_PORT=6070
VANZELF_PORT=6071
NEOGROK_PORT=3000
ZOEKT_INDEX_DIR="$HOME/.zoekt"
SRC_ROOT="$HOME/src"
LAUNCH_AGENTS="$HOME/Library/LaunchAgents"

# Colors
bold="\033[1m"
green="\033[32m"
yellow="\033[33m"
red="\033[31m"
reset="\033[0m"

info()  { printf "${bold}==> %s${reset}\n" "$*"; }
ok()    { printf "${green}  ✓ %s${reset}\n" "$*"; }
warn()  { printf "${yellow}  ! %s${reset}\n" "$*"; }
die()   { printf "${red}  ✗ %s${reset}\n" "$*"; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# --- Uninstall ---

cmd_uninstall() {
    info "Uninstalling zoekt-vanzelf launchd agents"
    for label in com.zoekt.vanzelf com.zoekt.serve com.zoekt.neogrok; do
        local plist="$LAUNCH_AGENTS/$label.plist"
        if launchctl list "$label" >/dev/null 2>&1; then
            launchctl bootout "gui/$(id -u)" "$plist" 2>/dev/null || true
            ok "Stopped $label"
        fi
        if [ -f "$plist" ]; then
            rm -f "$plist"
            ok "Removed $plist"
        fi
    done

    # Remove zoekt-search symlink
    if [ -L "$HOME/.local/bin/zoekt-search" ]; then
        rm -f "$HOME/.local/bin/zoekt-search"
        ok "Removed zoekt-search from ~/.local/bin"
    fi

    # Remove Claude Code skill
    if [ -L "$HOME/.claude/commands/zoekt.md" ]; then
        rm -f "$HOME/.claude/commands/zoekt.md"
        ok "Removed /zoekt slash command"
    fi
    if [ -d "$HOME/.claude/skills/zoekt" ]; then
        rm -rf "$HOME/.claude/skills/zoekt"
        ok "Removed zoekt skill"
    fi

    echo ""
    info "Uninstall complete"
    echo "  Binaries (zoekt-webserver, zoekt-git-index, zoekt-vanzelf) are still in GOBIN."
    echo "  Index shards are still in $ZOEKT_INDEX_DIR."
    echo "  Remove them manually if you want a full cleanup."
    exit 0
}

# Check for uninstall
if [ "${1:-}" = "uninstall" ]; then
    cmd_uninstall
fi

# --- Preflight checks ---

info "Checking prerequisites"

command -v go >/dev/null 2>&1 || die "Go is not installed. Install from https://go.dev/dl/ or: brew install go"
ok "Go $(go version | awk '{print $3}' | sed 's/go//')"

command -v git >/dev/null 2>&1 || die "git is not installed"
ok "git"

GOBIN="$(go env GOBIN)"
if [ -z "$GOBIN" ]; then
    GOBIN="$(go env GOPATH)/bin"
fi
ok "GOBIN: $GOBIN"

# Ensure GOBIN is in PATH for the rest of this script.
export PATH="$GOBIN:$PATH"

# --- Install zoekt ---

info "Installing zoekt"

if command -v zoekt-webserver >/dev/null 2>&1; then
    ok "zoekt-webserver already installed"
else
    echo "  Installing zoekt-webserver..."
    go install github.com/sourcegraph/zoekt/cmd/zoekt-webserver@latest
    ok "zoekt-webserver"
fi

if command -v zoekt-git-index >/dev/null 2>&1; then
    ok "zoekt-git-index already installed"
else
    echo "  Installing zoekt-git-index..."
    go install github.com/sourcegraph/zoekt/cmd/zoekt-git-index@latest
    ok "zoekt-git-index"
fi

# --- Install zoekt-vanzelf ---

info "Installing zoekt-vanzelf"

if [ -f "$SCRIPT_DIR/go.mod" ] && grep -q "zoekt-vanzelf" "$SCRIPT_DIR/go.mod" 2>/dev/null; then
    echo "  Building from source..."
    (cd "$SCRIPT_DIR" && go install ./cmd/zoekt-vanzelf)
else
    echo "  Installing from module..."
    go install github.com/dvydra/zoekt-vanzelf/cmd/zoekt-vanzelf@latest
fi
ok "zoekt-vanzelf → $GOBIN/zoekt-vanzelf"

# Build the PATH that launchd agents will use (they don't inherit the shell's).
# zoekt-git-index may live outside the current GOBIN (a symlink in ~/.local/bin,
# or an older Go's bin dir) and zoekt-vanzelf execs it by name, so its directory
# goes on the agent PATH explicitly. Without it every reindex fails with
# "executable file not found in $PATH".
ZOEKT_INDEX_BIN_DIR="$(dirname "$(command -v zoekt-git-index)")"
AGENT_PATH="$GOBIN:$ZOEKT_INDEX_BIN_DIR:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

# --- Install neogrok (optional) ---

info "Installing neogrok (web UI)"

if command -v neogrok >/dev/null 2>&1; then
    ok "neogrok already installed"
    HAS_NEOGROK=true
elif command -v npm >/dev/null 2>&1; then
    echo "  Installing via npm..."
    npm install -g neogrok 2>/dev/null
    if command -v neogrok >/dev/null 2>&1; then
        ok "neogrok"
        HAS_NEOGROK=true
    else
        warn "npm install succeeded but neogrok not found on PATH"
        HAS_NEOGROK=false
    fi
else
    warn "npm not found — skipping neogrok (install Node.js to get the web UI)"
    HAS_NEOGROK=false
fi

# --- Install zoekt-search CLI ---

info "Installing zoekt-search CLI"

BIN_DIR="$HOME/.local/bin"
mkdir -p "$BIN_DIR"
chmod +x "$SCRIPT_DIR/skill/zoekt-search"
ln -sf "$SCRIPT_DIR/skill/zoekt-search" "$BIN_DIR/zoekt-search"
ok "zoekt-search → $BIN_DIR/zoekt-search"

# Check if ~/.local/bin is on PATH
if ! echo "$PATH" | tr ':' '\n' | grep -qx "$BIN_DIR"; then
    warn "$BIN_DIR is not on your PATH"
    warn "Add to your shell profile: export PATH=\"\$HOME/.local/bin:\$PATH\""
fi

# --- Install Claude Code skill (optional) ---

if [ -d "$HOME/.claude" ]; then
    info "Installing Claude Code integration"

    # Slash command: /zoekt
    COMMANDS_DIR="$HOME/.claude/commands"
    mkdir -p "$COMMANDS_DIR"
    ln -sf "$SCRIPT_DIR/skill/zoekt.md" "$COMMANDS_DIR/zoekt.md"
    ok "/zoekt slash command"

    # Auto-triggered skill
    SKILLS_DIR="$HOME/.claude/skills/zoekt"
    mkdir -p "$SKILLS_DIR"
    ln -sf "$SCRIPT_DIR/skill/SKILL.md" "$SKILLS_DIR/SKILL.md"
    ok "zoekt auto-skill (triggers on code search)"
fi

# --- Initial index ---

info "Setting up index directory"
mkdir -p "$ZOEKT_INDEX_DIR"
ok "$ZOEKT_INDEX_DIR"

if [ -d "$SRC_ROOT" ] && [ -z "$(ls "$ZOEKT_INDEX_DIR"/*.zoekt 2>/dev/null | head -1)" ]; then
    info "Running initial index of repos under $SRC_ROOT"
    echo "  This may take a few minutes on first run..."

    find "$SRC_ROOT" -maxdepth 3 -name .git -type d 2>/dev/null | sort | while read -r gitdir; do
        repo="$(dirname "$gitdir")"
        name="${repo#"$SRC_ROOT"/}"
        printf "  Indexing %s..." "$name"
        if zoekt-git-index -index "$ZOEKT_INDEX_DIR" "$repo" 2>/dev/null; then
            printf " ${green}done${reset}\n"
        else
            printf " ${yellow}skipped${reset}\n"
        fi
    done
    ok "Initial index complete"
elif [ ! -d "$SRC_ROOT" ]; then
    warn "No $SRC_ROOT directory found — skipping initial index"
    warn "Create it and run: zoekt-vanzelf reindex"
else
    ok "Index already populated, skipping initial index"
fi

# --- Set up launchd agents ---

info "Setting up launchd agents"
mkdir -p "$LAUNCH_AGENTS"

ZOEKT_WEBSERVER_BIN="$(command -v zoekt-webserver)"
ZOEKT_VANZELF_BIN="$(command -v zoekt-vanzelf)"

# Helper to write a plist, unload old version if present, and load the new one.
write_agent() {
    local label="$1"
    local plist_path="$LAUNCH_AGENTS/$label.plist"
    local content="$2"

    # Unload by label (handles label/filename mismatches from old installs)
    if launchctl list "$label" >/dev/null 2>&1; then
        launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
    fi
    # Also try bootout by plist path in case the label changed
    [ -f "$plist_path" ] && launchctl bootout "gui/$(id -u)" "$plist_path" 2>/dev/null || true

    echo "$content" > "$plist_path"
    launchctl bootstrap "gui/$(id -u)" "$plist_path"
    ok "$label"
}

# --- zoekt-webserver ---
write_agent "com.zoekt.serve" "<?xml version=\"1.0\" encoding=\"UTF-8\"?>
<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://apple.com/DTDs/PropertyList-1.0.dtd\">
<plist version=\"1.0\">
<dict>
    <key>Label</key>
    <string>com.zoekt.serve</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>$AGENT_PATH</string>
    </dict>
    <key>ProgramArguments</key>
    <array>
        <string>$ZOEKT_WEBSERVER_BIN</string>
        <string>-index</string>
        <string>$ZOEKT_INDEX_DIR</string>
        <string>-listen</string>
        <string>:$ZOEKT_PORT</string>
        <string>-rpc</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/zoekt-serve.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/zoekt-serve.log</string>
</dict>
</plist>"

# --- zoekt-vanzelf ---
write_agent "com.zoekt.vanzelf" "<?xml version=\"1.0\" encoding=\"UTF-8\"?>
<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://apple.com/DTDs/PropertyList-1.0.dtd\">
<plist version=\"1.0\">
<dict>
    <key>Label</key>
    <string>com.zoekt.vanzelf</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>$AGENT_PATH</string>
    </dict>
    <key>ProgramArguments</key>
    <array>
        <string>$ZOEKT_VANZELF_BIN</string>
        <string>serve</string>
        <string>-port</string>
        <string>$VANZELF_PORT</string>
        <string>-zoekt</string>
        <string>http://localhost:$ZOEKT_PORT</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/zoekt-vanzelf.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/zoekt-vanzelf.log</string>
</dict>
</plist>"

# --- neogrok (if available) ---
if [ "$HAS_NEOGROK" = true ]; then
    NEOGROK_BIN="$(command -v neogrok)"
    NEOGROK_BIN_DIR="$(dirname "$NEOGROK_BIN")"
    NEOGROK_AGENT_PATH="$NEOGROK_BIN_DIR:$AGENT_PATH"

    write_agent "com.zoekt.neogrok" "<?xml version=\"1.0\" encoding=\"UTF-8\"?>
<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://apple.com/DTDs/PropertyList-1.0.dtd\">
<plist version=\"1.0\">
<dict>
    <key>Label</key>
    <string>com.zoekt.neogrok</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>$NEOGROK_AGENT_PATH</string>
        <key>ZOEKT_URL</key>
        <string>http://localhost:$VANZELF_PORT</string>
        <key>PORT</key>
        <string>$NEOGROK_PORT</string>
    </dict>
    <key>ProgramArguments</key>
    <array>
        <string>$NEOGROK_BIN</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/zoekt-neogrok.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/zoekt-neogrok.log</string>
</dict>
</plist>"
fi

# --- Clean up old agents from previous installs ---

# The old com.zoekt.index agent is redundant — zoekt-vanzelf handles reindexing.
# The old com.zoekt.rapid label was the pre-rename version of com.zoekt.vanzelf.
for old_label in com.zoekt.index com.zoekt.rapid; do
    if launchctl list "$old_label" >/dev/null 2>&1; then
        launchctl bootout "gui/$(id -u)/$old_label" 2>/dev/null || true
        ok "Removed obsolete agent: $old_label"
    fi
    # bootout alone leaves the plist behind, and launchd loads it again at
    # next login.
    rm -f "$HOME/Library/LaunchAgents/$old_label.plist"
done

# --- Done ---

echo ""
info "Installation complete"
echo ""
echo "  Services:"
echo "    zoekt-webserver  http://localhost:$ZOEKT_PORT  (base trigram index)"
echo "    zoekt-vanzelf    http://localhost:$VANZELF_PORT  (proxy with live working tree)"
if [ "$HAS_NEOGROK" = true ]; then
echo "    neogrok          http://localhost:$NEOGROK_PORT  (web UI)"
fi
echo ""
echo "  CLI:"
echo "    zoekt-search 'pattern'       search from terminal"
echo "    zoekt-search 'pat' -s -n 10  compact span output"
echo "    zoekt-vanzelf status         show tracked repos"
echo ""
echo "  Logs:"
echo "    tail -f /tmp/zoekt-serve.log"
echo "    tail -f /tmp/zoekt-vanzelf.log"
if [ "$HAS_NEOGROK" = true ]; then
echo "    tail -f /tmp/zoekt-neogrok.log"
fi
echo ""
echo "  Manage:"
echo "    ./install.sh             re-run to upgrade"
echo "    ./install.sh uninstall   remove launchd agents"
