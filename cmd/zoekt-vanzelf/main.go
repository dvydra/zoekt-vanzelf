package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dvydra/zoekt-vanzelf/internal/rapid"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "discover":
		cmdDiscover(os.Args[2:])
	case "poll":
		cmdPoll(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "reindex":
		cmdReindex(os.Args[2:])
	case "rescan":
		cmdRescan(os.Args[2:])
	case "version":
		fmt.Println("zoekt-vanzelf", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `zoekt-vanzelf - fast code search with working tree awareness

Commands:
  discover    Scan for git repos under configured roots
  poll        Poll repos for state changes (debug mode)
  serve       Start the search proxy
  status      Show repo states from running proxy
  reindex     Trigger reindex (all or specific repo)
  rescan      Re-discover repos
  version     Print version
  help        Show this help`)
}

func cmdDiscover(args []string) {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	rootsFlag := fs.String("roots", "", "comma-separated root directories (default: ~/src)")
	depthFlag := fs.Int("depth", 0, "max scan depth (default: 3)")
	fs.Parse(args)

	cfg := rapid.DefaultConfig()

	if *rootsFlag != "" {
		cfg.Roots = strings.Split(*rootsFlag, ",")
	}
	if *depthFlag > 0 {
		cfg.ScanDepth = *depthFlag
	}

	repos, err := rapid.DiscoverRepos(cfg.Roots, cfg.ScanDepth, cfg.ExcludePatterns)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Found %d repos:\n", len(repos))
	for _, repo := range repos {
		fmt.Println(repo)
	}
}

func cmdPoll(args []string) {
	fs := flag.NewFlagSet("poll", flag.ExitOnError)
	rootsFlag := fs.String("roots", "", "comma-separated root directories (default: ~/src)")
	fs.Parse(args)

	cfg := rapid.DefaultConfig()
	if *rootsFlag != "" {
		cfg.Roots = strings.Split(*rootsFlag, ",")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	state := rapid.NewStateTable()
	poller := rapid.NewPoller(cfg, state)

	fmt.Fprintln(os.Stderr, "polling repos (ctrl-c to stop)...")
	poller.Run(ctx)
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	rootsFlag := fs.String("roots", "", "comma-separated root directories (default: ~/src)")
	portFlag := fs.Int("port", 0, "proxy listen port (default: 6071)")
	hostFlag := fs.String("host", "", "proxy listen address (default: 127.0.0.1; use 0.0.0.0 to publish on the network)")
	zoektFlag := fs.String("zoekt", "", "upstream zoekt URL (default: http://localhost:6070)")
	fs.Parse(args)

	cfg := rapid.DefaultConfig()
	if *rootsFlag != "" {
		cfg.Roots = strings.Split(*rootsFlag, ",")
	}
	if *portFlag > 0 {
		cfg.ProxyPort = *portFlag
	}
	if *hostFlag != "" {
		cfg.ProxyHost = *hostFlag
	}
	if *zoektFlag != "" {
		cfg.ZoektURL = *zoektFlag
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	state := rapid.NewStateTable()
	proxy := rapid.NewSearchProxy(cfg.ZoektURL, state)
	reindexMgr := rapid.NewReindexManager(cfg, state, proxy)
	poller := rapid.NewPoller(cfg, state)
	poller.Reindex = reindexMgr
	poller.Proxy = proxy
	scheduler := rapid.NewScheduler(cfg, reindexMgr)
	srv := rapid.NewServer(proxy, state, reindexMgr, poller, scheduler, cfg.ProxyHost, cfg.ProxyPort, cfg.ZoektURL)

	// Refresh repo map from zoekt on startup (needed for smart startup).
	proxy.RefreshRepoMap()

	// Start fsnotify watcher for instant file change detection.
	watcher, err := rapid.NewWatcher(poller, state, cfg.WatchSkipDirs)
	if err != nil {
		log.Printf("fsnotify unavailable, falling back to polling only: %v", err)
	} else {
		poller.Watcher = watcher
		go watcher.Run()
		defer watcher.Close()
	}

	// Start poller in background.
	go poller.Run(ctx)

	// Start hourly reindex scheduler.
	go scheduler.Run(ctx)

	// Periodically refresh the zoekt repo name map.
	go func() {
		ticker := time.NewTicker(cfg.DiscoveryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				proxy.RefreshRepoMap()
			}
		}
	}()

	fmt.Fprintf(os.Stderr, "zoekt-vanzelf proxy on %s → %s\n", net.JoinHostPort(cfg.ProxyHost, strconv.Itoa(cfg.ProxyPort)), cfg.ZoektURL)

	// Run server (blocks until error or signal).
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			cancel()
		}
	}()

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "\nshutting down, waiting for in-flight reindex jobs...")
	reindexMgr.Wait()
	fmt.Fprintln(os.Stderr, "done")
}

const (
	ansiReset  = "\033[0m"
	ansiDim    = "\033[90m"
	ansiGreen  = "\033[32m"
	ansiRed    = "\033[31m"
	ansiYellow = "\033[33m"
	ansiClear  = "\033[2J\033[H"
)

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	portFlag := fs.Int("port", 6071, "proxy port to query")
	liveFlag := fs.Bool("live", false, "live mode with change highlighting")
	intervalFlag := fs.Float64("interval", 1.0, "refresh interval in seconds (live mode)")
	fs.Parse(args)

	apiURL := fmt.Sprintf("http://localhost:%d/api/status", *portFlag)

	if *liveFlag {
		cmdStatusLive(apiURL, time.Duration(*intervalFlag*float64(time.Second)))
		return
	}

	status, err := fetchStatus(apiURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	renderStatus(status, nil, nil)
}

func cmdStatusLive(apiURL string, interval time.Duration) {
	var prev *rapid.StatusResponse
	// Track when each repo field last changed, for flash duration.
	flashes := make(map[string]*statusFlash) // key: "path:field"
	flashDur := 2 * time.Second

	// Hide cursor.
	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	for {
		status, err := fetchStatus(apiURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			time.Sleep(interval)
			continue
		}

		// Compute new flashes by diffing against previous state.
		if prev != nil {
			now := time.Now()
			for path, r := range status.Repos {
				old, existed := prev.Repos[path]
				if !existed {
					flashes[path+":dirty"] = &statusFlash{ansiGreen, now.Add(flashDur)}
					flashes[path+":branch"] = &statusFlash{ansiGreen, now.Add(flashDur)}
					flashes[path+":sha"] = &statusFlash{ansiGreen, now.Add(flashDur)}
					continue
				}
				if r.DirtyFiles != old.DirtyFiles {
					if r.DirtyFiles > old.DirtyFiles {
						flashes[path+":dirty"] = &statusFlash{ansiGreen, now.Add(flashDur)}
					} else {
						flashes[path+":dirty"] = &statusFlash{ansiRed, now.Add(flashDur)}
					}
				}
				if r.Branch != old.Branch {
					flashes[path+":branch"] = &statusFlash{ansiGreen, now.Add(flashDur)}
				}
				if r.HeadSHA != old.HeadSHA {
					flashes[path+":sha"] = &statusFlash{ansiGreen, now.Add(flashDur)}
				}
			}
		}

		// Expire old flashes.
		now := time.Now()
		for k, f := range flashes {
			if now.After(f.until) {
				delete(flashes, k)
			}
		}

		fmt.Print(ansiClear)
		renderStatus(status, prev, flashes)
		prev = status

		select {
		case <-sigCh:
			fmt.Print(ansiClear)
			return
		case <-time.After(interval):
		}
	}
}

func fetchStatus(apiURL string) (*rapid.StatusResponse, error) {
	resp, err := http.Get(apiURL)
	if err != nil {
		return nil, fmt.Errorf("cannot reach zoekt-vanzelf: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var status rapid.StatusResponse
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("parse error: %v", err)
	}
	return &status, nil
}

type statusFlash struct {
	color string
	until time.Time
}

func renderStatus(status *rapid.StatusResponse, prev *rapid.StatusResponse, flashes map[string]*statusFlash) {
	fmt.Printf("zoekt-vanzelf — %d repos, up %s\n", status.RepoCount, status.Uptime)
	fmt.Printf("next full reindex: %s\n\n", status.NextFullReindex)

	paths := make([]string, 0, len(status.Repos))
	for p := range status.Repos {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	home, _ := os.UserHomeDir()
	srcRoot := filepath.Join(home, "src") + "/"

	for _, path := range paths {
		r := status.Repos[path]
		relPath := path
		if cut, ok := strings.CutPrefix(path, srcRoot); ok {
			relPath = cut
		}
		dir := filepath.Dir(relPath)
		base := filepath.Base(relPath)
		var name string
		if dir == "." {
			name = base
		} else {
			name = fmt.Sprintf("%s%s/%s%s", ansiDim, dir, ansiReset, base)
		}

		// Branch + SHA with optional flash.
		branchStr := r.Branch
		shaStr := shortSHA(r.HeadSHA)
		if f, ok := flashes[path+":branch"]; ok {
			branchStr = f.color + branchStr + ansiReset
		}
		if f, ok := flashes[path+":sha"]; ok {
			shaStr = f.color + shaStr + ansiReset
		}

		// Dirty count with optional flash.
		dirty := ""
		if r.DirtyFiles > 0 {
			dirtyStr := fmt.Sprintf("%d dirty", r.DirtyFiles)
			if f, ok := flashes[path+":dirty"]; ok {
				dirtyStr = f.color + dirtyStr + ansiReset
			}
			dirty = fmt.Sprintf(" (%s)", dirtyStr)
		}

		reindexing := ""
		if r.Reindexing {
			reindexing = fmt.Sprintf(" %s[reindexing]%s", ansiYellow, ansiReset)
		}
		stale := ""
		if r.Status == "stale" || r.Status == "error" {
			stale = fmt.Sprintf(" %s[%s]%s", ansiRed, r.Status, ansiReset)
		}

		padding := 30 - len(relPath)
		if padding < 1 {
			padding = 1
		}
		fmt.Printf("  %s%*s%s@%s%s%s%s\n", name, padding, "", branchStr, shaStr, dirty, stale, reindexing)
	}
}

func cmdReindex(args []string) {
	fs := flag.NewFlagSet("reindex", flag.ExitOnError)
	portFlag := fs.Int("port", 6071, "proxy port to query")
	fs.Parse(args)

	remaining := fs.Args()
	var url string
	if len(remaining) > 0 {
		url = fmt.Sprintf("http://localhost:%d/api/reindex/%s", *portFlag, remaining[0])
	} else {
		url = fmt.Sprintf("http://localhost:%d/api/reindex", *portFlag)
	}

	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "error: %s\n", string(body))
		os.Exit(1)
	}
	fmt.Println(string(body))
}

func cmdRescan(args []string) {
	fs := flag.NewFlagSet("rescan", flag.ExitOnError)
	portFlag := fs.Int("port", 6071, "proxy port to query")
	fs.Parse(args)

	url := fmt.Sprintf("http://localhost:%d/api/rescan", *portFlag)
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Println(string(body))
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
