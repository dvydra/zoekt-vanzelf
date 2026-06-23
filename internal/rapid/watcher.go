package rapid

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	}, nil
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
func (w *Watcher) addRecursive(root string) {
	// Gitignored dirs never enter `git status`, so watching them only produces
	// no-op polls. Fetch them once per repo and prune whole subtrees in the
	// walk. Best-effort: on error we fall back to the name-based skips below.
	ignored, err := GetIgnoredDirs(root)
	if err != nil {
		log.Printf("[watcher] %s: ignored-dir lookup failed, watching all: %v", root, err)
		ignored = nil
	}

	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if name == ".git" || w.skipDirs[name] {
			return filepath.SkipDir
		}
		// Prune gitignored subtrees (path != root: a repo root is never ignored).
		if ignored != nil && path != root {
			if rel, relErr := filepath.Rel(root, path); relErr == nil && ignored[rel] {
				return filepath.SkipDir
			}
		}
		if err := w.watcher.Add(path); err != nil {
			log.Printf("[watcher] failed to watch %s: %v", path, err)
		}
		return nil
	})
}

// removeRecursive removes all watches under a repo directory.
func (w *Watcher) removeRecursive(root string) {
	for _, watched := range w.watcher.WatchList() {
		if strings.HasPrefix(watched, root+"/") || watched == root {
			w.watcher.Remove(watched)
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
