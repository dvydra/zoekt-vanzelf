package rapid

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

const gitTimeout = 10 * time.Second

// BranchHead holds the current branch name and HEAD SHA for a repo.
type BranchHead struct {
	Branch string
	SHA    string
}

// DirtyFile represents a file whose working tree content differs from HEAD.
type DirtyFile struct {
	Path   string
	Status FileStatus
}

type FileStatus int

const (
	FileModified  FileStatus = iota // tracked file with changes
	FileAdded                       // new untracked file
	FileDeleted                     // tracked file deleted from worktree
	FileRenamed                     // renamed file
	FileCopied                      // copied file
	FileUnmerged                    // unmerged (conflict)
)

func (s FileStatus) String() string {
	switch s {
	case FileModified:
		return "modified"
	case FileAdded:
		return "added"
	case FileDeleted:
		return "deleted"
	case FileRenamed:
		return "renamed"
	case FileCopied:
		return "copied"
	case FileUnmerged:
		return "unmerged"
	default:
		return "unknown"
	}
}

// GetRepoState returns the branch, HEAD SHA, and dirty files for a repo from a
// single `git status --porcelain=v2 --branch` invocation. The --branch flag
// prepends "# branch.oid" / "# branch.head" header lines, so we get everything
// pollRepo needs from one git subprocess instead of three (the old path forked
// symbolic-ref + rev-parse + status). At 28 repos polled on a timer this is the
// dominant CPU cost, so collapsing 3 forks into 1 matters.
func GetRepoState(repoPath string) (BranchHead, []DirtyFile, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "status", "--porcelain=v2", "--branch")
	out, err := cmd.Output()
	if err != nil {
		return BranchHead{}, nil, fmt.Errorf("git status: %w", err)
	}

	bh := parseBranchHeader(out)
	files, err := ParsePorcelainV2(out) // file entries only; "# branch.*" headers are ignored
	if err != nil {
		return BranchHead{}, nil, err
	}
	return bh, files, nil
}

// GetIgnoredDirs returns the gitignored directories for a repo as paths
// relative to the repo root (no trailing slash). It's used by the fsnotify
// watcher to prune entire subtrees (node_modules, build output, live data/log
// dirs): their contents never appear in `git status`, so they never enter the
// delta index, so watching them produces only no-op polls.
//
// One `git ls-files --directory` call per repo collapses each ignored dir to a
// single "dir/" entry instead of listing its contents. Ignored individual
// files are skipped — we only prune directories.
func GetIgnoredDirs(repoPath string) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", repoPath,
		"-c", "core.quotePath=false", // keep non-ASCII paths unquoted so prefixes match
		"ls-files", "--others", "--ignored", "--exclude-standard", "--directory")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}

	dirs := make(map[string]bool)
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		// Only directories carry a trailing slash; ignored files don't and
		// aren't worth pruning.
		if line[len(line)-1] != '/' {
			continue
		}
		dirs[string(line[:len(line)-1])] = true
	}
	return dirs, nil
}

// parseBranchHeader extracts the branch name and HEAD SHA from the "# branch.*"
// header lines emitted by `git status --porcelain=v2 --branch`. Its output
// matches the old symbolic-ref/rev-parse pair so repo-change detection is
// unchanged:
//   - an empty repo reports "# branch.oid (initial)" -> SHA stays ""
//   - a detached HEAD reports "# branch.head (detached)" -> branch "HEAD"
//     (the same value `git rev-parse --abbrev-ref HEAD` returned before)
func parseBranchHeader(data []byte) BranchHead {
	var bh BranchHead
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		switch {
		case bytes.HasPrefix(line, []byte("# branch.oid ")):
			oid := string(bytes.TrimSpace(line[len("# branch.oid "):]))
			if oid != "(initial)" {
				bh.SHA = oid
			}
		case bytes.HasPrefix(line, []byte("# branch.head ")):
			head := string(bytes.TrimSpace(line[len("# branch.head "):]))
			if head == "(detached)" {
				head = "HEAD"
			}
			bh.Branch = head
		}
	}
	return bh
}

// ParsePorcelainV2 parses the output of `git status --porcelain=v2`.
func ParsePorcelainV2(data []byte) ([]DirtyFile, error) {
	var files []DirtyFile

	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		switch line[0] {
		case '1': // ordinary changed entry
			f, err := parseOrdinaryEntry(line)
			if err != nil {
				continue
			}
			files = append(files, f)

		case '2': // rename or copy entry
			rf, err := parseRenameEntry(line)
			if err != nil {
				continue
			}
			files = append(files, rf...)

		case 'u': // unmerged entry
			f, err := parseUnmergedEntry(line)
			if err != nil {
				continue
			}
			files = append(files, f)

		case '?': // untracked
			// Format: ? <path>
			if len(line) > 2 {
				files = append(files, DirtyFile{
					Path:   string(line[2:]),
					Status: FileAdded,
				})
			}

		case '!': // ignored — skip
			continue
		}
	}

	return files, nil
}

// parseOrdinaryEntry parses a porcelain v2 "1" line.
// Format: 1 XY sub mH mI mW hH hI <path>
func parseOrdinaryEntry(line []byte) (DirtyFile, error) {
	fields := bytes.Fields(line)
	if len(fields) < 9 {
		return DirtyFile{}, fmt.Errorf("malformed ordinary entry: %s", line)
	}

	xy := string(fields[1])
	path := string(fields[8])

	status := classifyXY(xy)
	return DirtyFile{Path: path, Status: status}, nil
}

// parseRenameEntry parses a porcelain v2 "2" line.
// Format: 2 XY sub mH mI mW hH hI Xscore <path>\t<origPath>
// Returns two dirty files: the new path (renamed) and the old path (deleted).
func parseRenameEntry(line []byte) ([]DirtyFile, error) {
	// The last portion after the 9th space-separated field is "<newpath>\t<oldpath>".
	// We can't use bytes.Fields because it splits on tabs too.
	// Instead, split on tab first to separate paths, then parse the prefix.
	tabIdx := bytes.IndexByte(line, '\t')
	if tabIdx < 0 {
		return nil, fmt.Errorf("malformed rename entry (no tab): %s", line)
	}

	prefix := line[:tabIdx]  // "2 XY sub mH mI mW hH hI Xscore <newpath>"
	origPath := string(bytes.TrimSpace(line[tabIdx+1:]))

	fields := bytes.Fields(prefix)
	if len(fields) < 10 {
		return nil, fmt.Errorf("malformed rename entry: %s", line)
	}
	newPath := string(fields[9])

	result := []DirtyFile{
		{Path: newPath, Status: FileRenamed},
	}
	if origPath != "" && origPath != newPath {
		result = append(result, DirtyFile{Path: origPath, Status: FileDeleted})
	}
	return result, nil
}

// parseUnmergedEntry parses a porcelain v2 "u" line.
// Format: u XY sub m1 m2 m3 mW h1 h2 h3 <path>
func parseUnmergedEntry(line []byte) (DirtyFile, error) {
	fields := bytes.Fields(line)
	if len(fields) < 11 {
		return DirtyFile{}, fmt.Errorf("malformed unmerged entry: %s", line)
	}

	path := string(fields[10])
	return DirtyFile{Path: path, Status: FileUnmerged}, nil
}

// classifyXY maps the two-character XY status to a FileStatus.
// X = index status, Y = worktree status. We care about both.
func classifyXY(xy string) FileStatus {
	if len(xy) < 2 {
		return FileModified
	}

	// If worktree shows deletion.
	if xy[1] == 'D' {
		return FileDeleted
	}
	// If index shows deletion.
	if xy[0] == 'D' {
		return FileDeleted
	}
	// If index shows addition.
	if xy[0] == 'A' {
		return FileAdded
	}

	return FileModified
}

// RemoteRef identifies a remote-tracking branch and the commit it points at.
type RemoteRef struct {
	Branch string // short name, e.g. "origin/main"; empty if the repo has none
	SHA    string
}

// GetRemoteDefault resolves a repo's remote default branch and its SHA.
// Resolution order matches what a clone leaves behind: the
// refs/remotes/origin/HEAD symref first, then origin/main, then origin/master.
//
// A repo with no origin returns a zero RemoteRef and no error — local-only
// scratch repos are normal under ~/src, and having nothing to index there is
// not a failure.
//
// One `git for-each-ref` fork covers all three candidates. This runs on the
// slower remote-ref ticker, not the repo poll, so it leaves the poller's
// one-fork-per-repo-per-cycle budget alone.
func GetRemoteDefault(repoPath string) (RemoteRef, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "for-each-ref",
		"--format=%(refname:short)\t%(objectname)\t%(symref:short)",
		"refs/remotes/origin/HEAD",
		"refs/remotes/origin/main",
		"refs/remotes/origin/master",
	)
	out, err := cmd.Output()
	if err != nil {
		return RemoteRef{}, fmt.Errorf("git for-each-ref: %w", err)
	}
	return parseRemoteDefault(out), nil
}

// parseRemoteDefault picks the default branch out of for-each-ref output.
//
// refs/remotes/origin/HEAD shortens to just "origin", so it's identified by
// carrying a symref target rather than by name; its objectname is already
// resolved through the symref. A symref pointing at a deleted branch prints an
// empty objectname, which falls through to the main/master candidates.
func parseRemoteDefault(out []byte) RemoteRef {
	var symref RemoteRef
	byName := make(map[string]string, 2)

	for _, line := range bytes.Split(out, []byte("\n")) {
		fields := bytes.SplitN(line, []byte("\t"), 3)
		if len(fields) < 3 {
			continue
		}
		name, sha, target := string(fields[0]), string(fields[1]), string(fields[2])
		if target != "" {
			if sha != "" {
				symref = RemoteRef{Branch: target, SHA: sha}
			}
			continue
		}
		if sha != "" {
			byName[name] = sha
		}
	}

	if symref.Branch != "" {
		return symref
	}
	for _, candidate := range []string{"origin/main", "origin/master"} {
		if sha := byName[candidate]; sha != "" {
			return RemoteRef{Branch: candidate, SHA: sha}
		}
	}
	return RemoteRef{}
}
