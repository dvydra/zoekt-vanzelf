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

	// A zero interval disables the remote-ref check; receiving from a nil
	// channel blocks forever, so the select arm simply never fires.
	var remoteRefC <-chan time.Time
	if p.config.RemoteRefInterval > 0 {
		remoteRefTicker := time.NewTicker(p.config.RemoteRefInterval)
		defer remoteRefTicker.Stop()
		remoteRefC = remoteRefTicker.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-pollTicker.C:
			p.pollAll()
		case <-discoveryTicker.C:
			p.discoverAndPoll()
		case <-remoteRefC:
			p.checkRemoteRefs()
		}
	}
}

// checkRemoteRefs re-resolves every repo's remote default branch and reindexes
// the ones whose shard is behind it.
//
// A fetch writes refs/remotes/origin/* and nothing else — no HEAD move, no
// index write, no working tree change — so `git status` sees nothing and the
// fsnotify watcher (which skips .git entirely) sees nothing. Polling the refs
// is the only way to notice, and the shard's own record of each branch's SHA is
// the comparison point, so this survives restarts without extra state.
func (p *Poller) checkRemoteRefs() {
	if p.Reindex == nil || p.Proxy == nil || !p.config.IndexRemoteDefault {
		return
	}

	for _, path := range p.state.Paths() {
		rr, indexed, need := p.remoteRefNeedsReindex(path)
		if !need {
			continue
		}
		if indexed == "" {
			log.Printf("[%s] %s missing from shard — reindexing to add it", path, rr.Branch)
		} else {
			log.Printf("[%s] %s moved: %s → %s", path, rr.Branch, shortSHA(indexed), shortSHA(rr.SHA))
		}

		p.state.SetStatus(path, RepoStale)
		p.Reindex.TriggerReindex(path)
	}
}

// remoteRefNeedsReindex reports whether a repo's shard is behind its remote
// default branch, returning the resolved ref and the SHA the shard currently
// holds for it ("" when the shard has no such branch).
func (p *Poller) remoteRefNeedsReindex(path string) (RemoteRef, string, bool) {
	if p.Reindex != nil && p.Reindex.IsBusy(path) {
		return RemoteRef{}, "", false
	}
	// Leave failing repos to the hourly full reindex rather than retrying them
	// every cycle.
	if s := p.state.Get(path); s != nil && s.Status == RepoError {
		return RemoteRef{}, "", false
	}

	rr, err := GetRemoteDefault(path)
	if err != nil {
		log.Printf("[%s] remote ref check failed: %v", path, err)
		return RemoteRef{}, "", false
	}
	if rr.Branch == "" {
		return rr, "", false // local-only repo, nothing to track
	}

	indexed := p.Proxy.IndexedBranchSHA(path, rr.Branch)
	return rr, indexed, indexed != rr.SHA
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
