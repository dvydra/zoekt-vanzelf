package rapid

import "testing"

func TestGetIgnoredDirs(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)

	// Ignore a build dir, a live-data dir, and a loose file.
	writeTestFile(t, repo, ".gitignore", "node_modules/\ndata/\nsecret.txt\n")
	// Tracked source — must NOT be pruned.
	writeTestFile(t, repo, "src/main.go", "package main\n")
	// Ignored, high-churn dirs (need content so git sees them as untracked).
	writeTestFile(t, repo, "node_modules/pkg/index.js", "//\n")
	writeTestFile(t, repo, "data/mysql/ibdata1", "binary\n")
	// Ignored loose file — a file, not a directory, so it must not be pruned.
	writeTestFile(t, repo, "secret.txt", "shh\n")

	gitAdd(t, repo, ".") // respects .gitignore: only .gitignore + src/main.go land
	gitCommit(t, repo, "initial")

	ignored, err := GetIgnoredDirs(repo)
	if err != nil {
		t.Fatal(err)
	}

	if !ignored["node_modules"] {
		t.Errorf("expected node_modules ignored; got %v", ignored)
	}
	if !ignored["data"] {
		t.Errorf("expected data ignored; got %v", ignored)
	}
	if ignored["src"] {
		t.Error("tracked dir src must not be reported as ignored")
	}
	if ignored["secret.txt"] {
		t.Error("ignored file secret.txt must not appear as an ignored dir")
	}
}
