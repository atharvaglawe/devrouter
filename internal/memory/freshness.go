package memory

import (
	"os/exec"
	"strings"
)

// GitFileHash returns the current git blob hash for a file in a repo.
// Returns "" if the file is untracked, the repo isn't a git repo, or git fails.
func GitFileHash(repoPath, filePath string) string {
	cmd := exec.Command("git", "log", "-1", "--format=%H", "--", filePath)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
