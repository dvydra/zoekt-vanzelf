package rapid

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Server is the HTTP server for zoekt-vanzelf.
type Server struct {
	proxy     *SearchProxy
	state     *StateTable
	reindex   *ReindexManager
	poller    *Poller
	scheduler *Scheduler
	host      string
	port      int
	zoektURL  string
	client    *http.Client
	startedAt time.Time
}

func NewServer(proxy *SearchProxy, state *StateTable, reindex *ReindexManager, poller *Poller, scheduler *Scheduler, host string, port int, zoektURL string) *Server {
	return &Server{
		proxy:     proxy,
		state:     state,
		reindex:   reindex,
		poller:    poller,
		scheduler: scheduler,
		host:      host,
		port:      port,
		zoektURL:  zoektURL,
		client:    &http.Client{},
		startedAt: time.Now(),
	}
}

// Addr is the address the server binds. An empty host would mean every
// interface, which this server must never default to: it proxies a searchable
// index of every repository under the configured roots.
func (s *Server) Addr() string {
	host := s.host
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(s.port))
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", s.handleSearch)
	mux.HandleFunc("/api/list", s.handlePassthrough)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/reindex/", s.handleReindexRepo)
	mux.HandleFunc("/api/reindex", s.handleReindex)
	mux.HandleFunc("/api/rescan", s.handleRescan)

	addr := s.Addr()
	log.Printf("listening on %s", addr)
	return http.ListenAndServe(addr, mux)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	var req ZoektSearchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.Query == "" {
		http.Error(w, "missing query", http.StatusBadRequest)
		return
	}

	useChunkMatches := req.Opts != nil && req.Opts.ChunkMatches
	resp, err := s.proxy.Search(body, req.Query, useChunkMatches)
	if err != nil {
		log.Printf("search error: %v", err)
		http.Error(w, "search error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handlePassthrough forwards any request directly to zoekt-webserver.
func (s *Server) handlePassthrough(w http.ResponseWriter, r *http.Request) {
	url := strings.TrimSuffix(s.zoektURL, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, url, r.Body)
	if err != nil {
		http.Error(w, "proxy error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	proxyReq.Header = r.Header.Clone()

	resp, err := s.client.Do(proxyReq)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// StatusResponse is the JSON response for GET /api/status.
type StatusResponse struct {
	Uptime           string                  `json:"uptime"`
	RepoCount        int                     `json:"repo_count"`
	NextFullReindex  string                  `json:"next_full_reindex"`
	Repos            map[string]RepoStatusDTO `json:"repos"`
}

type RepoStatusDTO struct {
	Path           string `json:"path"`
	Branch         string `json:"branch"`
	HeadSHA        string `json:"head"`
	IndexedSHA     string `json:"indexed_sha"`
	IndexedAt      string `json:"indexed_at,omitempty"`
	DirtyFiles     int    `json:"dirty_files"`
	DeltaSizeBytes int64  `json:"delta_size_bytes"`
	Status         string `json:"status"`
	Reindexing     bool   `json:"reindexing"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	allStates := s.state.All()
	repos := make(map[string]RepoStatusDTO, len(allStates))

	for path, state := range allStates {
		var deltaBytes int64
		if state.DeltaIndex != nil {
			for _, data := range state.DeltaIndex.Files {
				deltaBytes += int64(len(data))
			}
		}

		indexedAt := ""
		if !state.IndexedAt.IsZero() {
			indexedAt = state.IndexedAt.Format(time.RFC3339)
		}

		repos[path] = RepoStatusDTO{
			Path:           path,
			Branch:         state.Branch,
			HeadSHA:        state.HeadSHA,
			IndexedSHA:     state.IndexedSHA,
			IndexedAt:      indexedAt,
			DirtyFiles:     len(state.DirtyFiles),
			DeltaSizeBytes: deltaBytes,
			Status:         state.Status.String(),
			Reindexing:     s.reindex.IsBusy(path),
		}
	}

	resp := StatusResponse{
		Uptime:          time.Since(s.startedAt).Round(time.Second).String(),
		RepoCount:       len(repos),
		NextFullReindex: s.scheduler.NextReindexAt().Format(time.RFC3339),
		Repos:           repos,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleReindex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.reindex.ReindexAll()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "reindex triggered"})
}

func (s *Server) handleReindexRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract repo name from path: /api/reindex/{repo}
	repo := strings.TrimPrefix(r.URL.Path, "/api/reindex/")
	if repo == "" {
		http.Error(w, "missing repo path", http.StatusBadRequest)
		return
	}

	// Find matching repo by path suffix.
	var found string
	for _, path := range s.state.Paths() {
		if strings.HasSuffix(path, "/"+repo) || path == repo {
			found = path
			break
		}
	}

	if found == "" {
		http.Error(w, "repo not found: "+repo, http.StatusNotFound)
		return
	}

	s.reindex.TriggerReindex(found)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "reindex triggered", "repo": found})
}

func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	go s.poller.discoverAndPoll()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "rescan triggered"})
}
