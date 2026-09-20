package rapid

import (
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	// Roots to scan for git repos.
	Roots []string

	// How deep to look for .git directories under each root.
	ScanDepth int

	// Glob patterns matched against repo paths to exclude.
	ExcludePatterns []string

	// Port for the zoekt-vanzelf proxy.
	ProxyHost string
	ProxyPort int

	// Upstream zoekt-webserver URL.
	ZoektURL string

	// How often to poll each repo for state changes.
	RepoPollInterval time.Duration

	// How often to scan for new/removed repos.
	DiscoveryInterval time.Duration

	// How often to do a full reindex of all repos.
	ReindexInterval time.Duration

	// Directory for zoekt index shards.
	DataDir string

	// Max concurrent reindex jobs.
	MaxConcurrentReindex int

	// Trigger early reindex if delta exceeds these thresholds.
	MaxDirtyFiles int
	MaxDeltaBytes int64

	// Directory names to skip when setting up fsnotify watches.
	WatchSkipDirs []string

	// What to do during reindex gap: "stale" or "blackout".
	ReindexGapMode string

	// Index the remote default branch (origin/main) alongside the working
	// tree's HEAD, so search covers what is actually on main — including
	// merged work that never landed in the local checkout.
	IndexRemoteDefault bool

	// How often to re-resolve remote-tracking refs. A fetch moves
	// refs/remotes/origin/* without touching HEAD, the index, or the working
	// tree, so neither the repo poll nor fsnotify can see it.
	RemoteRefInterval time.Duration
}

func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Roots:           []string{filepath.Join(home, "src")},
		ScanDepth:       3,
		ExcludePatterns: []string{"*/node_modules/*", "*/vendor/*", "*/.terraform/*"},
		// Loopback by default: this proxy serves a searchable index of every
		// repo under ~/src, and binding 0.0.0.0 hands that to the whole LAN.
		ProxyHost:            "127.0.0.1",
		ProxyPort:            6071,
		ZoektURL:             "http://localhost:6070",
		RepoPollInterval:     10 * time.Second,
		DiscoveryInterval:    60 * time.Second,
		ReindexInterval:      time.Hour,
		DataDir:              filepath.Join(home, ".zoekt"),
		MaxConcurrentReindex: 2,
		MaxDirtyFiles:        500,
		MaxDeltaBytes:        50 * 1024 * 1024, // 50 MB
		WatchSkipDirs:        []string{"node_modules", "vendor", ".terraform", ".next", "__pycache__", ".build", "dist", ".cache"},
		ReindexGapMode:       "stale",
		IndexRemoteDefault:   true,
		RemoteRefInterval:    60 * time.Second,
	}
}
