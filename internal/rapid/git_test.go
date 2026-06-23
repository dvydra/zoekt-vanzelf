package rapid

import (
	"testing"
)

func TestParsePorcelainV2_Modified(t *testing.T) {
	// Ordinary modified file (worktree changed).
	input := []byte("1 .M N... 100644 100644 100644 abc123 def456 src/main.go\n")

	files, err := ParsePorcelainV2(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].Path != "src/main.go" {
		t.Errorf("path = %q, want %q", files[0].Path, "src/main.go")
	}
	if files[0].Status != FileModified {
		t.Errorf("status = %v, want modified", files[0].Status)
	}
}

func TestParsePorcelainV2_Deleted(t *testing.T) {
	input := []byte("1 .D N... 100644 100644 000000 abc123 def456 old_file.go\n")

	files, err := ParsePorcelainV2(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].Status != FileDeleted {
		t.Errorf("status = %v, want deleted", files[0].Status)
	}
}

func TestParsePorcelainV2_Added(t *testing.T) {
	input := []byte("1 A. N... 000000 100644 100644 0000000 abc123 new_file.go\n")

	files, err := ParsePorcelainV2(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].Status != FileAdded {
		t.Errorf("status = %v, want added", files[0].Status)
	}
}

func TestParsePorcelainV2_Untracked(t *testing.T) {
	input := []byte("? newfile.txt\n")

	files, err := ParsePorcelainV2(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].Path != "newfile.txt" {
		t.Errorf("path = %q, want %q", files[0].Path, "newfile.txt")
	}
	if files[0].Status != FileAdded {
		t.Errorf("status = %v, want added", files[0].Status)
	}
}

func TestParsePorcelainV2_Rename(t *testing.T) {
	input := []byte("2 R. N... 100644 100644 100644 abc123 def456 R100 new.go\told.go\n")

	files, err := ParsePorcelainV2(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files (new + old tombstone), got %d: %+v", len(files), files)
	}
	if files[0].Path != "new.go" || files[0].Status != FileRenamed {
		t.Errorf("files[0] = %+v, want new.go/renamed", files[0])
	}
	if files[1].Path != "old.go" || files[1].Status != FileDeleted {
		t.Errorf("files[1] = %+v, want old.go/deleted", files[1])
	}
}

func TestParsePorcelainV2_Unmerged(t *testing.T) {
	input := []byte("u UU N... 100644 100644 100644 100644 abc123 def456 ghi789 conflict.go\n")

	files, err := ParsePorcelainV2(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].Status != FileUnmerged {
		t.Errorf("status = %v, want unmerged", files[0].Status)
	}
}

func TestParsePorcelainV2_Mixed(t *testing.T) {
	input := []byte(`1 .M N... 100644 100644 100644 abc123 def456 modified.go
1 .D N... 100644 100644 000000 abc123 def456 deleted.go
? untracked.txt
`)

	files, err := ParsePorcelainV2(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(files))
	}
}

func TestParsePorcelainV2_Empty(t *testing.T) {
	files, err := ParsePorcelainV2([]byte(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(files))
	}
}

func TestParsePorcelainV2_Ignored(t *testing.T) {
	input := []byte("! ignored_file.txt\n")

	files, err := ParsePorcelainV2(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("expected 0 files (ignored), got %d", len(files))
	}
}

func TestParseBranchHeader(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantBranch string
		wantSHA    string
	}{
		{
			name:       "normal branch",
			input:      "# branch.oid a6cef210878fa1081e78a4e8969c49e664c6dd45\n# branch.head main\n# branch.upstream origin/main\n# branch.ab +0 -0\n",
			wantBranch: "main",
			wantSHA:    "a6cef210878fa1081e78a4e8969c49e664c6dd45",
		},
		{
			// Detached HEAD must map to "HEAD" to match the value the old
			// `git rev-parse --abbrev-ref HEAD` returned, so state detection
			// doesn't see a spurious branch change.
			name:       "detached HEAD maps to HEAD",
			input:      "# branch.oid a6cef210878fa1081e78a4e8969c49e664c6dd45\n# branch.head (detached)\n",
			wantBranch: "HEAD",
			wantSHA:    "a6cef210878fa1081e78a4e8969c49e664c6dd45",
		},
		{
			// Empty repo (no commits) reports "(initial)" — SHA stays empty,
			// matching the old rev-parse-fails behavior.
			name:       "initial empty repo has no SHA",
			input:      "# branch.oid (initial)\n# branch.head main\n",
			wantBranch: "main",
			wantSHA:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bh := parseBranchHeader([]byte(tt.input))
			if bh.Branch != tt.wantBranch {
				t.Errorf("branch = %q, want %q", bh.Branch, tt.wantBranch)
			}
			if bh.SHA != tt.wantSHA {
				t.Errorf("sha = %q, want %q", bh.SHA, tt.wantSHA)
			}
		})
	}
}

func TestParsePorcelainV2_IgnoresBranchHeaders(t *testing.T) {
	// GetRepoState feeds the full `--branch` output through ParsePorcelainV2,
	// so the "# branch.*" header lines must be ignored, leaving only file
	// entries. This guards the assumption that lets one git call do both jobs.
	input := []byte("# branch.oid abc123\n# branch.head main\n# branch.ab +0 -0\n" +
		"1 .M N... 100644 100644 100644 abc def src/main.go\n? new.txt\n")

	files, err := ParsePorcelainV2(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %+v", len(files), files)
	}
	if files[0].Path != "src/main.go" || files[1].Path != "new.txt" {
		t.Errorf("unexpected file entries: %+v", files)
	}
}

func TestClassifyXY(t *testing.T) {
	tests := []struct {
		xy   string
		want FileStatus
	}{
		{".M", FileModified},
		{"M.", FileModified},
		{"MM", FileModified},
		{".D", FileDeleted},
		{"D.", FileDeleted},
		{"A.", FileAdded},
		{"AM", FileAdded},
		{"..", FileModified},
	}

	for _, tt := range tests {
		got := classifyXY(tt.xy)
		if got != tt.want {
			t.Errorf("classifyXY(%q) = %v, want %v", tt.xy, got, tt.want)
		}
	}
}

func TestDirtySetChanged(t *testing.T) {
	a := []DirtyFile{{Path: "a.go", Status: FileModified}}
	b := []DirtyFile{{Path: "a.go", Status: FileModified}}
	c := []DirtyFile{{Path: "a.go", Status: FileDeleted}}
	d := []DirtyFile{{Path: "a.go", Status: FileModified}, {Path: "b.go", Status: FileAdded}}

	if dirtySetChanged(a, b) {
		t.Error("identical sets should not be changed")
	}
	if !dirtySetChanged(a, c) {
		t.Error("different status should be changed")
	}
	if !dirtySetChanged(a, d) {
		t.Error("different count should be changed")
	}
	if !dirtySetChanged(nil, a) {
		t.Error("nil vs non-nil should be changed")
	}
	if dirtySetChanged(nil, nil) {
		t.Error("nil vs nil should not be changed")
	}
}
