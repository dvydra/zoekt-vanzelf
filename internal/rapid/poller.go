package rapid

import (
	"context"
	"log"
	"sync"
	"time"
)

// Poller periodically polls all managed repos for state changes.
type Poller struct {
	config  Config
	state   *StateTable
	Reindex *ReindexManager // nil if reindex not wired up (e.g. poll-only mode)
	Proxy   *SearchProxy    // nil if proxy not wired up (e.g. poll-only mode)
	Watcher *Watcher        // nil if fsnotify not available

	// Per-repo locks to prevent concurrent pollRepo calls from the
	// poll ticker and the fsnotify watcher.
	repoMu   sync.Mutex
	repoLocks map[string]*sync.Mutex
}

func NewPoller(cfg Config, state *StateTable) *Poller {
	return &Poller{config: cfg, state: state, repoLocks: make(map[string]*sync.Mutex)}
}

// repoLock returns a per-repo mutex, creating one if needed.
func (p *Poller) repoLock(path string) *sync.Mutex {
	p.repoMu.Lock()
	defer p.repoMu.Unlock()
	mu, ok := p.repoLocks[path]
	if !ok {
		mu = &sync.Mutex{}
		p.repoLocks[path] = mu
	}
	return mu
}

// Run starts the polling loop. It blocks until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	// Initial discovery + poll.
	p.discoverAndPoll()

	pollTicker := time.NewTicker(p.config.RepoPollInterval)
	defer pollTicker.Stop()

	discoveryTicker := time.NewTicker(p.config.DiscoveryInterval)
	defer discoveryTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-pollTicker.C:
			p.pollAll()
		case <-discoveryTicker.C:
			p.discoverAndPoll()
		}
	}
}

func (p *Poller) discoverAndPoll() {
	repos, err := DiscoverRepos(p.config.Roots, p.config.ScanDepth, p.config.ExcludePatterns)
	if err != nil {
		log.Printf("discovery error: %v", err)
		return
	}

	// Remove repos that no longer exist.
	tracked := p.state.Paths()
	repoSet := make(map[string]bool, len(repos))
	for _, r := range repos {
		repoSet[r] = true
	}
	for _, path := range tracked {
		if !repoSet[path] {
			log.Printf("[%s] removed (no longer exists)", path)
			p.state.Remove(path)
		}
	}

	// Poll all discovered repos.
	for _, path := range repos {
		p.pollRepo(path)
	}

	// Sync fsnotify watches.
	if p.Watcher != nil {
		p.Watcher.Sync()
	}
}

func (p *Poller) pollAll() {
	for _, path := range p.state.Paths() {
		p.pollRepo(path)
	}
}

func (p *Poller) pollRepo(path string) {
	mu := p.repoLock(path)
	mu.Lock()
	defer mu.Unlock()

	bh, dirty, err := GetRepoState(path)
	if err != nil {
		log.Printf("[%s] git error: %v", path, err)
		return
	}

	change := p.state.Update(path, bh, dirty)

	needsReindex := false

	if change.BranchChanged && change.OldBranch == "" {
		// New repo — check if zoekt already has a valid shard.
		if p.Proxy != nil {
			indexedSHA := p.Proxy.IndexedSHA(path)
			if indexedSHA == bh.SHA {
				// Zoekt shard matches current HEAD — no reindex needed.
				p.state.SetIndexed(path, indexedSHA)
				p.state.SetStatus(path, RepoIdle)
				log.Printf("[%s] new repo on %s@%s (%d dirty) — shard current",
					path, change.NewBranch, shortSHA(change.NewSHA), change.DirtyCount)
			} else if indexedSHA != "" {
				// Shard exists but stale.
				log.Printf("[%s] new repo on %s@%s (%d dirty) — shard stale (indexed %s)",
					path, change.NewBranch, shortSHA(change.NewSHA), change.DirtyCount, shortSHA(indexedSHA))
				p.state.SetIndexed(path, indexedSHA)
				needsReindex = true
			} else {
				// No shard at all.
				log.Printf("[%s] new repo on %s@%s (%d dirty) — no shard",
					path, change.NewBranch, shortSHA(change.NewSHA), change.DirtyCount)
				needsReindex = true
			}
		} else {
			log.Printf("[%s] new repo on %s@%s (%d dirty)",
				path, change.NewBranch, shortSHA(change.NewSHA), change.DirtyCount)
		}
	} else if change.BranchChanged {
		log.Printf("[%s] branch changed: %s → %s",
			path, change.OldBranch, change.NewBranch)
		needsReindex = true
	} else if change.HeadChanged {
		log.Printf("[%s] HEAD changed: %s → %s",
			path, shortSHA(change.OldSHA), shortSHA(change.NewSHA))
		needsReindex = true
	} else if change.DirtyChanged {
		log.Printf("[%s] dirty files changed: %d files",
			path, change.DirtyCount)
	}

	if needsReindex && p.Reindex != nil {
		p.state.SetStatus(path, RepoStale)
		if p.config.ReindexGapMode == "blackout" {
			// Destroy delta immediately — searches will miss dirty files during reindex.
			p.state.SetDelta(path, nil)
		}
		// In "stale" mode (default), keep the old delta so searches still return
		// results during reindex. The delta may be slightly wrong (relative to old
		// HEAD), but that's better than a gap. Reindex completion rebuilds it.
		p.Reindex.TriggerReindex(path)
	} else {
		// Rebuild delta index if dirty files changed.
		if len(dirty) > 0 && change.DirtyChanged {
			delta := BuildDeltaIndex(path, dirty)
			p.state.SetDelta(path, delta)

			// Check delta threshold — trigger early reindex if too large.
			if p.Reindex != nil && p.deltaExceedsThreshold(len(dirty), delta) {
				log.Printf("[%s] delta exceeds threshold (%d files), triggering early reindex", path, len(dirty))
				p.state.SetStatus(path, RepoStale)
				if p.config.ReindexGapMode == "blackout" {
					p.state.SetDelta(path, nil)
				}
				p.Reindex.TriggerReindex(path)
			}
		} else if len(dirty) == 0 {
			p.state.SetDelta(path, nil)
		}
	}
}

func (p *Poller) deltaExceedsThreshold(dirtyCount int, delta *DeltaIndex) bool {
	if dirtyCount > p.config.MaxDirtyFiles {
		return true
	}
	if delta == nil {
		return false
	}
	var totalBytes int64
	for _, data := range delta.Files {
		totalBytes += int64(len(data))
	}
	return totalBytes > p.config.MaxDeltaBytes
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
