package rapid

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E_DeltaSearchLifecycle tests the full flow:
//   - Start zoekt-vanzelf with a test repo
//   - Edit a file → search finds the edit via delta
//   - Add a new file → searchable via delta
//   - Delete a file → no longer searchable (tombstone)
//   - Verify zoekt results for dirty paths are suppressed
func TestE2E_DeltaSearchLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	// Create a real git repo with initial content.
	root := t.TempDir()
	repoDir := filepath.Join(root, "test-repo")
	initGitRepo(t, repoDir)

	writeTestFile(t, repoDir, "hello.go", `package main

func hello() {
	println("original_content_e2e_marker")
}
`)
	writeTestFile(t, repoDir, "goodbye.go", `package main

func goodbye() {
	println("goodbye_e2e_marker")
}
`)
	gitAdd(t, repoDir, ".")
	gitCommit(t, repoDir, "initial commit")

	// Start a mock zoekt server that returns empty results.
	// This isolates the test from needing real zoekt.
	mockZoektURL := startMockZoekt(t)

	// Pick a free port for zoekt-vanzelf.
	rapidPort := freePort(t)

	// Configure and start zoekt-vanzelf.
	cfg := DefaultConfig()
	cfg.Roots = []string{root}
	cfg.ScanDepth = 3
	cfg.ProxyPort = rapidPort
	cfg.ZoektURL = mockZoektURL
	cfg.RepoPollInterval = 200 * time.Millisecond // fast polling for tests
	cfg.DiscoveryInterval = 60 * time.Second
	cfg.ReindexInterval = 60 * time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	state := NewStateTable()
	proxy := NewSearchProxy(cfg.ZoektURL, state)
	reindexMgr := NewReindexManager(cfg, state, proxy)
	poller := NewPoller(cfg, state)
	poller.Reindex = nil // don't trigger real zoekt-git-index in tests
	poller.Proxy = proxy
	scheduler := NewScheduler(cfg, reindexMgr)
	srv := NewServer(proxy, state, reindexMgr, poller, scheduler, cfg.ProxyHost, cfg.ProxyPort, cfg.ZoektURL)

	proxy.RefreshRepoMap()
	go poller.Run(ctx)
	go func() { srv.ListenAndServe() }()

	// Wait for server to be ready.
	rapidURL := fmt.Sprintf("http://localhost:%d", rapidPort)
	waitForServer(t, rapidURL, 5*time.Second)

	// Wait for initial poll to discover repo and build delta.
	waitForCondition(t, 5*time.Second, func() bool {
		return state.Get(repoDir) != nil
	}, "repo should be discovered")

	// --- Test 1: Search finds committed content via delta ---
	// The committed files won't be in delta (they're clean), and mock zoekt returns nothing.
	// So initially we should get no results for committed-only content.
	t.Run("clean_files_not_in_delta", func(t *testing.T) {
		matches := search(t, rapidURL, "original_content_e2e_marker")
		if len(matches) != 0 {
			t.Errorf("expected 0 results for clean file (mock zoekt returns empty), got %d", len(matches))
		}
	})

	// --- Test 2: Edit a file → delta picks it up ---
	t.Run("edit_file_appears_in_delta", func(t *testing.T) {
		writeTestFile(t, repoDir, "hello.go", `package main

func hello() {
	println("modified_content_e2e_unique_xyzzy")
}
`)
		waitForSearchResult(t, rapidURL, "modified_content_e2e_unique_xyzzy", 5*time.Second)
	})

	// --- Test 3: Old content no longer found after edit ---
	t.Run("old_content_suppressed_after_edit", func(t *testing.T) {
		waitForCondition(t, 5*time.Second, func() bool {
			matches := search(t, rapidURL, "original_content_e2e_marker")
			return len(matches) == 0
		}, "old content should be suppressed")
	})

	// --- Test 4: Add a new untracked file → searchable ---
	t.Run("new_untracked_file_searchable", func(t *testing.T) {
		writeTestFile(t, repoDir, "brand_new.go", `package main

func brandNew() {
	println("brand_new_e2e_unique_plugh")
}
`)
		waitForSearchResult(t, rapidURL, "brand_new_e2e_unique_plugh", 5*time.Second)
	})

	// --- Test 5: Delete a file → tombstoned, content no longer found ---
	t.Run("deleted_file_tombstoned", func(t *testing.T) {
		os.Remove(filepath.Join(repoDir, "goodbye.go"))
		waitForCondition(t, 5*time.Second, func() bool {
			matches := search(t, rapidURL, "goodbye_e2e_marker")
			return len(matches) == 0
		}, "deleted file content should not be found")
	})

	// --- Test 6: Status API returns repo info ---
	t.Run("status_api", func(t *testing.T) {
		resp, err := http.Get(rapidURL + "/api/status")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var status StatusResponse
		json.NewDecoder(resp.Body).Decode(&status)

		if status.RepoCount != 1 {
			t.Errorf("expected 1 repo, got %d", status.RepoCount)
		}

		repoStatus, ok := status.Repos[repoDir]
		if !ok {
			t.Fatal("test repo not in status")
		}
		if repoStatus.Branch != "main" {
			t.Errorf("expected branch main, got %s", repoStatus.Branch)
		}
		if repoStatus.DirtyFiles < 2 {
			t.Errorf("expected at least 2 dirty files, got %d", repoStatus.DirtyFiles)
		}
	})

	// --- Test 7: Regex search works ---
	t.Run("regex_search", func(t *testing.T) {
		matches := search(t, rapidURL, "brand_new.*plugh")
		if len(matches) == 0 {
			t.Error("regex search should find matches")
		}
	})

	// --- Test 8: Multiple matches in same file ---
	t.Run("multiple_matches_same_file", func(t *testing.T) {
		writeTestFile(t, repoDir, "multi.go", `package main

// e2e_multi_marker line one
// e2e_multi_marker line two
// e2e_multi_marker line three
`)
		waitForCondition(t, 5*time.Second, func() bool {
			matches := search(t, rapidURL, "e2e_multi_marker")
			return len(matches) >= 3
		}, "should find 3 matches in multi.go")
	})
}

// TestE2E_LiveProxy tests against the actual running zoekt-vanzelf instance.
// Only runs if zoekt-vanzelf is running on port 6071.
func TestE2E_LiveProxy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live E2E test in short mode")
	}

	rapidURL := "http://localhost:6071"
	resp, err := http.Get(rapidURL + "/api/status")
	if err != nil {
		t.Skip("zoekt-vanzelf not running on port 6071, skipping live test")
	}
	resp.Body.Close()

	// Create a unique marker in this repo's working tree.
	marker := fmt.Sprintf("live_e2e_test_marker_%d", time.Now().UnixNano())
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	markerFile := filepath.Join(wd, "e2e_scratch_test.tmp")

	content := fmt.Sprintf("// %s\n", marker)
	if err := os.WriteFile(markerFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(markerFile)

	// Wait for the marker to appear in search results.
	t.Run("live_edit_searchable", func(t *testing.T) {
		waitForSearchResult(t, rapidURL, marker, 10*time.Second)
	})

	// Remove the file and verify it disappears.
	t.Run("live_delete_disappears", func(t *testing.T) {
		os.Remove(markerFile)
		waitForCondition(t, 10*time.Second, func() bool {
			matches := search(t, rapidURL, marker)
			return len(matches) == 0
		}, "marker should disappear after file removal")
	})
}

// TestE2E_ProxyMerge_EditFile proves the full proxy merge flow:
//   - Mock zoekt returns stale results for the OLD file content ("foo\nfoo")
//   - The working tree has the NEW content ("bar\nfoo")
//   - Searching "foo": zoekt's line 1 hit is suppressed, delta returns only line 2
//   - Searching "bar": zoekt has nothing, delta returns line 1
func TestE2E_ProxyMerge_EditFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	// Create a git repo with initial content "foo\nfoo".
	root := t.TempDir()
	repoDir := filepath.Join(root, "test-repo")
	initGitRepo(t, repoDir)
	writeTestFile(t, repoDir, "test.txt", "foo\nfoo\n")
	gitAdd(t, repoDir, ".")
	gitCommit(t, repoDir, "initial")

	// Now edit the file in the working tree: "bar\nfoo".
	writeTestFile(t, repoDir, "test.txt", "bar\nfoo\n")

	// Start a mock zoekt that returns what zoekt WOULD return for the old content:
	// two matches for "foo" in test.txt (line 1 and line 2).
	mockURL := startMockZoektWithStaleResults(t, "test-repo", repoDir)

	rapidPort := freePort(t)

	cfg := DefaultConfig()
	cfg.Roots = []string{root}
	cfg.ScanDepth = 3
	cfg.ProxyPort = rapidPort
	cfg.ZoektURL = mockURL
	cfg.RepoPollInterval = 200 * time.Millisecond
	cfg.DiscoveryInterval = 60 * time.Second
	cfg.ReindexInterval = 60 * time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	state := NewStateTable()
	proxy := NewSearchProxy(cfg.ZoektURL, state)
	reindexMgr := NewReindexManager(cfg, state, proxy)
	poller := NewPoller(cfg, state)
	poller.Reindex = nil
	poller.Proxy = proxy
	scheduler := NewScheduler(cfg, reindexMgr)
	srv := NewServer(proxy, state, reindexMgr, poller, scheduler, cfg.ProxyHost, cfg.ProxyPort, cfg.ZoektURL)

	proxy.RefreshRepoMap()
	go poller.Run(ctx)
	go func() { srv.ListenAndServe() }()

	rapidURL := fmt.Sprintf("http://localhost:%d", rapidPort)
	waitForServer(t, rapidURL, 5*time.Second)

	// Wait for delta to be built.
	waitForCondition(t, 5*time.Second, func() bool {
		s := state.Get(repoDir)
		return s != nil && s.DeltaIndex != nil && len(s.DeltaIndex.Files) > 0
	}, "delta index should be built for edited file")

	t.Run("search_foo_returns_only_line_2_from_delta", func(t *testing.T) {
		// Mock zoekt returns 2 hits for "foo" (lines 1 and 2, from old content).
		// But test.txt is dirty, so ALL zoekt results for it are suppressed.
		// Delta has "bar\nfoo\n", so only line 2 matches "foo".
		// clean.txt also passes through from zoekt (1 match).
		// So total = 1 (delta, line 2 of test.txt) + 1 (zoekt, clean.txt) = 2 matches.
		waitForCondition(t, 5*time.Second, func() bool {
			matches := search(t, rapidURL, "foo")
			var testTxtMatches []searchMatch
			for _, m := range matches {
				if m.File == "test.txt" {
					testTxtMatches = append(testTxtMatches, m)
				}
			}
			if len(testTxtMatches) != 1 {
				t.Logf("DEBUG: test.txt matches=%d total=%d: %+v", len(testTxtMatches), len(matches), matches)
				return false
			}
			return testTxtMatches[0].Line == 2 && testTxtMatches[0].Content == "foo"
		}, "should find exactly 1 'foo' match in test.txt on line 2 (delta), not 2 (stale zoekt)")
	})

	t.Run("search_bar_returns_line_1_from_delta", func(t *testing.T) {
		// Mock zoekt returns nothing for "bar" (old content didn't have it).
		// Delta has "bar" on line 1.
		matches := search(t, rapidURL, "bar")
		if len(matches) != 1 {
			t.Fatalf("expected 1 match for 'bar', got %d: %+v", len(matches), matches)
		}
		if matches[0].Line != 1 {
			t.Errorf("expected match on line 1, got line %d", matches[0].Line)
		}
		if matches[0].Content != "bar" {
			t.Errorf("expected content 'bar', got %q", matches[0].Content)
		}
	})

	t.Run("clean_file_still_returned_from_zoekt", func(t *testing.T) {
		// Mock zoekt also returns a match for "foo" in clean.txt (not dirty).
		// This should pass through unfiltered.
		matches := search(t, rapidURL, "foo")
		hasClean := false
		for _, m := range matches {
			if m.File == "clean.txt" {
				hasClean = true
			}
		}
		if !hasClean {
			t.Error("expected zoekt result for clean.txt to pass through")
		}
	})
}

// startMockZoektWithStaleResults returns a mock zoekt that:
//   - /api/list: reports the repo with name=repoName, source=repoPath
//   - /api/search for "foo": returns 2 matches in test.txt (lines 1 and 2)
//     PLUS 1 match in clean.txt (not dirty, should pass through)
//   - /api/search for anything else: returns empty
func startMockZoektWithStaleResults(t *testing.T, repoName, repoPath string) string {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"List":{"Repos":[{"Repository":{"Name":%q,"Source":%q,"Branches":[{"Name":"HEAD","Version":"abc123"}]}}]}}`,
			repoName, repoPath)
	})

	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Q string `json:"q"` }
		json.NewDecoder(r.Body).Decode(&req)

		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(req.Q, "foo") {
			// Return stale results: "foo" on lines 1 and 2 of test.txt (old content)
			// Plus a result in clean.txt that should not be suppressed.
			line1 := base64.StdEncoding.EncodeToString([]byte("foo\n"))
			cleanLine := base64.StdEncoding.EncodeToString([]byte("foo in clean file\n"))
			fmt.Fprintf(w, `{"Result":{"Files":[
				{"Repository":%q,"FileName":"test.txt","LineMatches":[
					{"Line":%q,"LineNumber":1,"LineFragments":[{"LineOffset":0,"MatchLength":3}]},
					{"Line":%q,"LineNumber":2,"LineFragments":[{"LineOffset":0,"MatchLength":3}]}
				]},
				{"Repository":%q,"FileName":"clean.txt","LineMatches":[
					{"Line":%q,"LineNumber":1,"LineFragments":[{"LineOffset":0,"MatchLength":3}]}
				]}
			],"FileCount":100,"MatchCount":3}}`,
				repoName, line1, line1,
				repoName, cleanLine)
		} else {
			fmt.Fprint(w, `{"Result":{"Files":null,"FileCount":100,"MatchCount":0}}`)
		}
	})

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)
	t.Cleanup(func() { srv.Close() })
	return fmt.Sprintf("http://localhost:%d", listener.Addr().(*net.TCPAddr).Port)
}

// --- Helpers ---

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0755)
	run(t, dir, "git", "init", "-b", "main")
	run(t, dir, "git", "config", "user.email", "test@test.com")
	run(t, dir, "git", "config", "user.name", "Test")
}

func gitAdd(t *testing.T, dir, path string) {
	t.Helper()
	run(t, dir, "git", "add", path)
}

func gitCommit(t *testing.T, dir, msg string) {
	t.Helper()
	run(t, dir, "git", "commit", "-m", msg)
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("command %s %v failed: %v", name, args, err)
	}
}

func writeTestFile(t *testing.T, repoDir, name, content string) {
	t.Helper()
	path := filepath.Join(repoDir, name)
	os.MkdirAll(filepath.Dir(path), 0755)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

type searchMatch struct {
	Repo     string
	File     string
	Line     int
	Content  string
}

func search(t *testing.T, baseURL, query string) []searchMatch {
	t.Helper()
	body := fmt.Sprintf(`{"q":"%s"}`, query)
	resp, err := http.Post(baseURL+"/api/search", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("search request failed: %v", err)
	}
	defer resp.Body.Close()

	var raw map[string]any
	json.NewDecoder(resp.Body).Decode(&raw)

	var matches []searchMatch
	resultMap, _ := raw["Result"].(map[string]any)
	if resultMap == nil {
		return matches
	}
	files, _ := resultMap["Files"].([]any)
	for _, fileRaw := range files {
		f, ok := fileRaw.(map[string]any)
		if !ok {
			continue
		}
		repo, _ := f["Repository"].(string)
		file, _ := f["FileName"].(string)
		lineMatches, _ := f["LineMatches"].([]any)
		for _, lmRaw := range lineMatches {
			lm, ok := lmRaw.(map[string]any)
			if !ok {
				continue
			}
			lineB64, _ := lm["Line"].(string)
			lineNum, _ := lm["LineNumber"].(float64)
			decoded, _ := base64.StdEncoding.DecodeString(lineB64)
			matches = append(matches, searchMatch{
				Repo:    repo,
				File:    file,
				Line:    int(lineNum),
				Content: strings.TrimSpace(string(decoded)),
			})
		}
	}
	return matches
}

func waitForSearchResult(t *testing.T, baseURL, query string, timeout time.Duration) {
	t.Helper()
	waitForCondition(t, timeout, func() bool {
		matches := search(t, baseURL, query)
		return len(matches) > 0
	}, fmt.Sprintf("search for %q should return results", query))
}

func waitForCondition(t *testing.T, timeout time.Duration, check func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", msg)
}

func waitForServer(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url + "/api/status")
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("server did not start in time")
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// startMockZoekt starts a minimal mock zoekt-webserver that returns empty results.
// Returns the base URL (e.g. "http://localhost:12345").
func startMockZoekt(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Result":{"Files":null,"FileCount":0,"MatchCount":0}}`)
	})

	mux.HandleFunc("/api/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"List":{"Repos":[]}}`)
	})

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}

	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)
	t.Cleanup(func() { srv.Close() })

	return fmt.Sprintf("http://localhost:%d", listener.Addr().(*net.TCPAddr).Port)
}
