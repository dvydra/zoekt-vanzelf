package rapid

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"
)

// ReindexManager handles running zoekt-git-index for repos that need reindexing.
type ReindexManager struct {
	config Config
	state  *StateTable
	proxy  *SearchProxy

	sem  chan struct{} // concurrency limiter
	mu   sync.Mutex
	busy map[string]bool // paths currently being reindexed
	wg   sync.WaitGroup  // tracks in-flight reindex jobs
}

func NewReindexManager(cfg Config, state *StateTable, proxy *SearchProxy) *ReindexManager {
	return &ReindexManager{
		config: cfg,
		state:  state,
		proxy:  proxy,
		sem:    make(chan struct{}, cfg.MaxConcurrentReindex),
		busy:   make(map[string]bool),
	}
}

// TriggerReindex queues a reindex for the given repo. Non-blocking.
// Returns immediately if the repo is already being reindexed.
func (rm *ReindexManager) TriggerReindex(repoPath string) {
	rm.mu.Lock()
	if rm.busy[repoPath] {
		rm.mu.Unlock()
		return
	}
	rm.busy[repoPath] = true
	rm.mu.Unlock()

	rm.wg.Add(1)
	go rm.reindex(repoPath)
}

// Wait blocks until all in-flight reindex jobs complete.
func (rm *ReindexManager) Wait() {
	rm.wg.Wait()
}

// IsBusy returns true if the repo is currently being reindexed.
func (rm *ReindexManager) IsBusy(repoPath string) bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return rm.busy[repoPath]
}

func (rm *ReindexManager) reindex(repoPath string) {
	defer rm.wg.Done()
	defer func() {
		rm.mu.Lock()
		delete(rm.busy, repoPath)
		rm.mu.Unlock()
	}()

	// Acquire semaphore slot.
	rm.sem <- struct{}{}
	defer func() { <-rm.sem }()

	// Mark repo as indexing.
	rm.state.SetStatus(repoPath, RepoIndexing)

	log.Printf("[%s] reindexing...", repoPath)
	start := time.Now()

	err := runZoektGitIndex(repoPath, rm.config.DataDir)
	if err != nil {
		log.Printf("[%s] reindex failed: %v", repoPath, err)
		rm.state.SetStatus(repoPath, RepoError)
		return
	}

	elapsed := time.Since(start)
	log.Printf("[%s] reindexed in %s", repoPath, elapsed.Round(time.Millisecond))

	// Update indexed SHA and recompute the delta against the new HEAD, both
	// from a single git invocation.
	bh, dirty, err := GetRepoState(repoPath)
	if err == nil {
		rm.state.SetIndexed(repoPath, bh.SHA)
		if len(dirty) > 0 {
			rm.state.SetDelta(repoPath, BuildDeltaIndex(repoPath, dirty))
		} else {
			rm.state.SetDelta(repoPath, nil)
		}
	}
	rm.state.SetStatus(repoPath, RepoIdle)

	// Refresh the proxy's repo name map since zoekt may have new shard names.
	rm.proxy.RefreshRepoMap()
}

// ReindexAll triggers a reindex for every managed repo. Used for hourly full reindex.
func (rm *ReindexManager) ReindexAll() {
	paths := rm.state.Paths()
	log.Printf("full reindex: %d repos", len(paths))
	for _, path := range paths {
		rm.TriggerReindex(path)
	}
}

func runZoektGitIndex(repoPath, dataDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "zoekt-git-index",
		"-branches", "HEAD",
		"-index", dataDir,
		repoPath,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(output))
	}
	return nil
}
