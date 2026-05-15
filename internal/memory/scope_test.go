package memory

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// checkGitAvailable skips the test if git is not available.
func checkGitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
}

// setupTestGitRepo creates a test git setup with a feature branch diverging from origin/release.
// Returns the path to the working repository.
func setupTestGitRepo(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	originDir := filepath.Join(tmpDir, "origin.git")
	workDir := filepath.Join(tmpDir, "working")

	// Initialize bare origin repo
	mustGit(t, "", "init", "--bare", originDir)

	// Initialize working repo
	mustGit(t, "", "init", workDir)
	mustGit(t, workDir, "config", "user.email", "test@test.com")
	mustGit(t, workDir, "config", "user.name", "Test User")
	mustGit(t, workDir, "remote", "add", "origin", originDir)

	// Create initial commit with stable files
	writeFile(t, workDir, "stable.go", "package main\n")
	writeFile(t, workDir, "stable2.go", "package main\n")
	mustGit(t, workDir, "add", ".")
	mustGit(t, workDir, "commit", "-m", "initial commit")

	// Push to origin as "release" branch
	mustGit(t, workDir, "push", "origin", "HEAD:release")

	// Create feature branch and add a new file
	mustGit(t, workDir, "checkout", "-b", "feature-xyz")
	writeFile(t, workDir, "new_file.go", "package main\n")
	mustGit(t, workDir, "add", ".")
	mustGit(t, workDir, "commit", "-m", "add feature file")

	return workDir
}

// setupTestGitRepoOnRelease creates a test git repo where the working branch matches origin/release exactly.
func setupTestGitRepoOnRelease(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	originDir := filepath.Join(tmpDir, "origin.git")
	workDir := filepath.Join(tmpDir, "working")

	// Initialize bare origin repo
	mustGit(t, "", "init", "--bare", originDir)

	// Initialize working repo
	mustGit(t, "", "init", workDir)
	mustGit(t, workDir, "config", "user.email", "test@test.com")
	mustGit(t, workDir, "config", "user.name", "Test User")
	mustGit(t, workDir, "remote", "add", "origin", originDir)

	// Create initial commit with stable file
	writeFile(t, workDir, "stable.go", "package main\n")
	mustGit(t, workDir, "add", ".")
	mustGit(t, workDir, "commit", "-m", "initial commit")

	// Push to origin as "release" branch (working branch matches release exactly)
	mustGit(t, workDir, "push", "origin", "HEAD:release")

	return workDir
}

// mustGit executes a git command, failing the test if it errors.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// writeFile writes content to a file in the specified directory.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	filePath := filepath.Join(dir, name)
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// TestCurrentBranch tests the CurrentBranch function.
func TestCurrentBranch(t *testing.T) {
	checkGitAvailable(t)

	repoPath := setupTestGitRepo(t)

	tests := []struct {
		name     string
		repoPath string
		want     string
	}{
		{
			name:     "feature branch",
			repoPath: repoPath,
			want:     "feature-xyz",
		},
		{
			name:     "empty repoPath",
			repoPath: "",
			want:     "global",
		},
		{
			name:     "invalid repoPath",
			repoPath: "/nonexistent/path/that/does/not/exist",
			want:     "global",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CurrentBranch(tc.repoPath)
			if got != tc.want {
				t.Errorf("CurrentBranch(%q) = %q, want %q", tc.repoPath, got, tc.want)
			}
		})
	}
}

// TestCurrentBranch_DetachedHead tests CurrentBranch when HEAD is detached.
func TestCurrentBranch_DetachedHead(t *testing.T) {
	checkGitAvailable(t)

	repoPath := setupTestGitRepo(t)

	// Get the current commit hash
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repoPath
	commitHashBytes, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	// Trim whitespace from the hash
	commitHash := string(bytes.TrimSpace(commitHashBytes))

	// Checkout the commit directly (detached HEAD)
	mustGit(t, repoPath, "checkout", commitHash)

	got := CurrentBranch(repoPath)
	if got != "global" {
		t.Errorf("CurrentBranch on detached HEAD = %q, want %q", got, "global")
	}
}

// TestScopeForFile tests the ScopeForFile function.
func TestScopeForFile(t *testing.T) {
	checkGitAvailable(t)

	repoPath := setupTestGitRepo(t)

	tests := []struct {
		name     string
		repoPath string
		filePath string
		want     string
	}{
		{
			name:     "unchanged file",
			repoPath: repoPath,
			filePath: "stable.go",
			want:     "global",
		},
		{
			name:     "new file on branch",
			repoPath: repoPath,
			filePath: "new_file.go",
			want:     "feature-xyz",
		},
		{
			name:     "empty repoPath",
			repoPath: "",
			filePath: "stable.go",
			want:     "global",
		},
		{
			name:     "empty filePath",
			repoPath: repoPath,
			filePath: "",
			want:     "global",
		},
		{
			name:     "both empty",
			repoPath: "",
			filePath: "",
			want:     "global",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScopeForFile(tc.repoPath, tc.filePath)
			if got != tc.want {
				t.Errorf("ScopeForFile(%q, %q) = %q, want %q", tc.repoPath, tc.filePath, got, tc.want)
			}
		})
	}
}

// TestScopeForFiles tests the ScopeForFiles function.
func TestScopeForFiles(t *testing.T) {
	checkGitAvailable(t)

	repoPath := setupTestGitRepo(t)

	tests := []struct {
		name     string
		repoPath string
		filesCSV string
		want     string
	}{
		{
			name:     "all stable",
			repoPath: repoPath,
			filesCSV: "stable.go,stable2.go",
			want:     "global",
		},
		{
			name:     "one branch file",
			repoPath: repoPath,
			filesCSV: "stable.go,new_file.go",
			want:     "feature-xyz",
		},
		{
			name:     "only branch file",
			repoPath: repoPath,
			filesCSV: "new_file.go",
			want:     "feature-xyz",
		},
		{
			name:     "empty filesCSV",
			repoPath: repoPath,
			filesCSV: "",
			want:     "global",
		},
		{
			name:     "whitespace in CSV",
			repoPath: repoPath,
			filesCSV: " stable.go , stable2.go ",
			want:     "global",
		},
		{
			name:     "empty repoPath",
			repoPath: "",
			filesCSV: "stable.go",
			want:     "global",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScopeForFiles(tc.repoPath, tc.filesCSV)
			if got != tc.want {
				t.Errorf("ScopeForFiles(%q, %q) = %q, want %q", tc.repoPath, tc.filesCSV, got, tc.want)
			}
		})
	}
}

// TestScopeForDecision tests the ScopeForDecision function.
func TestScopeForDecision(t *testing.T) {
	checkGitAvailable(t)

	t.Run("with stable files", func(t *testing.T) {
		repoPath := setupTestGitRepo(t)
		got := ScopeForDecision(repoPath, "stable.go,stable2.go")
		if got != "global" {
			t.Errorf("ScopeForDecision(..., stable files) = %q, want %q", got, "global")
		}
	})

	t.Run("with branch file", func(t *testing.T) {
		repoPath := setupTestGitRepo(t)
		got := ScopeForDecision(repoPath, "stable.go,new_file.go")
		if got != "feature-xyz" {
			t.Errorf("ScopeForDecision(..., mixed files) = %q, want %q", got, "feature-xyz")
		}
	})

	t.Run("no files, branch ahead of release", func(t *testing.T) {
		repoPath := setupTestGitRepo(t)
		got := ScopeForDecision(repoPath, "")
		if got != "feature-xyz" {
			t.Errorf("ScopeForDecision(..., no files, branch ahead) = %q, want %q", got, "feature-xyz")
		}
	})

	t.Run("no files, on release", func(t *testing.T) {
		repoPath := setupTestGitRepoOnRelease(t)
		got := ScopeForDecision(repoPath, "")
		if got != "global" {
			t.Errorf("ScopeForDecision(..., no files, on release) = %q, want %q", got, "global")
		}
	})
}
