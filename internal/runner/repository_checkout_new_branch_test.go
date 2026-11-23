package runner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func TestCheckoutInvocationRepositoriesSeedsEmptyRemote(t *testing.T) {
	for _, base := range []string{"main", "develop"} {
		t.Run(base, func(t *testing.T) {
			remote := filepath.Join(t.TempDir(), "empty.git")
			runGitT(t, "", "init", "--bare", "-q", "-b", "main", remote)
			workspace := t.TempDir()
			repo := domain.RunnerRepository{CloneURL: remote, Ref: base, WorkBranch: "feature/first", Dir: "app"}
			if base != "main" {
				repo.DefaultBranch = "main"
			}
			inv := &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{repo}}
			if err := checkoutInvocationRepositories(context.Background(), workspace, inv); err != nil {
				t.Fatalf("checkout of a new repository: %v", err)
			}
			checkout := filepath.Join(workspace, repo.Dir)
			if got := runGitT(t, checkout, "branch", "--show-current"); got != repo.WorkBranch {
				t.Fatalf("checked out %q, want %q", got, repo.WorkBranch)
			}
			head := runGitT(t, checkout, "rev-parse", "HEAD")
			for _, ref := range []string{"refs/heads/main", "refs/heads/" + base} {
				if got := runGitT(t, remote, "rev-parse", ref); got != head {
					t.Fatalf("%s is %q, want %q", ref, got, head)
				}
			}
			if files := runGitT(t, checkout, "ls-tree", "-r", "--name-only", "HEAD"); files != "" {
				t.Fatalf("the initial commit must be empty, found %q", files)
			}
		})
	}
}

func TestCloneRepositoryCreatesOnlyNewWorkBranch(t *testing.T) {
	remote := bareRemoteWithBaseline(t)
	repo := &domain.RunnerRepository{RepositoryID: "repo", CloneURL: remote, Ref: "master", WorkBranch: "feature/caller", CreateNewWorkBranch: true}
	workspace := t.TempDir()
	if err := cloneRepository(context.Background(), workspace, repo, true); err != nil {
		t.Fatalf("create new work branch: %v", err)
	}
	if branch := runGitT(t, workspace, "branch", "--show-current"); branch != "feature/caller" {
		t.Fatalf("work branch = %q", branch)
	}
	// Publish it, then ask for the same name as a new branch: the runner must
	// refuse instead of attaching to the branch it just pushed.
	runGitT(t, workspace, "push", "-q", "origin", "feature/caller")
	taken := &domain.RunnerRepository{RepositoryID: "repo", CloneURL: remote, Ref: "master", WorkBranch: "feature/caller", CreateNewWorkBranch: true}
	if err := cloneRepository(context.Background(), t.TempDir(), taken, true); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("existing remote branch accepted as new: %v", err)
	}
}
