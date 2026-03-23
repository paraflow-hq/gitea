// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !windows

package integration

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	activities_model "code.gitea.io/gitea/models/activities"
	"code.gitea.io/gitea/models/db"
	git_model "code.gitea.io/gitea/models/git"
	issues_model "code.gitea.io/gitea/models/issues"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/gitrepo"
	repo_module "code.gitea.io/gitea/modules/repository"
	pull_service "code.gitea.io/gitea/services/pull"
	repo_service "code.gitea.io/gitea/services/repository"
	files_service "code.gitea.io/gitea/services/repository/files"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// populateRepoFast adds N files to a bare repo using git plumbing commands directly.
// Bypasses ChangeRepoFiles to make setup O(N) instead of O(N^2).
func populateRepoFast(t testing.TB, repo *repo_model.Repository, n int) {
	t.Helper()
	bareRepoPath := repo_model.RepoPath(repo.OwnerName, repo.Name)
	headRef := repo.DefaultBranch
	indexFile := filepath.Join(t.TempDir(), "tmp-index")
	env := append(os.Environ(), "GIT_INDEX_FILE="+indexFile, "GIT_DIR="+bareRepoPath)

	gitRun := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v failed: %s", args, out)
		return strings.TrimSpace(string(out))
	}

	gitRun("read-tree", "refs/heads/"+headRef)

	tmpFilesDir := t.TempDir()
	var pathsList strings.Builder
	for i := range n {
		dir := filepath.Join(tmpFilesDir, fmt.Sprintf("dir%04d", i/100))
		_ = os.MkdirAll(dir, 0o755)
		fpath := filepath.Join(dir, fmt.Sprintf("file-%05d.txt", i))
		_ = os.WriteFile(fpath, fmt.Appendf(nil, "content-%d\n", i), 0o644)
		pathsList.WriteString(fpath)
		pathsList.WriteByte('\n')
	}

	cmd := exec.Command("git", "hash-object", "-w", "--stdin-paths")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(pathsList.String())
	hashOut, err := cmd.Output()
	require.NoError(t, err, "hash-object batch failed")
	hashes := strings.Split(strings.TrimSpace(string(hashOut)), "\n")
	require.Len(t, hashes, n)

	var indexInfo strings.Builder
	for i := range n {
		treePath := fmt.Sprintf("dir%04d/file-%05d.txt", i/100, i)
		fmt.Fprintf(&indexInfo, "100644 %s\t%s\000", hashes[i], treePath)
	}

	cmd = exec.Command("git", "update-index", "--add", "-z", "--index-info")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(indexInfo.String())
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "update-index failed: %s", out)

	newTree := gitRun("write-tree")
	headCommit := gitRun("rev-parse", "refs/heads/"+headRef)

	cmd = exec.Command("git", "commit-tree", newTree, "-p", headCommit, "-m", fmt.Sprintf("populate %d files", n))
	commitEnv := make([]string, len(env), len(env)+4)
	copy(commitEnv, env)
	commitEnv = append(commitEnv,
		"GIT_AUTHOR_NAME=bench", "GIT_AUTHOR_EMAIL=bench@test.local",
		"GIT_COMMITTER_NAME=bench", "GIT_COMMITTER_EMAIL=bench@test.local",
	)
	cmd.Env = commitEnv
	commitOut, err := cmd.Output()
	require.NoError(t, err, "commit-tree failed")
	newCommit := strings.TrimSpace(string(commitOut))
	gitRun("update-ref", "refs/heads/"+headRef, newCommit)
}

func createTestRepo(t testing.TB, user *user_model.User, repoName string, n int) *repo_model.Repository {
	t.Helper()
	repo, err := repo_service.CreateRepository(context.TODO(), user, user, repo_service.CreateRepoOptions{
		Name:     repoName,
		AutoInit: true,
		Readme:   "Default",
	})
	require.NoError(t, err)
	if n > 0 {
		populateRepoFast(t, repo, n)
	}
	return unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: repo.ID})
}

// ---------------------------------------------------------------------------
// Benchmark: ChangeRepoFiles latency vs repo file count
// ---------------------------------------------------------------------------

// BenchmarkCreateFileByRepoSize measures how single-file create latency
// scales with the number of existing files in the repository.
func BenchmarkCreateFileByRepoSize(b *testing.B) {
	fileCounts := []int{100, 1000, 5000, 10000}

	onGiteaRun(b, func(b *testing.B, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(b, &user_model.User{ID: 2})

		for _, n := range fileCounts {
			b.Run(fmt.Sprintf("files=%d", n), func(b *testing.B) {
				repo := createTestRepo(b, user, fmt.Sprintf("bench-size-%d", n), n)
				b.ResetTimer()
				for i := 0; b.Loop(); i++ {
					treePath := fmt.Sprintf("bench/new-file-%d.txt", i)
					_, err := createFile(user, repo, treePath)
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	})
}

// ---------------------------------------------------------------------------
// Test: Optimized vs Normal — latency + correctness
// ---------------------------------------------------------------------------

// TestOptimizedVsNormalCreateFile compares PushInternalSkipHooks + manual
// side effects against the normal ChangeRepoFiles flow. Verifies functional
// equivalence (SHA, branch DB sync, file readability) and measures latency.
func TestOptimizedVsNormalCreateFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow benchmark test")
	}
	fileCounts := []int{10, 100, 500, 2000, 5000}

	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

		t.Logf("%-8s  %14s  %14s  %14s  %10s",
			"Files", "Normal", "Optimized", "Speedup", "Correct?")

		for _, n := range fileCounts {
			repoNormal := createTestRepo(t, user, fmt.Sprintf("opt-normal-%d", n), n)
			repoOpt := createTestRepo(t, user, fmt.Sprintf("opt-fast-%d", n), n)
			treePath := fmt.Sprintf("opt-test/file-%d.txt", n)
			content := "optimized-test-content\n"

			// Normal flow
			normalStart := time.Now()
			normalResp, err := files_service.ChangeRepoFiles(context.TODO(), repoNormal, user, &files_service.ChangeRepoFilesOptions{
				Files:     []*files_service.ChangeRepoFile{{Operation: "create", TreePath: treePath, ContentReader: strings.NewReader(content)}},
				OldBranch: repoNormal.DefaultBranch, NewBranch: repoNormal.DefaultBranch,
				Message: "normal: add " + treePath,
			})
			require.NoError(t, err)
			normalDur := time.Since(normalStart)

			// Optimized flow
			optResp := runOptimizedCreate(t, repoOpt, user, repoOpt.DefaultBranch, treePath, content, "optimized: add "+treePath)

			// Verify
			correct := true
			if len(normalResp.Files) == 0 || normalResp.Files[0].SHA != optResp.fileSHA {
				t.Errorf("files=%d: SHA mismatch", n)
				correct = false
			}
			if !optResp.branchSynced {
				t.Errorf("files=%d: branch not synced to DB", n)
				correct = false
			}
			if !optResp.fileReadable {
				t.Errorf("files=%d: file not readable", n)
				correct = false
			}

			tag := "YES"
			if !correct {
				tag = "NO"
			}
			t.Logf("%-8d  %12dms  %12dms  %12.1fx  %10s",
				n, normalDur.Milliseconds(), optResp.syncDur.Milliseconds(),
				float64(normalDur)/float64(optResp.syncDur), tag)
		}
	})
}

// ---------------------------------------------------------------------------
// Test: Functional regression — all operations + side effects
// ---------------------------------------------------------------------------

// TestOptimizedPushRegression verifies create, update, delete, batch, new-branch
// operations and business side effects (activity, issue auto-close, custom hooks,
// repo size) are identical between normal and optimized flows.
func TestOptimizedPushRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow regression test")
	}
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

		t.Run("CreateFile", func(t *testing.T) {
			repoN := createTestRepo(t, user, "reg-create-n", 50)
			repoO := createTestRepo(t, user, "reg-create-o", 50)
			beforeN := countActions(t, repoN.ID)
			beforeO := countActions(t, repoO.ID)

			_, err := files_service.ChangeRepoFiles(context.TODO(), repoN, user, &files_service.ChangeRepoFilesOptions{
				Files:     []*files_service.ChangeRepoFile{{Operation: "create", TreePath: "reg/f.txt", ContentReader: strings.NewReader("x")}},
				OldBranch: repoN.DefaultBranch, NewBranch: repoN.DefaultBranch, Message: "normal",
			})
			require.NoError(t, err)
			runOptimizedCreate(t, repoO, user, repoO.DefaultBranch, "reg/f.txt", "x", "optimized")
			time.Sleep(200 * time.Millisecond)

			verifyFileInGit(t, repoN, repoN.DefaultBranch, "reg/f.txt")
			verifyFileInGit(t, repoO, repoO.DefaultBranch, "reg/f.txt")
			assert.Greater(t, countActions(t, repoN.ID), beforeN, "normal: should create activity")
			assert.Greater(t, countActions(t, repoO.ID), beforeO, "optimized: should create activity")
		})

		t.Run("CreateFileNewBranch", func(t *testing.T) {
			repoN := createTestRepo(t, user, "reg-branch-n", 50)
			repoO := createTestRepo(t, user, "reg-branch-o", 50)

			_, err := files_service.ChangeRepoFiles(context.TODO(), repoN, user, &files_service.ChangeRepoFilesOptions{
				Files:     []*files_service.ChangeRepoFile{{Operation: "create", TreePath: "b.txt", ContentReader: strings.NewReader("x")}},
				OldBranch: repoN.DefaultBranch, NewBranch: "feat", Message: "normal",
			})
			require.NoError(t, err)
			runOptimizedCreate(t, repoO, user, "feat", "b.txt", "x", "optimized")
			time.Sleep(200 * time.Millisecond)

			assert.NotNil(t, getBranch(t, repoN.ID, "feat"), "normal: branch should exist")
			assert.NotNil(t, getBranch(t, repoO.ID, "feat"), "optimized: branch should exist")
			verifyFileInGit(t, repoN, "feat", "b.txt")
			verifyFileInGit(t, repoO, "feat", "b.txt")
		})

		t.Run("UpdateFile", func(t *testing.T) {
			repoN := createTestRepo(t, user, "reg-update-n", 50)
			repoO := createTestRepo(t, user, "reg-update-o", 50)
			for _, r := range []*repo_model.Repository{repoN, repoO} {
				_, err := files_service.ChangeRepoFiles(context.TODO(), r, user, &files_service.ChangeRepoFilesOptions{
					Files:     []*files_service.ChangeRepoFile{{Operation: "create", TreePath: "u.txt", ContentReader: strings.NewReader("old")}},
					OldBranch: r.DefaultBranch, NewBranch: r.DefaultBranch, Message: "setup",
				})
				require.NoError(t, err)
			}
			sha := getFileSHA(t, repoN, repoN.DefaultBranch, "u.txt")
			_, err := files_service.ChangeRepoFiles(context.TODO(), repoN, user, &files_service.ChangeRepoFilesOptions{
				Files:     []*files_service.ChangeRepoFile{{Operation: "update", TreePath: "u.txt", SHA: sha, ContentReader: strings.NewReader("new")}},
				OldBranch: repoN.DefaultBranch, NewBranch: repoN.DefaultBranch, Message: "update",
			})
			require.NoError(t, err)
			runOptimizedUpdate(t, repoO, user, "u.txt", "new", "update")
			newN := getFileSHA(t, repoN, repoN.DefaultBranch, "u.txt")
			newO := getFileSHA(t, repoO, repoO.DefaultBranch, "u.txt")
			assert.NotEqual(t, sha, newN)
			assert.Equal(t, newN, newO, "updated SHA should match")
		})

		t.Run("DeleteFile", func(t *testing.T) {
			repoN := createTestRepo(t, user, "reg-delete-n", 50)
			repoO := createTestRepo(t, user, "reg-delete-o", 50)
			for _, r := range []*repo_model.Repository{repoN, repoO} {
				_, err := files_service.ChangeRepoFiles(context.TODO(), r, user, &files_service.ChangeRepoFilesOptions{
					Files:     []*files_service.ChangeRepoFile{{Operation: "create", TreePath: "d.txt", ContentReader: strings.NewReader("del")}},
					OldBranch: r.DefaultBranch, NewBranch: r.DefaultBranch, Message: "setup",
				})
				require.NoError(t, err)
			}
			sha := getFileSHA(t, repoN, repoN.DefaultBranch, "d.txt")
			_, err := files_service.ChangeRepoFiles(context.TODO(), repoN, user, &files_service.ChangeRepoFilesOptions{
				Files:     []*files_service.ChangeRepoFile{{Operation: "delete", TreePath: "d.txt", SHA: sha}},
				OldBranch: repoN.DefaultBranch, NewBranch: repoN.DefaultBranch, Message: "delete",
			})
			require.NoError(t, err)
			runOptimizedDelete(t, repoO, user, "d.txt", "delete")
			verifyFileNotInGit(t, repoN, repoN.DefaultBranch, "d.txt")
			verifyFileNotInGit(t, repoO, repoO.DefaultBranch, "d.txt")
		})

		t.Run("IssueAutoClose", func(t *testing.T) {
			repoN := createTestRepo(t, user, "reg-issue-n", 10)
			repoO := createTestRepo(t, user, "reg-issue-o", 10)
			issueN := createIssue(t, repoN, user)
			issueO := createIssue(t, repoO, user)

			_, err := files_service.ChangeRepoFiles(context.TODO(), repoN, user, &files_service.ChangeRepoFilesOptions{
				Files:     []*files_service.ChangeRepoFile{{Operation: "create", TreePath: "fix.txt", ContentReader: strings.NewReader("fix")}},
				OldBranch: repoN.DefaultBranch, NewBranch: repoN.DefaultBranch,
				Message: fmt.Sprintf("closes #%d", issueN.Index),
			})
			require.NoError(t, err)
			runOptimizedCreate(t, repoO, user, repoO.DefaultBranch, "fix.txt", "fix", fmt.Sprintf("closes #%d", issueO.Index))
			time.Sleep(500 * time.Millisecond)

			issueN, _ = issues_model.GetIssueByID(context.TODO(), issueN.ID)
			issueO, _ = issues_model.GetIssueByID(context.TODO(), issueO.ID)
			assert.Equal(t, issueN.IsClosed, issueO.IsClosed, "issue auto-close should match")
		})

		t.Run("CustomPreReceiveHook", func(t *testing.T) {
			repoO := createTestRepo(t, user, "reg-hook", 10)
			marker := t.TempDir() + "/hook-marker"
			hookPath := repo_model.RepoPath(repoO.OwnerName, repoO.Name) + "/hooks/pre-receive.d/zzz-custom"
			require.NoError(t, os.WriteFile(hookPath, fmt.Appendf(nil, "#!/bin/sh\ntouch '%s'\n", marker), 0o755))
			runOptimizedCreate(t, repoO, user, repoO.DefaultBranch, "hook.txt", "x", "hook test")
			_, err := os.Stat(marker)
			assert.NoError(t, err, "custom pre-receive hook should still execute")
		})

		t.Run("RepoSizeUpdate", func(t *testing.T) {
			repoO := createTestRepo(t, user, "reg-size", 10)
			before := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: repoO.ID}).Size
			runOptimizedCreate(t, repoO, user, repoO.DefaultBranch, "big.txt", strings.Repeat("x", 10000), "size test")
			time.Sleep(200 * time.Millisecond)
			after := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: repoO.ID}).Size
			assert.Greater(t, after, before, "repo size should increase")
		})
	})
}

// ---------------------------------------------------------------------------
// Optimized flow implementation (used by tests above)
// ---------------------------------------------------------------------------

type optResult struct {
	fileSHA      string
	branchSynced bool
	fileReadable bool
	syncDur      time.Duration
}

func runOptimizedCreate(t testing.TB, repo *repo_model.Repository, doer *user_model.User, branch, treePath, content, msg string) optResult {
	t.Helper()
	return runOptimizedOp(t, repo, doer, branch, msg, func(tmp *files_service.TemporaryUploadRepository) (string, error) {
		h, err := tmp.HashObjectAndWrite(context.TODO(), strings.NewReader(content))
		if err != nil {
			return "", err
		}
		return h, tmp.AddObjectToIndex(context.TODO(), "100644", h, treePath)
	})
}

func runOptimizedUpdate(t testing.TB, repo *repo_model.Repository, doer *user_model.User, treePath, content, msg string) optResult {
	t.Helper()
	return runOptimizedOp(t, repo, doer, repo.DefaultBranch, msg, func(tmp *files_service.TemporaryUploadRepository) (string, error) {
		h, err := tmp.HashObjectAndWrite(context.TODO(), strings.NewReader(content))
		if err != nil {
			return "", err
		}
		return h, tmp.AddObjectToIndex(context.TODO(), "100644", h, treePath)
	})
}

func runOptimizedDelete(t testing.TB, repo *repo_model.Repository, doer *user_model.User, treePath, msg string) optResult {
	t.Helper()
	return runOptimizedOp(t, repo, doer, repo.DefaultBranch, msg, func(tmp *files_service.TemporaryUploadRepository) (string, error) {
		return "", tmp.RemoveFilesFromIndex(context.TODO(), treePath)
	})
}

func runOptimizedOp(t testing.TB, repo *repo_model.Repository, doer *user_model.User, targetBranch, msg string, modifyIndex func(*files_service.TemporaryUploadRepository) (string, error)) optResult {
	t.Helper()
	ctx := context.TODO()
	var result optResult
	start := time.Now()

	tmp, err := files_service.NewTemporaryUploadRepository(repo)
	require.NoError(t, err)
	defer tmp.Close()

	require.NoError(t, tmp.Clone(ctx, repo.DefaultBranch, true))
	require.NoError(t, tmp.SetDefaultIndex(ctx))
	blobSHA, err := modifyIndex(tmp)
	require.NoError(t, err)
	result.fileSHA = blobSHA

	treeHash, err := tmp.WriteTree(ctx)
	require.NoError(t, err)

	oldCommit, err := tmp.GetBranchCommit(repo.DefaultBranch)
	require.NoError(t, err)
	oldCommitID := oldCommit.ID.String()

	commitHash, err := tmp.CommitTree(ctx, &files_service.CommitTreeUserOptions{
		ParentCommitID: oldCommitID, TreeHash: treeHash,
		CommitMessage: msg, DoerUser: doer,
	})
	require.NoError(t, err)

	// Internal push (skip hooks)
	require.NoError(t, tmp.PushInternalSkipHooks(ctx, doer, commitHash, targetBranch, false))

	// Manual side effects
	gitRepo, err := gitrepo.OpenRepository(ctx, repo)
	require.NoError(t, err)
	defer gitRepo.Close()

	err = repo_service.SyncBranchesToDB(ctx, repo.ID, doer.ID,
		[]string{targetBranch}, []string{commitHash}, gitRepo.GetCommit)
	result.branchSynced = err == nil

	pushOpts := &repo_module.PushUpdateOptions{
		RefFullName: git.RefNameFromBranch(targetBranch),
		OldCommitID: oldCommitID, NewCommitID: commitHash,
		PusherID: doer.ID, PusherName: doer.Name,
		RepoUserName: repo.OwnerName, RepoName: repo.Name,
	}
	pull_service.UpdatePullsRefs(ctx, repo, pushOpts)

	// PushUpdates async (production-like)
	done := make(chan error, 1)
	go func() { done <- repo_service.PushUpdates([]*repo_module.PushUpdateOptions{pushOpts}) }()

	result.syncDur = time.Since(start)

	// Verify file readable
	if c, err := gitRepo.GetCommit(commitHash); err == nil {
		result.fileReadable = c != nil
	}

	<-done // wait for correctness
	return result
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func verifyFileInGit(t testing.TB, repo *repo_model.Repository, branch, treePath string) {
	t.Helper()
	gitRepo, err := gitrepo.OpenRepository(context.TODO(), repo)
	require.NoError(t, err)
	defer gitRepo.Close()
	commit, err := gitRepo.GetBranchCommit(branch)
	require.NoError(t, err)
	_, err = commit.GetTreeEntryByPath(treePath)
	assert.NoError(t, err, "file %s not found on branch %s", treePath, branch)
}

func verifyFileNotInGit(t testing.TB, repo *repo_model.Repository, branch, treePath string) {
	t.Helper()
	gitRepo, err := gitrepo.OpenRepository(context.TODO(), repo)
	require.NoError(t, err)
	defer gitRepo.Close()
	commit, err := gitRepo.GetBranchCommit(branch)
	require.NoError(t, err)
	_, err = commit.GetTreeEntryByPath(treePath)
	assert.Error(t, err, "file %s should NOT exist on branch %s", treePath, branch)
}

func getFileSHA(t testing.TB, repo *repo_model.Repository, branch, treePath string) string {
	t.Helper()
	gitRepo, err := gitrepo.OpenRepository(context.TODO(), repo)
	require.NoError(t, err)
	defer gitRepo.Close()
	commit, err := gitRepo.GetBranchCommit(branch)
	require.NoError(t, err)
	entry, err := commit.GetTreeEntryByPath(treePath)
	require.NoError(t, err)
	return entry.ID.String()
}

func getBranch(t testing.TB, repoID int64, name string) *git_model.Branch {
	t.Helper()
	b := &git_model.Branch{RepoID: repoID, Name: name}
	has, err := db.GetEngine(context.TODO()).Get(b)
	require.NoError(t, err)
	if !has {
		return nil
	}
	return b
}

func countActions(t testing.TB, repoID int64) int {
	t.Helper()
	n, err := db.GetEngine(context.TODO()).Where("repo_id = ?", repoID).Count(new(activities_model.Action))
	require.NoError(t, err)
	return int(n)
}

func createIssue(t testing.TB, repo *repo_model.Repository, user *user_model.User) *issues_model.Issue {
	t.Helper()
	issue := &issues_model.Issue{RepoID: repo.ID, PosterID: user.ID, Title: "test", Content: "test"}
	require.NoError(t, issues_model.NewIssue(context.TODO(), repo, issue, nil, nil))
	return issue
}
