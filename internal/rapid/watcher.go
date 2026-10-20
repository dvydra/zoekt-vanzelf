package rapid

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher uses fsnotify to react to file changes instantly,
// triggering a poll for the affected repo without waiting for
// the next 2s poll cycle.
type Watcher struct {
	poller   *Poller
	state    *StateTable
	watcher  *fsnotify.Watcher
	skipDirs map[string]bool // directory names to skip when watching

	mu       sync.Mutex
	repoDirs map[string]bool // watched repo paths

	// On kqueue platforms (macOS, BSDs) fsnotify holds an open fd for every
	// watched directory and every entry in it, so a large tree can fill the
	// process's fd table. Once that happens every git subprocess fails with
	// EMFILE and repo tracking collapses. fdBudget caps what the watcher may
	// hold; repos that don't fit fall back to the poll interval. 0 = no cap.
	fdBudget     int
	fdUsed       int
	fdCost       map[string]int // watched dir → fds it holds
	budgetWarned bool
}

func NewWatcher(poller *Poller, state *StateTable, skipDirs []string) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	skip := make(map[string]bool, len(skipDirs))
	for _, d := range skipDirs {
		skip[d] = true
	}
	return &Watcher{
		poller:   poller,
		state:    state,
		watcher:  fsw,
		skipDirs: skip,
		repoDirs: make(map[string]bool),
		fdBudget: watchFDBudget(),
		fdCost:   make(map[string]int),
	}, nil
}

// watchFDBudget returns how many fds the watcher may hold: the soft
// RLIMIT_NOFILE, which the Go runtime has already raised to the hard limit,
// minus headroom for git subprocesses, sockets and logs. Returns 0 (no cap)
// on platforms where watches don't consume fds.
func watchFDBudget() int {
	switch runtime.GOOS {
	case "darwin", "freebsd", "netbsd", "openbsd", "dragonfly":
	default:
		return 0
	}
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0
	}
	const headroom = 4096
	cur := int(lim.Cur)
	if cur <= 0 || cur <= 2*headroom {
		return cur / 2
	}
	return cur - headroom
}

// Sync adds/removes watches to match the current set of managed repos.
func (w *Watcher) Sync() {
	w.mu.Lock()
	defer w.mu.Unlock()

	current := make(map[string]bool)
	for _, path := range w.state.Paths() {
		current[path] = true
	}

	// Add new repos (recursively watch all subdirectories).
	for path := range current {
		if !w.repoDirs[path] {
			w.addRecursive(path)
		}
	}

	// Remove stale repos.
	for path := range w.repoDirs {
		if !current[path] {
			w.removeRecursive(path)
		}
	}

	w.repoDirs = current
}

// Run processes fsnotify events and triggers repo polls. Blocks until closed.
func (w *Watcher) Run() {
	// Debounce: batch rapid edits (e.g. save-all) into one poll per repo.
	pending := make(map[string]time.Time)
	var mu sync.Mutex
	debounce := 50 * time.Millisecond
	done := make(chan struct{})

	// Flush goroutine checks pending repos and polls them.
	go func() {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			mu.Lock()
			now := time.Now()
			var ready []string
			for repo, deadline := range pending {
				if now.After(deadline) {
					ready = append(ready, repo)
				}
			}
			for _, repo := range ready {
				delete(pending, repo)
			}
			mu.Unlock()

			for _, repo := range ready {
				w.poller.pollRepo(repo)
			}
		}
	}()

	defer close(done)

	for {
		select {
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			// Skip .git internal changes.
			if strings.Contains(event.Name, "/.git/") {
				continue
			}
			// Find which repo this file belongs to.
			repo := w.repoForPath(event.Name)
			if repo == "" {
				continue
			}
			mu.Lock()
			pending[repo] = time.Now().Add(debounce)
			mu.Unlock()

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[watcher] error: %v", err)
		}
	}
}

// Close shuts down the watcher.
func (w *Watcher) Close() error {
	return w.watcher.Close()
}

// addRecursive walks a repo directory and adds watches for all subdirectories,
// skipping .git, the configured skip-dir names, and any gitignored directory.
// The walk stops for good once the fd budget is spent.
func (w *Watcher) addRecursive(root string) {
	// Gitignored dirs never enter `git status`, so watching them only produces
	// no-op polls. Fetch them once per repo and prune whole subtrees in the
	// walk. Best-effort: on error we fall back to the name-based skips below.
	ignored, err := GetIgnoredDirs(root)
	if err != nil {
		log.Printf("[watcher] %s: ignored-dir lookup failed, watching all: %v", root, err)
		ignored = nil
	}
	w.walkAndWatch(root, root, ignored)
}

// walkAndWatch watches dir and recurses into its subdirectories. It returns
// false once the fd budget is spent so callers stop walking.
func (w *Watcher) walkAndWatch(root, dir string, ignored map[string]bool) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true // unreadable dir: skip it, keep going
	}

	// kqueue opens the directory itself plus every entry in it.
	cost := 1 + len(entries)
	if w.fdBudget > 0 && w.fdUsed+cost > w.fdBudget {
		if !w.budgetWarned {
			w.budgetWarned = true
			log.Printf("[watcher] fd budget of %d spent at %s; this and later repos rely on polling instead of instant change detection", w.fdBudget, root)
		}
		return false
	}
	if err := w.watcher.Add(dir); err != nil {
		log.Printf("[watcher] failed to watch %s: %v", dir, err)
	} else {
		w.fdUsed += cost
		w.fdCost[dir] = cost
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == ".git" || w.skipDirs[name] {
			continue
		}
		sub := filepath.Join(dir, name)
		// Prune gitignored subtrees (a repo root is never ignored).
		if ignored != nil {
			if rel, relErr := filepath.Rel(root, sub); relErr == nil && ignored[rel] {
				continue
			}
		}
		if !w.walkAndWatch(root, sub, ignored) {
			return false
		}
	}
	return true
}

// removeRecursive removes all watches under a repo directory and gives their
// fds back to the budget.
func (w *Watcher) removeRecursive(root string) {
	for _, watched := range w.watcher.WatchList() {
		if strings.HasPrefix(watched, root+"/") || watched == root {
			w.watcher.Remove(watched)
			w.fdUsed -= w.fdCost[watched]
			delete(w.fdCost, watched)
		}
	}
}

// repoForPath finds the managed repo that contains the given file path.
func (w *Watcher) repoForPath(path string) string {
	w.mu.Lock()
	defer w.mu.Unlock()

	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}

	for repo := range w.repoDirs {
		if strings.HasPrefix(abs, repo+"/") || abs == repo {
			return repo
		}
	}
	return ""
}
