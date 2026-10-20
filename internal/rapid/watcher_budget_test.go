package rapid

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoWithDirs makes a git repo with n subdirectories of filesPer files each.
func repoWithDirs(t *testing.T, n, filesPer int) string {
	t.Helper()
	root := t.TempDir()
	initGitRepo(t, root)
	for i := 0; i < n; i++ {
		dir := filepath.Join(root, fmt.Sprintf("d%d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < filesPer; j++ {
			writeTestFile(t, dir, fmt.Sprintf("f%d.txt", j), "x\n")
		}
	}
	return root
}

func watchedUnder(w *Watcher, root string) []string {
	var out []string
	for _, p := range w.watcher.WatchList() {
		if p == root || strings.HasPrefix(p, root+"/") {
			out = append(out, p)
		}
	}
	return out
}

// kqueue backs every watched directory and entry with an fd. The watcher must
// stop before it fills the process's fd table, or every git subprocess after
// that fails with EMFILE and repo tracking collapses.
func TestWatcher_StopsAtFDBudget(t *testing.T) {
	root := repoWithDirs(t, 4, 10) // root costs 1+5 entries, each dN costs 11

	w, err := NewWatcher(nil, NewStateTable(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.fdBudget = 30

	w.addRecursive(root)

	if w.fdUsed > w.fdBudget {
		t.Errorf("fdUsed = %d, exceeds budget %d", w.fdUsed, w.fdBudget)
	}
	got := watchedUnder(w, root)
	if len(got) == 0 || len(got) >= 5 {
		t.Errorf("watched %d dirs, want some but not all 5: %v", len(got), got)
	}

	// Removing the repo hands the fds back.
	w.removeRecursive(root)
	if w.fdUsed != 0 {
		t.Errorf("fdUsed after remove = %d, want 0", w.fdUsed)
	}
	if left := watchedUnder(w, root); len(left) != 0 {
		t.Errorf("still watching %v after remove", left)
	}
}

func TestWatcher_NoBudgetWatchesEverything(t *testing.T) {
	root := repoWithDirs(t, 4, 2)

	w, err := NewWatcher(nil, NewStateTable(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.fdBudget = 0

	w.addRecursive(root)

	if got := watchedUnder(w, root); len(got) != 5 {
		t.Errorf("watched %d dirs, want 5: %v", len(got), got)
	}
}

// Claude Code worktrees under .claude/ are full copies of the repo that are
// never indexed; watching them only spends fds.
func TestWatcher_SkipsClaudeDir(t *testing.T) {
	root := repoWithDirs(t, 1, 1)
	wt := filepath.Join(root, ".claude", "worktrees", "feature", "src")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, wt, "main.go", "package main\n")

	w, err := NewWatcher(nil, NewStateTable(), DefaultConfig().WatchSkipDirs)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	w.addRecursive(root)

	for _, p := range watchedUnder(w, root) {
		if strings.Contains(p, "/.claude") {
			t.Errorf("watching %s, want .claude skipped", p)
		}
	}
	if got := watchedUnder(w, root); len(got) != 2 {
		t.Errorf("watched %d dirs, want root and d0: %v", len(got), got)
	}
}

func TestWatchFDBudget_LeavesHeadroom(t *testing.T) {
	b := watchFDBudget()
	if b < 0 {
		t.Errorf("budget = %d, want >= 0", b)
	}
}
