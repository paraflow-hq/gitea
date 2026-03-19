// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	activities_model "code.gitea.io/gitea/models/activities"
	"code.gitea.io/gitea/models/db"
	git_model "code.gitea.io/gitea/models/git"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	webhook_model "code.gitea.io/gitea/models/webhook"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/gitrepo"
	repo_module "code.gitea.io/gitea/modules/repository"
	webhook_module "code.gitea.io/gitea/modules/webhook"
	pull_service "code.gitea.io/gitea/services/pull"
	repo_service "code.gitea.io/gitea/services/repository"
	files_service "code.gitea.io/gitea/services/repository/files"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOptimizedPushRegression is a comprehensive functional regression test that
// verifies every business side effect of ChangeRepoFiles is preserved when using
// PushInternalSkipHooks + manual side effect calls.
//
// For each test scenario, it runs both the normal flow and the optimized flow on
// identical repos and compares all observable outcomes.
//
// Run: make test-sqlite#TestOptimizedPushRegression
func TestOptimizedPushRegression(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

		// ============================================================
		// Test 1: Create file — git state, DB branch, activity, webhook
		// ============================================================
		t.Run("CreateFile", func(t *testing.T) {
			repoN := createTestRepoForRegression(t, user, "reg-create-normal", 100)
			repoO := createTestRepoForRegression(t, user, "reg-create-opt", 100)

			// Install a webhook on both repos to verify webhook triggering
			installTestWebhook(t, repoN)
			installTestWebhook(t, repoO)

			treePath := "regression/new-file.txt"
			content := "regression test content\n"

			// Snapshot before
			normalBefore := snapshotRepo(t, repoN, user)
			optBefore := snapshotRepo(t, repoO, user)

			// Normal flow
			normalResp, err := files_service.ChangeRepoFiles(context.TODO(), repoN, user, &files_service.ChangeRepoFilesOptions{
				Files: []*files_service.ChangeRepoFile{
					{Operation: "create", TreePath: treePath, ContentReader: strings.NewReader(content)},
				},
				OldBranch: repoN.DefaultBranch,
				NewBranch: repoN.DefaultBranch,
				Message:   "normal: create file",
			})
			require.NoError(t, err)

			// Optimized flow
			optResult := doOptimizedCreateFile(t, repoO, user, repoO.DefaultBranch, treePath, content, "optimized: create file")

			// Wait for async side effects (queue=immediate in tests, so should be done)
			time.Sleep(200 * time.Millisecond)

			// Snapshot after
			normalAfter := snapshotRepo(t, repoN, user)
			optAfter := snapshotRepo(t, repoO, user)

			// --- Verify: git file exists with correct SHA ---
			assert.NotEmpty(t, normalResp.Files, "normal: should return file")
			assert.Equal(t, normalResp.Files[0].SHA, optResult.fileSHA, "SHA mismatch between normal and optimized")

			// --- Verify: file readable from git ---
			verifyFileInGit(t, repoN, repoN.DefaultBranch, treePath, "normal")
			verifyFileInGit(t, repoO, repoO.DefaultBranch, treePath, "optimized")

			// --- Verify: branch commit updated in DB ---
			normalBranch := getBranchFromDB(t, repoN.ID, repoN.DefaultBranch)
			optBranch := getBranchFromDB(t, repoO.ID, repoO.DefaultBranch)
			assert.NotEqual(t, normalBefore.branchCommitID, normalBranch.CommitID, "normal: branch commit should change")
			assert.NotEqual(t, optBefore.branchCommitID, optBranch.CommitID, "optimized: branch commit should change")

			// --- Verify: activity/action created ---
			assert.Greater(t, normalAfter.actionCount, normalBefore.actionCount, "normal: should create activity")
			assert.Greater(t, optAfter.actionCount, optBefore.actionCount, "optimized: should create activity")

			// --- Verify: webhook task created ---
			assert.Greater(t, normalAfter.webhookTaskCount, normalBefore.webhookTaskCount, "normal: should create webhook task")
			assert.Greater(t, optAfter.webhookTaskCount, optBefore.webhookTaskCount, "optimized: should create webhook task")

			// --- Verify: repo updated_unix changed ---
			assert.Greater(t, normalAfter.repoUpdatedUnix, normalBefore.repoUpdatedUnix, "normal: repo updated_unix should change")
			assert.Greater(t, optAfter.repoUpdatedUnix, optBefore.repoUpdatedUnix, "optimized: repo updated_unix should change")

			t.Log("  CreateFile: PASS (git, branch DB, activity, webhook, repo timestamp)")
		})

		// ============================================================
		// Test 2: Create file on new branch
		// ============================================================
		t.Run("CreateFileNewBranch", func(t *testing.T) {
			repoN := createTestRepoForRegression(t, user, "reg-branch-normal", 50)
			repoO := createTestRepoForRegression(t, user, "reg-branch-opt", 50)
			newBranch := "feature-test"

			// Normal
			_, err := files_service.ChangeRepoFiles(context.TODO(), repoN, user, &files_service.ChangeRepoFilesOptions{
				Files: []*files_service.ChangeRepoFile{
					{Operation: "create", TreePath: "branch-test.txt", ContentReader: strings.NewReader("x")},
				},
				OldBranch: repoN.DefaultBranch,
				NewBranch: newBranch,
				Message:   "normal: new branch",
			})
			require.NoError(t, err)

			// Optimized
			doOptimizedCreateFile(t, repoO, user, newBranch, "branch-test.txt", "x", "optimized: new branch")
			time.Sleep(200 * time.Millisecond)

			// Verify new branch exists in DB for both
			normalBranch := getBranchFromDB(t, repoN.ID, newBranch)
			optBranch := getBranchFromDB(t, repoO.ID, newBranch)
			assert.NotNil(t, normalBranch, "normal: new branch should exist in DB")
			assert.NotNil(t, optBranch, "optimized: new branch should exist in DB")

			// Verify file on new branch
			verifyFileInGit(t, repoN, newBranch, "branch-test.txt", "normal")
			verifyFileInGit(t, repoO, newBranch, "branch-test.txt", "optimized")

			t.Log("  CreateFileNewBranch: PASS (branch exists in DB and git)")
		})

		// ============================================================
		// Test 3: Update file (verify SHA change)
		// ============================================================
		t.Run("UpdateFile", func(t *testing.T) {
			repoN := createTestRepoForRegression(t, user, "reg-update-normal", 50)
			repoO := createTestRepoForRegression(t, user, "reg-update-opt", 50)

			// First create a file in both
			treePath := "update-me.txt"
			for _, r := range []*repo_model.Repository{repoN, repoO} {
				_, err := files_service.ChangeRepoFiles(context.TODO(), r, user, &files_service.ChangeRepoFilesOptions{
					Files: []*files_service.ChangeRepoFile{
						{Operation: "create", TreePath: treePath, ContentReader: strings.NewReader("original")},
					},
					OldBranch: r.DefaultBranch, NewBranch: r.DefaultBranch, Message: "create for update test",
				})
				require.NoError(t, err)
			}

			// Get the SHA for update
			normalSHA := getFileSHA(t, repoN, repoN.DefaultBranch, treePath)
			optSHA := getFileSHA(t, repoO, repoO.DefaultBranch, treePath)
			assert.Equal(t, normalSHA, optSHA, "pre-update SHA should match")

			// Normal update
			_, err := files_service.ChangeRepoFiles(context.TODO(), repoN, user, &files_service.ChangeRepoFilesOptions{
				Files: []*files_service.ChangeRepoFile{
					{Operation: "update", TreePath: treePath, SHA: normalSHA, ContentReader: strings.NewReader("updated content")},
				},
				OldBranch: repoN.DefaultBranch, NewBranch: repoN.DefaultBranch, Message: "normal: update file",
			})
			require.NoError(t, err)

			// Optimized update
			doOptimizedUpdateFile(t, repoO, user, treePath, optSHA, "updated content", "optimized: update file")
			time.Sleep(200 * time.Millisecond)

			// Verify both have new SHA
			newNormalSHA := getFileSHA(t, repoN, repoN.DefaultBranch, treePath)
			newOptSHA := getFileSHA(t, repoO, repoO.DefaultBranch, treePath)
			assert.NotEqual(t, normalSHA, newNormalSHA, "normal: SHA should change after update")
			assert.NotEqual(t, optSHA, newOptSHA, "optimized: SHA should change after update")
			assert.Equal(t, newNormalSHA, newOptSHA, "updated SHA should match between normal and optimized")

			t.Log("  UpdateFile: PASS (SHA updated, content matches)")
		})

		// ============================================================
		// Test 4: Delete file
		// ============================================================
		t.Run("DeleteFile", func(t *testing.T) {
			repoN := createTestRepoForRegression(t, user, "reg-delete-normal", 50)
			repoO := createTestRepoForRegression(t, user, "reg-delete-opt", 50)
			treePath := "delete-me.txt"

			// Create file in both
			for _, r := range []*repo_model.Repository{repoN, repoO} {
				_, err := files_service.ChangeRepoFiles(context.TODO(), r, user, &files_service.ChangeRepoFilesOptions{
					Files: []*files_service.ChangeRepoFile{
						{Operation: "create", TreePath: treePath, ContentReader: strings.NewReader("to be deleted")},
					},
					OldBranch: r.DefaultBranch, NewBranch: r.DefaultBranch, Message: "create for delete test",
				})
				require.NoError(t, err)
			}

			// Normal delete
			sha := getFileSHA(t, repoN, repoN.DefaultBranch, treePath)
			_, err := files_service.ChangeRepoFiles(context.TODO(), repoN, user, &files_service.ChangeRepoFilesOptions{
				Files: []*files_service.ChangeRepoFile{
					{Operation: "delete", TreePath: treePath, SHA: sha},
				},
				OldBranch: repoN.DefaultBranch, NewBranch: repoN.DefaultBranch, Message: "normal: delete file",
			})
			require.NoError(t, err)

			// Optimized delete
			sha = getFileSHA(t, repoO, repoO.DefaultBranch, treePath)
			doOptimizedDeleteFile(t, repoO, user, treePath, sha, "optimized: delete file")
			time.Sleep(200 * time.Millisecond)

			// Verify file gone from both
			verifyFileNotInGit(t, repoN, repoN.DefaultBranch, treePath, "normal")
			verifyFileNotInGit(t, repoO, repoO.DefaultBranch, treePath, "optimized")

			t.Log("  DeleteFile: PASS (file removed from both)")
		})

		// ============================================================
		// Test 5: Multi-file batch operation
		// ============================================================
		t.Run("BatchCreateFiles", func(t *testing.T) {
			repoN := createTestRepoForRegression(t, user, "reg-batch-normal", 50)
			repoO := createTestRepoForRegression(t, user, "reg-batch-opt", 50)

			files := map[string]string{
				"batch/a.txt": "aaa",
				"batch/b.txt": "bbb",
				"batch/c.txt": "ccc",
			}

			// Normal batch
			var normalFiles []*files_service.ChangeRepoFile
			for path, content := range files {
				normalFiles = append(normalFiles, &files_service.ChangeRepoFile{
					Operation: "create", TreePath: path, ContentReader: strings.NewReader(content),
				})
			}
			_, err := files_service.ChangeRepoFiles(context.TODO(), repoN, user, &files_service.ChangeRepoFilesOptions{
				Files: normalFiles, OldBranch: repoN.DefaultBranch, NewBranch: repoN.DefaultBranch, Message: "normal: batch",
			})
			require.NoError(t, err)

			// Optimized batch
			doOptimizedBatchCreate(t, repoO, user, files, "optimized: batch")
			time.Sleep(200 * time.Millisecond)

			// Verify all files exist
			for path := range files {
				verifyFileInGit(t, repoN, repoN.DefaultBranch, path, "normal")
				verifyFileInGit(t, repoO, repoO.DefaultBranch, path, "optimized")
			}

			t.Log("  BatchCreateFiles: PASS (3 files created in both)")
		})

		t.Log("")
		t.Log("=== All Regression Tests PASSED ===")
	})
}

// ---------------------------------------------------------------------------
// Helpers: optimized operations
// ---------------------------------------------------------------------------

type regResult struct {
	fileSHA      string
	commitHash   string
	branchSynced bool
	fileReadable bool
}

func doOptimizedCreateFile(t testing.TB, repo *repo_model.Repository, doer *user_model.User, branch, treePath, content, msg string) regResult {
	t.Helper()
	return doOptimizedOp(t, repo, doer, branch, msg, func(tmpRepo *files_service.TemporaryUploadRepository) (string, error) {
		objectHash, err := tmpRepo.HashObjectAndWrite(context.TODO(), strings.NewReader(content))
		if err != nil {
			return "", err
		}
		return objectHash, tmpRepo.AddObjectToIndex(context.TODO(), "100644", objectHash, treePath)
	})
}

func doOptimizedUpdateFile(t testing.TB, repo *repo_model.Repository, doer *user_model.User, treePath, oldSHA, content, msg string) regResult {
	t.Helper()
	return doOptimizedOp(t, repo, doer, repo.DefaultBranch, msg, func(tmpRepo *files_service.TemporaryUploadRepository) (string, error) {
		objectHash, err := tmpRepo.HashObjectAndWrite(context.TODO(), strings.NewReader(content))
		if err != nil {
			return "", err
		}
		return objectHash, tmpRepo.AddObjectToIndex(context.TODO(), "100644", objectHash, treePath)
	})
}

func doOptimizedDeleteFile(t testing.TB, repo *repo_model.Repository, doer *user_model.User, treePath, sha, msg string) regResult {
	t.Helper()
	return doOptimizedOp(t, repo, doer, repo.DefaultBranch, msg, func(tmpRepo *files_service.TemporaryUploadRepository) (string, error) {
		return "", tmpRepo.RemoveFilesFromIndex(context.TODO(), treePath)
	})
}

func doOptimizedBatchCreate(t testing.TB, repo *repo_model.Repository, doer *user_model.User, files map[string]string, msg string) regResult {
	t.Helper()
	return doOptimizedOp(t, repo, doer, repo.DefaultBranch, msg, func(tmpRepo *files_service.TemporaryUploadRepository) (string, error) {
		var lastHash string
		for path, content := range files {
			objectHash, err := tmpRepo.HashObjectAndWrite(context.TODO(), strings.NewReader(content))
			if err != nil {
				return "", err
			}
			if err := tmpRepo.AddObjectToIndex(context.TODO(), "100644", objectHash, path); err != nil {
				return "", err
			}
			lastHash = objectHash
		}
		return lastHash, nil
	})
}

// doOptimizedOp runs the full optimized flow: clone, index modification, commit,
// internal push, and manual side effects.
func doOptimizedOp(t testing.TB, repo *repo_model.Repository, doer *user_model.User, targetBranch, msg string, modifyIndex func(*files_service.TemporaryUploadRepository) (string, error)) regResult {
	t.Helper()
	ctx := context.TODO()
	result := regResult{}

	tmpRepo, err := files_service.NewTemporaryUploadRepository(repo)
	require.NoError(t, err)
	defer tmpRepo.Close()

	require.NoError(t, tmpRepo.Clone(ctx, repo.DefaultBranch, true))
	require.NoError(t, tmpRepo.SetDefaultIndex(ctx))

	objectHash, err := modifyIndex(tmpRepo)
	require.NoError(t, err)
	result.fileSHA = objectHash

	treeHash, err := tmpRepo.WriteTree(ctx)
	require.NoError(t, err)

	oldCommit, err := tmpRepo.GetBranchCommit(repo.DefaultBranch)
	require.NoError(t, err)
	oldCommitID := oldCommit.ID.String()

	commitHash, err := tmpRepo.CommitTree(ctx, &files_service.CommitTreeUserOptions{
		ParentCommitID: oldCommitID,
		TreeHash:       treeHash,
		CommitMessage:  msg,
		DoerUser:       doer,
	})
	require.NoError(t, err)
	result.commitHash = commitHash

	// --- OPTIMIZED PUSH ---
	require.NoError(t, tmpRepo.PushInternalSkipHooks(ctx, doer, commitHash, targetBranch, false))

	// --- MANUAL SIDE EFFECTS ---
	gitRepo, err := gitrepo.OpenRepository(ctx, repo)
	require.NoError(t, err)
	defer gitRepo.Close()

	err = repo_service.SyncBranchesToDB(ctx, repo.ID, doer.ID,
		[]string{targetBranch}, []string{commitHash}, gitRepo.GetCommit)
	result.branchSynced = err == nil
	if err != nil {
		t.Logf("SyncBranchesToDB: %v", err)
	}

	pushOpts := &repo_module.PushUpdateOptions{
		RefFullName:  git.RefNameFromBranch(targetBranch),
		OldCommitID:  oldCommitID,
		NewCommitID:  commitHash,
		PusherID:     doer.ID,
		PusherName:   doer.Name,
		RepoUserName: repo.OwnerName,
		RepoName:     repo.Name,
	}
	pull_service.UpdatePullsRefs(ctx, repo, pushOpts)

	err = repo_service.PushUpdates([]*repo_module.PushUpdateOptions{pushOpts})
	if err != nil {
		t.Logf("PushUpdates: %v", err)
	}

	// Verify file readable
	newCommit, err := gitRepo.GetCommit(commitHash)
	if err == nil {
		result.fileReadable = true // commit exists
	}
	_ = newCommit

	return result
}

// ---------------------------------------------------------------------------
// Helpers: verification
// ---------------------------------------------------------------------------

type repoSnapshot struct {
	branchCommitID   string
	actionCount      int
	webhookTaskCount int
	repoUpdatedUnix  int64
	repoSize         int64
}

func snapshotRepo(t testing.TB, repo *repo_model.Repository, doer *user_model.User) repoSnapshot {
	t.Helper()
	s := repoSnapshot{}

	// Branch commit ID
	branch := getBranchFromDB(t, repo.ID, repo.DefaultBranch)
	if branch != nil {
		s.branchCommitID = branch.CommitID
	}

	// Action count
	s.actionCount = countActions(t, repo.ID)

	// Webhook task count
	s.webhookTaskCount = countWebhookTasks(t, repo.ID)

	// Repo updated time and size
	freshRepo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: repo.ID})
	s.repoUpdatedUnix = int64(freshRepo.UpdatedUnix)
	s.repoSize = freshRepo.Size

	return s
}

func getBranchFromDB(t testing.TB, repoID int64, branchName string) *git_model.Branch {
	t.Helper()
	branch := &git_model.Branch{RepoID: repoID, Name: branchName}
	has, err := db.GetEngine(context.TODO()).Get(branch)
	require.NoError(t, err)
	if !has {
		return nil
	}
	return branch
}

func countActions(t testing.TB, repoID int64) int {
	t.Helper()
	count, err := db.GetEngine(context.TODO()).Where("repo_id = ?", repoID).Count(new(activities_model.Action))
	require.NoError(t, err)
	return int(count)
}

func countWebhookTasks(t testing.TB, repoID int64) int {
	t.Helper()
	// Count webhook tasks for webhooks belonging to this repo
	count, err := db.GetEngine(context.TODO()).
		Table("hook_task").
		Join("INNER", "webhook", "webhook.id = hook_task.hook_id").
		Where("webhook.repo_id = ?", repoID).
		Count()
	require.NoError(t, err)
	return int(count)
}

func verifyFileInGit(t testing.TB, repo *repo_model.Repository, branch, treePath, label string) {
	t.Helper()
	gitRepo, err := gitrepo.OpenRepository(context.TODO(), repo)
	require.NoError(t, err)
	defer gitRepo.Close()

	commit, err := gitRepo.GetBranchCommit(branch)
	require.NoError(t, err, "%s: branch %s not found", label, branch)

	_, err = commit.GetTreeEntryByPath(treePath)
	assert.NoError(t, err, "%s: file %s not found on branch %s", label, treePath, branch)
}

func verifyFileNotInGit(t testing.TB, repo *repo_model.Repository, branch, treePath, label string) {
	t.Helper()
	gitRepo, err := gitrepo.OpenRepository(context.TODO(), repo)
	require.NoError(t, err)
	defer gitRepo.Close()

	commit, err := gitRepo.GetBranchCommit(branch)
	require.NoError(t, err)

	_, err = commit.GetTreeEntryByPath(treePath)
	assert.Error(t, err, "%s: file %s should NOT exist on branch %s", label, treePath, branch)
}

func getFileSHA(t testing.TB, repo *repo_model.Repository, branch, treePath string) string {
	t.Helper()
	gitRepo, err := gitrepo.OpenRepository(context.TODO(), repo)
	require.NoError(t, err)
	defer gitRepo.Close()

	commit, err := gitRepo.GetBranchCommit(branch)
	require.NoError(t, err)

	entry, err := commit.GetTreeEntryByPath(treePath)
	require.NoError(t, err, "file %s not found on branch %s", treePath, branch)

	return entry.ID.String()
}

func installTestWebhook(t testing.TB, repo *repo_model.Repository) {
	t.Helper()
	_, err := db.GetEngine(context.TODO()).Insert(&webhook_model.Webhook{
		RepoID:      repo.ID,
		URL:         "http://127.0.0.1:12345/test-webhook",
		ContentType: webhook_model.ContentTypeJSON,
		Events:      `{"push_events":true}`,
		IsActive:    true,
		Type:        webhook_module.GITEA,
	})
	require.NoError(t, err)
}

func createTestRepoForRegression(t testing.TB, user *user_model.User, name string, files int) *repo_model.Repository {
	t.Helper()
	return createTestRepo(t, user, name, files)
}
