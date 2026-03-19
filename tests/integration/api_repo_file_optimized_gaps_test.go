// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !windows

package integration

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	activities_model "code.gitea.io/gitea/models/activities"
	"code.gitea.io/gitea/models/db"
	issues_model "code.gitea.io/gitea/models/issues"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	webhook_model "code.gitea.io/gitea/models/webhook"
	webhook_module "code.gitea.io/gitea/modules/webhook"
	files_service "code.gitea.io/gitea/services/repository/files"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOptimizedGapAnalysis tests the scenarios that are MOST LIKELY to break
// with the optimized push, covering the blind spots in the basic regression test.
//
// Run: make test-sqlite#TestOptimizedGapAnalysis
func TestOptimizedGapAnalysis(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

		// ============================================================
		// Gap 1: Issue auto-close from commit message ("closes #N")
		// ============================================================
		t.Run("IssueAutoClose", func(t *testing.T) {
			repoN := createTestRepoForRegression(t, user, "gap-issue-normal", 10)
			repoO := createTestRepoForRegression(t, user, "gap-issue-opt", 10)

			issueN := createTestIssue(t, repoN, user, "Normal issue")
			issueO := createTestIssue(t, repoO, user, "Optimized issue")

			// Normal: commit with "closes #N"
			_, err := files_service.ChangeRepoFiles(context.TODO(), repoN, user, &files_service.ChangeRepoFilesOptions{
				Files: []*files_service.ChangeRepoFile{
					{Operation: "create", TreePath: "fix.txt", ContentReader: strings.NewReader("fix")},
				},
				OldBranch: repoN.DefaultBranch, NewBranch: repoN.DefaultBranch,
				Message: fmt.Sprintf("closes #%d", issueN.Index),
			})
			require.NoError(t, err)

			// Optimized: same commit message
			doOptimizedCreateFile(t, repoO, user, repoO.DefaultBranch, "fix.txt", "fix",
				fmt.Sprintf("closes #%d", issueO.Index))

			time.Sleep(500 * time.Millisecond)

			issueN = reloadIssue(t, issueN.ID)
			issueO = reloadIssue(t, issueO.ID)

			t.Logf("  IssueAutoClose: normal_closed=%v optimized_closed=%v", issueN.IsClosed, issueO.IsClosed)
			assert.Equal(t, issueN.IsClosed, issueO.IsClosed,
				"issue auto-close should match")
		})

		// ============================================================
		// Gap 2: Webhook push event created
		// ============================================================
		t.Run("WebhookPushEvent", func(t *testing.T) {
			repoN := createTestRepoForRegression(t, user, "gap-wh-normal", 10)
			repoO := createTestRepoForRegression(t, user, "gap-wh-opt", 10)

			whN := installTestWebhookGetID(t, repoN)
			whO := installTestWebhookGetID(t, repoO)

			_, err := files_service.ChangeRepoFiles(context.TODO(), repoN, user, &files_service.ChangeRepoFilesOptions{
				Files: []*files_service.ChangeRepoFile{
					{Operation: "create", TreePath: "wh.txt", ContentReader: strings.NewReader("wh")},
				},
				OldBranch: repoN.DefaultBranch, NewBranch: repoN.DefaultBranch,
				Message: "webhook test",
			})
			require.NoError(t, err)

			doOptimizedCreateFile(t, repoO, user, repoO.DefaultBranch, "wh.txt", "wh", "webhook test")

			time.Sleep(500 * time.Millisecond)

			normalTasks := getWebhookTasks(t, whN)
			optTasks := getWebhookTasks(t, whO)

			normalHasPush := hasEventType(normalTasks, "push")
			optHasPush := hasEventType(optTasks, "push")

			t.Logf("  WebhookPushEvent: normal=%v(%d tasks) optimized=%v(%d tasks)",
				normalHasPush, len(normalTasks), optHasPush, len(optTasks))
			assert.Equal(t, normalHasPush, optHasPush, "webhook push events should match")
		})

		// ============================================================
		// Gap 3: Activity feed (ActionCommitRepo)
		// ============================================================
		t.Run("ActivityFeed", func(t *testing.T) {
			repoN := createTestRepoForRegression(t, user, "gap-act-normal", 10)
			repoO := createTestRepoForRegression(t, user, "gap-act-opt", 10)

			beforeN := countActionsByType(t, repoN.ID, activities_model.ActionCommitRepo)
			beforeO := countActionsByType(t, repoO.ID, activities_model.ActionCommitRepo)

			_, err := files_service.ChangeRepoFiles(context.TODO(), repoN, user, &files_service.ChangeRepoFilesOptions{
				Files: []*files_service.ChangeRepoFile{
					{Operation: "create", TreePath: "act.txt", ContentReader: strings.NewReader("act")},
				},
				OldBranch: repoN.DefaultBranch, NewBranch: repoN.DefaultBranch,
				Message: "activity test",
			})
			require.NoError(t, err)

			doOptimizedCreateFile(t, repoO, user, repoO.DefaultBranch, "act.txt", "act", "activity test")

			time.Sleep(500 * time.Millisecond)

			newN := countActionsByType(t, repoN.ID, activities_model.ActionCommitRepo) - beforeN
			newO := countActionsByType(t, repoO.ID, activities_model.ActionCommitRepo) - beforeO

			t.Logf("  ActivityFeed: normal_new=%d optimized_new=%d", newN, newO)
			assert.Equal(t, newN, newO, "ActionCommitRepo count should match")
		})

		// ============================================================
		// Gap 4: Custom pre-receive hook still executes
		// ============================================================
		t.Run("CustomPreReceiveHook", func(t *testing.T) {
			repoO := createTestRepoForRegression(t, user, "gap-hook", 10)

			bareRepoPath := repo_model.RepoPath(repoO.OwnerName, repoO.Name)
			markerFile := t.TempDir() + "/hook-marker"

			hookDir := bareRepoPath + "/hooks/pre-receive.d"
			hookPath := hookDir + "/zzz-custom-test" // zzz prefix so it runs after gitea
			hookScript := fmt.Sprintf("#!/bin/sh\ntouch '%s'\n", markerFile)
			require.NoError(t, os.WriteFile(hookPath, []byte(hookScript), 0o755))

			// Optimized push
			doOptimizedCreateFile(t, repoO, user, repoO.DefaultBranch, "hook-test.txt", "x", "custom hook test")

			_, err := os.Stat(markerFile)
			executed := err == nil

			t.Logf("  CustomPreReceiveHook: executed=%v", executed)
			assert.True(t, executed,
				"Custom pre-receive hook should still execute with InternalPushingEnvironment")
		})

		// ============================================================
		// Gap 5: Repo size update
		// ============================================================
		t.Run("RepoSizeUpdate", func(t *testing.T) {
			repoN := createTestRepoForRegression(t, user, "gap-size-normal", 10)
			repoO := createTestRepoForRegression(t, user, "gap-size-opt", 10)

			// Get size before
			repoN = unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: repoN.ID})
			repoO = unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: repoO.ID})
			sizeBeforeN := repoN.Size
			sizeBeforeO := repoO.Size

			// Create a file with substantial content
			bigContent := strings.Repeat("x", 10000)

			_, err := files_service.ChangeRepoFiles(context.TODO(), repoN, user, &files_service.ChangeRepoFilesOptions{
				Files: []*files_service.ChangeRepoFile{
					{Operation: "create", TreePath: "big.txt", ContentReader: strings.NewReader(bigContent)},
				},
				OldBranch: repoN.DefaultBranch, NewBranch: repoN.DefaultBranch,
				Message: "size test",
			})
			require.NoError(t, err)

			doOptimizedCreateFile(t, repoO, user, repoO.DefaultBranch, "big.txt", bigContent, "size test")

			time.Sleep(500 * time.Millisecond)

			repoN = unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: repoN.ID})
			repoO = unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: repoO.ID})

			normalGrew := repoN.Size > sizeBeforeN
			optGrew := repoO.Size > sizeBeforeO

			t.Logf("  RepoSizeUpdate: normal_grew=%v(%d→%d) optimized_grew=%v(%d→%d)",
				normalGrew, sizeBeforeN, repoN.Size, optGrew, sizeBeforeO, repoO.Size)
			assert.Equal(t, normalGrew, optGrew, "repo size update behavior should match")
		})

		t.Log("")
	})
}

func createTestIssue(t testing.TB, repo *repo_model.Repository, user *user_model.User, title string) *issues_model.Issue {
	t.Helper()
	issue := &issues_model.Issue{
		RepoID:   repo.ID,
		PosterID: user.ID,
		Title:    title,
		Content:  "test issue",
	}
	err := issues_model.NewIssue(context.TODO(), repo, issue, nil, nil)
	require.NoError(t, err)
	return issue
}

func reloadIssue(t testing.TB, id int64) *issues_model.Issue {
	t.Helper()
	issue, err := issues_model.GetIssueByID(context.TODO(), id)
	require.NoError(t, err)
	return issue
}

func installTestWebhookGetID(t testing.TB, repo *repo_model.Repository) int64 {
	t.Helper()
	wh := &webhook_model.Webhook{
		RepoID:      repo.ID,
		URL:         "http://127.0.0.1:12345/test-webhook-gap",
		ContentType: webhook_model.ContentTypeJSON,
		Events:      `{"push_events":true}`,
		IsActive:    true,
		Type:        webhook_module.GITEA,
	}
	_, err := db.GetEngine(context.TODO()).Insert(wh)
	require.NoError(t, err)
	return wh.ID
}

func getWebhookTasks(t testing.TB, hookID int64) []*webhook_model.HookTask {
	t.Helper()
	var tasks []*webhook_model.HookTask
	err := db.GetEngine(context.TODO()).Where("hook_id = ?", hookID).Find(&tasks)
	require.NoError(t, err)
	return tasks
}

func hasEventType(tasks []*webhook_model.HookTask, eventType webhook_module.HookEventType) bool {
	for _, task := range tasks {
		if task.EventType == eventType {
			return true
		}
	}
	return false
}

func countActionsByType(t testing.TB, repoID int64, opType activities_model.ActionType) int {
	t.Helper()
	count, err := db.GetEngine(context.TODO()).
		Where("repo_id = ? AND op_type = ?", repoID, opType).
		Count(new(activities_model.Action))
	require.NoError(t, err)
	return int(count)
}
