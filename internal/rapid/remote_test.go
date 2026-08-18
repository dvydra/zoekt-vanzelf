package rapid

import (
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
)

func TestParseRemoteDefault_SymrefWins(t *testing.T) {
	// refs/remotes/origin/HEAD shortens to "origin" and carries a symref
	// target; origin/main is also listed with the same object.
	out := []byte("origin\tdeadbeef\torigin/main\norigin/main\tdeadbeef\t\n")

	rr := parseRemoteDefault(out)
	if rr.Branch != "origin/main" {
		t.Errorf("branch = %q, want origin/main", rr.Branch)
	}
	if rr.SHA != "deadbeef" {
		t.Errorf("sha = %q, want deadbeef", rr.SHA)
	}
}

func TestParseRemoteDefault_SymrefToMaster(t *testing.T) {
	// A repo whose default is master, plus a stray origin/main branch that
	// must not win over the symref.
	out := []byte("origin\taaa111\torigin/master\norigin/main\tbbb222\t\norigin/master\taaa111\t\n")

	rr := parseRemoteDefault(out)
	if rr.Branch != "origin/master" || rr.SHA != "aaa111" {
		t.Errorf("got %+v, want origin/master@aaa111", rr)
	}
}

func TestParseRemoteDefault_NoSymrefFallsBackToMain(t *testing.T) {
	out := []byte("origin/main\tcafe01\t\norigin/master\told999\t\n")

	rr := parseRemoteDefault(out)
	if rr.Branch != "origin/main" || rr.SHA != "cafe01" {
		t.Errorf("got %+v, want origin/main@cafe01", rr)
	}
}

func TestParseRemoteDefault_MasterOnly(t *testing.T) {
	out := []byte("origin/master\tfeed01\t\n")

	rr := parseRemoteDefault(out)
	if rr.Branch != "origin/master" || rr.SHA != "feed01" {
		t.Errorf("got %+v, want origin/master@feed01", rr)
	}
}

func TestParseRemoteDefault_StaleSymrefFallsThrough(t *testing.T) {
	// origin/HEAD pointing at a deleted branch prints an empty objectname.
	out := []byte("origin\t\torigin/trunk\norigin/main\tabc123\t\n")

	rr := parseRemoteDefault(out)
	if rr.Branch != "origin/main" || rr.SHA != "abc123" {
		t.Errorf("got %+v, want origin/main@abc123", rr)
	}
}

func TestParseRemoteDefault_Empty(t *testing.T) {
	if rr := parseRemoteDefault(nil); rr.Branch != "" || rr.SHA != "" {
		t.Errorf("got %+v, want zero RemoteRef", rr)
	}
}

func TestGetRemoteDefault_RealRepo(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	writeTestFile(t, dir, "a.txt", "hello\n")
	gitAdd(t, dir, "a.txt")
	gitCommit(t, dir, "init")

	// Simulate a clone's remote-tracking refs without a network remote.
	run(t, dir, "git", "update-ref", "refs/remotes/origin/main", "HEAD")
	run(t, dir, "git", "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	rr, err := GetRemoteDefault(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Branch != "origin/main" {
		t.Errorf("branch = %q, want origin/main", rr.Branch)
	}
	if len(rr.SHA) != 40 {
		t.Errorf("sha = %q, want a full 40-char sha", rr.SHA)
	}
}

func TestGetRemoteDefault_NoOrigin(t *testing.T) {
	// A local-only repo is not an error; it just has nothing to index.
	dir := t.TempDir()
	initGitRepo(t, dir)
	writeTestFile(t, dir, "a.txt", "hello\n")
	gitAdd(t, dir, "a.txt")
	gitCommit(t, dir, "init")

	rr, err := GetRemoteDefault(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rr.Branch != "" {
		t.Errorf("branch = %q, want empty", rr.Branch)
	}
}

func TestBranchesFor_IncludesRemoteDefault(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	writeTestFile(t, dir, "a.txt", "hello\n")
	gitAdd(t, dir, "a.txt")
	gitCommit(t, dir, "init")
	run(t, dir, "git", "update-ref", "refs/remotes/origin/main", "HEAD")

	cfg := DefaultConfig()
	cfg.IndexRemoteDefault = true
	rm := NewReindexManager(cfg, NewStateTable(), nil)

	got := rm.branchesFor(dir)
	want := []string{"HEAD", "origin/main"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("branchesFor = %v, want %v", got, want)
	}
}

func TestBranchesFor_HeadOnlyWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	writeTestFile(t, dir, "a.txt", "hello\n")
	gitAdd(t, dir, "a.txt")
	gitCommit(t, dir, "init")
	run(t, dir, "git", "update-ref", "refs/remotes/origin/main", "HEAD")

	cfg := DefaultConfig()
	cfg.IndexRemoteDefault = false
	rm := NewReindexManager(cfg, NewStateTable(), nil)

	got := rm.branchesFor(dir)
	if len(got) != 1 || got[0] != "HEAD" {
		t.Errorf("branchesFor = %v, want [HEAD]", got)
	}
}

func TestBranchesFor_HeadOnlyWithoutOrigin(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	writeTestFile(t, dir, "a.txt", "hello\n")
	gitAdd(t, dir, "a.txt")
	gitCommit(t, dir, "init")

	rm := NewReindexManager(DefaultConfig(), NewStateTable(), nil)

	got := rm.branchesFor(dir)
	if len(got) != 1 || got[0] != "HEAD" {
		t.Errorf("branchesFor = %v, want [HEAD]", got)
	}
}

func TestIndexedBranchSHA(t *testing.T) {
	repoPath := t.TempDir()
	zoektURL := startMockZoektWithBranches(t, "example.com/x/y", repoPath, map[string]string{
		"HEAD":        "head111",
		"origin/main": "main222",
	})

	proxy := NewSearchProxy(zoektURL, NewStateTable())
	proxy.RefreshRepoMap()

	abs, _ := filepath.Abs(repoPath)
	if got := proxy.IndexedBranchSHA(abs, "origin/main"); got != "main222" {
		t.Errorf("origin/main = %q, want main222", got)
	}
	if got := proxy.IndexedBranchSHA(abs, "HEAD"); got != "head111" {
		t.Errorf("HEAD = %q, want head111", got)
	}
	if got := proxy.IndexedSHA(abs); got != "head111" {
		t.Errorf("IndexedSHA = %q, want head111", got)
	}
	if got := proxy.IndexedBranchSHA(abs, "origin/nope"); got != "" {
		t.Errorf("missing branch = %q, want empty", got)
	}
}

// remoteRefNeedsReindex: the shard is behind the fetched ref.
func TestRemoteRefNeedsReindex_ShardBehind(t *testing.T) {
	dir, sha := repoWithRemoteMain(t)

	// Shard holds an older origin/main than the working repo's ref.
	zoektURL := startMockZoektWithBranches(t, "repo", dir, map[string]string{
		"HEAD":        sha,
		"origin/main": "0000000000000000000000000000000000000000",
	})
	p := pollerForRemoteTest(t, dir, zoektURL)

	rr, indexed, need := p.remoteRefNeedsReindex(dir)
	if !need {
		t.Fatalf("want reindex, got none (ref %+v, indexed %q)", rr, indexed)
	}
	if rr.Branch != "origin/main" || rr.SHA != sha {
		t.Errorf("ref = %+v, want origin/main@%s", rr, sha)
	}
	if indexed == "" {
		t.Error("indexed sha should be the stale value, not empty")
	}
}

// The shard already matches the fetched ref — nothing to do.
func TestRemoteRefNeedsReindex_ShardCurrent(t *testing.T) {
	dir, sha := repoWithRemoteMain(t)

	zoektURL := startMockZoektWithBranches(t, "repo", dir, map[string]string{
		"HEAD":        sha,
		"origin/main": sha,
	})
	p := pollerForRemoteTest(t, dir, zoektURL)

	if _, _, need := p.remoteRefNeedsReindex(dir); need {
		t.Error("shard is current, want no reindex")
	}
}

// A shard built before remote-branch indexing has no origin/main at all; that
// must read as "needs reindex" so existing installs pick the branch up once.
func TestRemoteRefNeedsReindex_BranchMissingFromShard(t *testing.T) {
	dir, sha := repoWithRemoteMain(t)

	zoektURL := startMockZoektWithBranches(t, "repo", dir, map[string]string{
		"HEAD": sha,
	})
	p := pollerForRemoteTest(t, dir, zoektURL)

	_, indexed, need := p.remoteRefNeedsReindex(dir)
	if !need {
		t.Error("branch missing from shard, want reindex")
	}
	if indexed != "" {
		t.Errorf("indexed = %q, want empty for a branch the shard lacks", indexed)
	}
}

// A repo with no origin never triggers a reindex, however stale the shard.
func TestRemoteRefNeedsReindex_NoOrigin(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	writeTestFile(t, dir, "a.txt", "hello\n")
	gitAdd(t, dir, "a.txt")
	gitCommit(t, dir, "init")

	zoektURL := startMockZoektWithBranches(t, "repo", dir, map[string]string{"HEAD": "whatever"})
	p := pollerForRemoteTest(t, dir, zoektURL)

	if _, _, need := p.remoteRefNeedsReindex(dir); need {
		t.Error("local-only repo, want no reindex")
	}
}

// A repo whose last index failed is left to the hourly full reindex.
func TestRemoteRefNeedsReindex_SkipsErroredRepo(t *testing.T) {
	dir, sha := repoWithRemoteMain(t)

	zoektURL := startMockZoektWithBranches(t, "repo", dir, map[string]string{"HEAD": sha})
	p := pollerForRemoteTest(t, dir, zoektURL)
	p.state.SetStatus(dir, RepoError)

	if _, _, need := p.remoteRefNeedsReindex(dir); need {
		t.Error("errored repo, want no reindex until the hourly sweep")
	}
}

// --- Helpers ---

// repoWithRemoteMain builds a repo with an origin/main remote-tracking ref and
// returns its path and that ref's SHA.
func repoWithRemoteMain(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	initGitRepo(t, dir)
	writeTestFile(t, dir, "a.txt", "hello\n")
	gitAdd(t, dir, "a.txt")
	gitCommit(t, dir, "init")
	run(t, dir, "git", "update-ref", "refs/remotes/origin/main", "HEAD")

	rr, err := GetRemoteDefault(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, rr.SHA
}

// pollerForRemoteTest wires a poller with a real proxy pointed at a mock zoekt
// and a reindex manager that is never actually triggered by these tests.
func pollerForRemoteTest(t *testing.T, repoPath, zoektURL string) *Poller {
	t.Helper()
	cfg := DefaultConfig()
	state := NewStateTable()
	proxy := NewSearchProxy(zoektURL, state)
	proxy.RefreshRepoMap()

	bh, dirty, err := GetRepoState(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	state.Update(repoPath, bh, dirty)

	p := NewPoller(cfg, state)
	p.Proxy = proxy
	p.Reindex = NewReindexManager(cfg, state, proxy)
	return p
}

// startMockZoektWithBranches serves an /api/list response describing one repo
// with the given branch → indexed SHA map.
func startMockZoektWithBranches(t *testing.T, repoName, repoPath string, branches map[string]string) string {
	t.Helper()

	body := ""
	for name, version := range branches {
		if body != "" {
			body += ","
		}
		body += fmt.Sprintf(`{"Name":%q,"Version":%q}`, name, version)
	}
	payload := fmt.Sprintf(`{"List":{"Repos":[{"Repository":{"Name":%q,"Source":%q,"Branches":[%s]}}]}}`,
		repoName, repoPath, body)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, payload)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)
	t.Cleanup(func() { srv.Close() })
	return fmt.Sprintf("http://localhost:%d", listener.Addr().(*net.TCPAddr).Port)
}
