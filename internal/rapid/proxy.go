package rapid

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ZoektSearchRequest is the JSON body sent to zoekt's /api/search.
// We keep the raw body to forward to zoekt, but parse q for delta search.
type ZoektSearchRequest struct {
	Query string `json:"q"`
	Num   int    `json:"num,omitempty"`
	Opts  *struct {
		ChunkMatches bool `json:"ChunkMatches,omitempty"`
	} `json:"opts,omitempty"`
}

// ZoektSearchResponse wraps the raw zoekt JSON so we preserve all fields.
type ZoektSearchResponse struct {
	raw map[string]any
}

func (r *ZoektSearchResponse) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.raw)
}

// SearchProxy handles search requests by querying zoekt and merging delta results.
type SearchProxy struct {
	zoektURL string
	state    *StateTable
	client   *http.Client

	// Repo name mappings from zoekt's /api/list.
	mu          sync.RWMutex
	nameToPath  map[string]string   // zoekt repo name → local path
	pathToNames map[string][]string // local path → zoekt repo names (can be multiple)
	pathToSHA   map[string]string   // local path → indexed SHA from zoekt

	// local path → branch name → indexed SHA. The shard's own record of what
	// each branch was at index time; used to spot a fetched remote branch that
	// the shard hasn't caught up with.
	pathToBranchSHA map[string]map[string]string

	// Shard names already reported as stale duplicates, so the warning is
	// logged once rather than on every refresh.
	staleShardWarned map[string]bool
}

func NewSearchProxy(zoektURL string, state *StateTable) *SearchProxy {
	return &SearchProxy{
		zoektURL:    zoektURL,
		state:       state,
		client:      &http.Client{},
		nameToPath:  make(map[string]string),
		pathToNames: make(map[string][]string),
		pathToSHA:   make(map[string]string),

		pathToBranchSHA: make(map[string]map[string]string),

		staleShardWarned: make(map[string]bool),
	}
}

// RefreshRepoMap queries zoekt's /api/list and builds the repo name ↔ path mapping.
//
// A path can have more than one shard: when a repo's remote URL changes, the
// next index writes a shard under the new name and the old one stays behind in
// the index dir. Zoekt lists and searches both. Every name still maps to the
// path so search hits from either shard merge correctly, but branch SHAs are
// taken from the newest shard only. Merging them last-wins let a stale shard
// report origin/main as behind forever, reindexing the repo every cycle.
func (p *SearchProxy) RefreshRepoMap() {
	type branchInfo struct {
		Name    string `json:"Name"`
		Version string `json:"Version"`
	}
	type listResponse struct {
		List struct {
			Repos []struct {
				Repository struct {
					Name     string       `json:"Name"`
					Source   string       `json:"Source"`
					Branches []branchInfo `json:"Branches"`
				} `json:"Repository"`
				IndexMetadata struct {
					IndexTime string `json:"IndexTime"`
				} `json:"IndexMetadata"`
			} `json:"Repos"`
		} `json:"List"`
	}

	url := strings.TrimSuffix(p.zoektURL, "/") + "/api/list"
	resp, err := p.client.Post(url, "application/json", strings.NewReader("{}"))
	if err != nil {
		log.Printf("failed to query zoekt /api/list: %v", err)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("failed to read zoekt /api/list: %v", err)
		return
	}

	var lr listResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		log.Printf("failed to parse zoekt /api/list: %v", err)
		return
	}

	type shard struct {
		name    string
		indexed time.Time
	}

	nameToPath := make(map[string]string)
	pathToNames := make(map[string][]string)
	pathToSHA := make(map[string]string)
	pathToBranchSHA := make(map[string]map[string]string)
	newest := make(map[string]shard) // path → shard whose branch SHAs we trust
	var stale []struct {
		path, name, winner string
		indexed            time.Time
	}

	for _, r := range lr.List.Repos {
		name := r.Repository.Name
		source := strings.TrimSuffix(r.Repository.Source, "/")
		if source == "" || name == "" {
			continue
		}
		// Resolve to absolute path.
		abs, err := filepath.Abs(source)
		if err != nil {
			continue
		}
		nameToPath[name] = abs
		pathToNames[abs] = append(pathToNames[abs], name)

		// A missing or unparseable IndexTime reads as the zero time, so any
		// dated shard beats it.
		indexed, _ := time.Parse(time.RFC3339Nano, r.IndexMetadata.IndexTime)
		cur := shard{name: name, indexed: indexed}
		if prev, seen := newest[abs]; seen {
			loser := cur
			if cur.indexed.After(prev.indexed) {
				loser = prev
				newest[abs] = cur
			}
			stale = append(stale, struct {
				path, name, winner string
				indexed            time.Time
			}{abs, loser.name, newest[abs].name, loser.indexed})
			if loser.name == cur.name {
				continue
			}
		} else {
			newest[abs] = cur
		}

		// Record every indexed branch of the newest shard; HEAD also gets its
		// own fast path.
		branchSHA := make(map[string]string, len(r.Repository.Branches))
		pathToBranchSHA[abs] = branchSHA
		delete(pathToSHA, abs)
		for _, b := range r.Repository.Branches {
			if b.Version == "" {
				continue
			}
			branchSHA[b.Name] = b.Version
			if b.Name == "HEAD" {
				pathToSHA[abs] = b.Version
			}
		}
	}

	p.mu.Lock()
	p.nameToPath = nameToPath
	p.pathToNames = pathToNames
	p.pathToSHA = pathToSHA
	p.pathToBranchSHA = pathToBranchSHA
	for _, s := range stale {
		if p.staleShardWarned[s.name] {
			continue
		}
		p.staleShardWarned[s.name] = true
		log.Printf("[%s] stale shard %q (indexed %s) for this path; using %q. Delete the old shard file to drop it from search results.",
			s.path, s.name, s.indexed.Format(time.RFC3339), s.winner)
	}
	p.mu.Unlock()

	log.Printf("refreshed repo map: %d zoekt repos mapped", len(nameToPath))
}

// IndexedSHA returns the SHA that zoekt has indexed for a given repo path.
// Returns empty string if unknown.
func (p *SearchProxy) IndexedSHA(repoPath string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pathToSHA[repoPath]
}

// IndexedBranchSHA returns the SHA zoekt has indexed for a specific branch of a
// repo, or empty string if that branch isn't in the shard at all. An absent
// branch reads the same as an out-of-date one, which is what we want: a shard
// built before remote-branch indexing was enabled needs the same reindex a
// stale one does.
func (p *SearchProxy) IndexedBranchSHA(repoPath, branch string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pathToBranchSHA[repoPath][branch]
}

// Search forwards a query to zoekt and merges delta index results.
// Works with raw JSON to preserve all fields zoekt returns.
// reqBody is the raw request JSON to forward to zoekt.
// rawQuery is the zoekt query string for delta search. useChunkMatches controls delta result format.
func (p *SearchProxy) Search(reqBody []byte, rawQuery string, useChunkMatches bool) (*ZoektSearchResponse, error) {
	// Forward raw request to zoekt.
	rawResp, err := p.queryZoektRaw(reqBody)
	if err != nil {
		return nil, fmt.Errorf("zoekt query: %w", err)
	}

	// Extract the Result object.
	resultRaw, ok := rawResp["Result"]
	if !ok {
		return &ZoektSearchResponse{raw: rawResp}, nil
	}
	result, ok := resultRaw.(map[string]any)
	if !ok {
		return &ZoektSearchResponse{raw: rawResp}, nil
	}

	// Extract Files array.
	filesRaw, _ := result["Files"]
	var files []any
	if filesRaw != nil {
		files, _ = filesRaw.([]any)
	}

	// Get mappings and state.
	p.mu.RLock()
	nameToPath := p.nameToPath
	pathToNames := p.pathToNames
	p.mu.RUnlock()

	allStates := p.state.All()

	// Filter zoekt results: suppress matches for dirty paths.
	var filteredFiles []any
	for _, fileRaw := range files {
		file, ok := fileRaw.(map[string]any)
		if !ok {
			filteredFiles = append(filteredFiles, fileRaw)
			continue
		}

		repoName, _ := file["Repository"].(string)
		fileName, _ := file["FileName"].(string)

		repoPath, ok := nameToPath[repoName]
		if !ok {
			filteredFiles = append(filteredFiles, fileRaw)
			continue
		}

		state, ok := allStates[repoPath]
		if !ok || state.DeltaIndex == nil {
			filteredFiles = append(filteredFiles, fileRaw)
			continue
		}

		if state.DeltaIndex.IsDirty(fileName) {
			continue
		}

		filteredFiles = append(filteredFiles, fileRaw)
	}

	// Parse zoekt query to extract the search pattern and filters for delta search.
	pq := ParseZoektQuery(rawQuery)

	// Add delta matches for all repos that have deltas.
	for path, state := range allStates {
		if state.DeltaIndex == nil || len(state.DeltaIndex.Files) == 0 {
			continue
		}

		deltaMatches, err := state.DeltaIndex.Search(pq.Pattern, &pq)
		if err != nil || len(deltaMatches) == 0 {
			continue
		}

		// Group delta matches by file.
		byFile := make(map[string][]DeltaMatch)
		for _, m := range deltaMatches {
			byFile[m.Path] = append(byFile[m.Path], m)
		}

		// Use zoekt's name for this repo if available, otherwise derive from path.
		names := pathToNames[path]
		repoName := deriveName(path)
		if len(names) > 0 {
			repoName = names[0]
		}

		for filePath, matches := range byFile {
			zf := deltaMatchesToRaw(repoName, filePath, matches, useChunkMatches)
			filteredFiles = append(filteredFiles, zf)
		}
	}

	result["Files"] = filteredFiles
	// Don't overwrite FileCount — zoekt's FileCount means total files searched,
	// not the number of matching files. Let the original value stand.
	rawResp["Result"] = result

	return &ZoektSearchResponse{raw: rawResp}, nil
}

func (p *SearchProxy) queryZoektRaw(reqBody []byte) (map[string]any, error) {
	url := strings.TrimSuffix(p.zoektURL, "/") + "/api/search"
	resp, err := p.client.Post(url, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zoekt returned %d: %s", resp.StatusCode, string(body))
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse zoekt response: %w", err)
	}

	return raw, nil
}

// deltaMatchesToRaw converts delta matches for a single file into a raw JSON-compatible map.
func deltaMatchesToRaw(repoName, filePath string, matches []DeltaMatch, useChunkMatches bool) map[string]any {
	result := map[string]any{
		"FileName":   filePath,
		"Repository": repoName,
		"Version":    "",
		"Language":   "",
		"Branches":   []string{"HEAD"},
		"Checksum":   "",
		"Score":      1.0,
	}

	if useChunkMatches {
		var chunkMatches []map[string]any
		for _, m := range matches {
			contentBytes := []byte(m.Line + "\n")
			chunkMatches = append(chunkMatches, map[string]any{
				"Content": contentBytes,
				"ContentStart": map[string]any{
					"ByteOffset": 0,
					"LineNumber":  m.LineNumber,
					"Column":      1,
				},
				"FileName": false,
				"Ranges": []map[string]any{
					{
						"Start": map[string]any{
							"ByteOffset": m.MatchStart,
							"LineNumber":  m.LineNumber,
							"Column":      m.MatchStart + 1,
						},
						"End": map[string]any{
							"ByteOffset": m.MatchEnd,
							"LineNumber":  m.LineNumber,
							"Column":      m.MatchEnd + 1,
						},
					},
				},
				"SymbolInfo": nil,
				"Score":      1.0,
				"DebugScore": "",
			})
		}
		result["ChunkMatches"] = chunkMatches
	} else {
		var lineMatches []map[string]any
		for _, m := range matches {
			encoded := base64.StdEncoding.EncodeToString([]byte(m.Line + "\n"))
			lineMatches = append(lineMatches, map[string]any{
				"Line":       encoded,
				"LineStart":  0,
				"LineEnd":    0,
				"LineNumber":  m.LineNumber,
				"Before":     nil,
				"After":      nil,
				"FileName":   false,
				"Score":      1.0,
				"DebugScore": "",
				"LineFragments": []map[string]any{
					{
						"LineOffset":  m.MatchStart,
						"Offset":      0,
						"MatchLength": m.MatchEnd - m.MatchStart,
						"SymbolInfo":  nil,
					},
				},
			})
		}
		result["LineMatches"] = lineMatches
	}

	return result
}

// deriveName creates a simple repo name from a filesystem path.
// Used as fallback when zoekt doesn't know about the repo yet.
func deriveName(absPath string) string {
	return filepath.Base(absPath)
}
