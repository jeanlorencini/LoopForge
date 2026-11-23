package gitwork

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SeedEmptyRemote publishes an empty initial commit when the remote has no refs.
// A true result tells the caller to clone the base, which may have been
// published by another checkout. Failed pushes leave the workspace untouched.
func SeedEmptyRemote(ctx context.Context, workspace, remote, defaultBranch, baseBranch string) (bool, error) {
	if UsesSSH(ctx) || IsSSHReference(remote) {
		return false, nil
	}
	if !strings.Contains(remote, "://") && !filepath.IsAbs(remote) {
		absolute, err := filepath.Abs(filepath.Join(workspace, remote))
		if err != nil {
			return false, err
		}
		remote = absolute
	}
	refs, err := Run(ctx, workspace, "ls-remote", "--refs", "--", remote)
	if err != nil {
		return false, nil
	}
	baseRef := "refs/heads/" + baseBranch
	if refs != "" {
		for line := range strings.SplitSeq(refs, "\n") {
			if _, ref, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && ref == baseRef {
				return true, nil
			}
		}
		return false, nil
	}

	seed, err := os.MkdirTemp("", "loopforge-empty-remote-")
	if err != nil {
		return false, fmt.Errorf("prepare initial commit: %w", err)
	}
	defer os.RemoveAll(seed)
	if defaultBranch = strings.TrimSpace(defaultBranch); defaultBranch == "" {
		defaultBranch = baseBranch
	}
	if _, err := Run(ctx, seed, "init", "-q", "-b", defaultBranch, "."); err != nil {
		return false, fmt.Errorf("initialize empty repository: %w", err)
	}
	EnsureIdentity(ctx, seed)
	if _, err := Run(ctx, seed, "commit", "-q", "--allow-empty", "-m", "Initialize repository"); err != nil {
		return false, fmt.Errorf("initialize empty repository: %w", err)
	}
	args := []string{"push"}
	if baseBranch != defaultBranch {
		args = append(args, "--atomic")
	}
	args = append(args, "--", remote, "HEAD:refs/heads/"+defaultBranch)
	if baseBranch != defaultBranch {
		args = append(args, "HEAD:"+baseRef)
	}
	if _, err := Run(ctx, seed, args...); err != nil {
		// Another runner may have published the base after our first check.
		if _, probeErr := Run(ctx, workspace, "ls-remote", "--exit-code", "--heads", "--", remote, baseRef); probeErr != nil {
			return false, fmt.Errorf("publish initial commit to %s: %w", baseBranch, err)
		}
	}
	return true, nil
}
