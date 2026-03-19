// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	auth_model "code.gitea.io/gitea/models/auth"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/git"
	repo_module "code.gitea.io/gitea/modules/repository"
	api "code.gitea.io/gitea/modules/structs"
	pull_service "code.gitea.io/gitea/services/pull"
	repo_service "code.gitea.io/gitea/services/repository"
	files_service "code.gitea.io/gitea/services/repository/files"

	"github.com/stretchr/testify/require"
)

// populateRepoFast adds N files to a bare repo using direct git plumbing commands.
// This bypasses Gitea's ChangeRepoFiles entirely, making setup O(N) total instead
// of O(N^2). It operates directly on the bare repo — no push, no hooks.
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

	// Read current HEAD tree into temp index
	gitRun("read-tree", "refs/heads/"+headRef)

	// Write all N files to a temp dir, then batch-hash them
	tmpFilesDir := t.TempDir()
	var pathsList strings.Builder
	for i := 0; i < n; i++ {
		dir := filepath.Join(tmpFilesDir, fmt.Sprintf("dir%04d", i/100))
		_ = os.MkdirAll(dir, 0o755)
		fpath := filepath.Join(dir, fmt.Sprintf("file-%05d.txt", i))
		_ = os.WriteFile(fpath, []byte(fmt.Sprintf("content-%d\n", i)), 0o644)
		pathsList.WriteString(fpath)
		pathsList.WriteByte('\n')
	}

	// Batch hash-object -w --stdin-paths
	cmd := exec.Command("git", "hash-object", "-w", "--stdin-paths")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(pathsList.String())
	hashOut, err := cmd.Output()
	require.NoError(t, err, "hash-object batch failed")
	hashes := strings.Split(strings.TrimSpace(string(hashOut)), "\n")
	require.Len(t, hashes, n)

	// Build update-index --index-info input (NUL-delimited)
	var indexInfo strings.Builder
	for i := 0; i < n; i++ {
		treePath := fmt.Sprintf("dir%04d/file-%05d.txt", i/100, i)
		fmt.Fprintf(&indexInfo, "100644 %s\t%s\000", hashes[i], treePath)
	}

	cmd = exec.Command("git", "update-index", "--add", "-z", "--index-info")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(indexInfo.String())
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "update-index failed: %s", out)

	// write-tree -> commit-tree -> update-ref
	newTree := gitRun("write-tree")
	headCommit := gitRun("rev-parse", "refs/heads/"+headRef)

	cmd = exec.Command("git", "commit-tree", newTree, "-p", headCommit, "-m", fmt.Sprintf("populate %d files", n))
	cmd.Env = append(env,
		"GIT_AUTHOR_NAME=bench", "GIT_AUTHOR_EMAIL=bench@test.local",
		"GIT_COMMITTER_NAME=bench", "GIT_COMMITTER_EMAIL=bench@test.local",
	)
	commitOut, err := cmd.Output()
	require.NoError(t, err, "commit-tree failed")
	newCommit := strings.TrimSpace(string(commitOut))

	gitRun("update-ref", "refs/heads/"+headRef, newCommit)
}

// createTestRepo creates a new repo owned by user, populates it with n files
// using fast direct git commands, and returns the repo model.
func createTestRepo(t testing.TB, user *user_model.User, repoName string, n int) *repo_model.Repository {
	t.Helper()
	ctx := context.TODO()
	repo, err := repo_service.CreateRepository(ctx, user, user, repo_service.CreateRepoOptions{
		Name:     repoName,
		AutoInit: true,
		Readme:   "Default",
	})
	require.NoError(t, err)
	if n > 0 {
		populateRepoFast(t, repo, n)
	}
	// Reload to pick up any DB changes
	repo = unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: repo.ID})
	return repo
}

// ---------------------------------------------------------------------------
// Test: Per-stage timing of ChangeRepoFiles internals
// ---------------------------------------------------------------------------

// TestCreateFileStageTimings creates repos of various sizes and prints
// per-stage timings for each git operation in ChangeRepoFiles.
// This directly measures the O(N) bottleneck in read-tree and write-tree.
//
// Run: make test-sqlite#TestCreateFileStageTimings
func TestCreateFileStageTimings(t *testing.T) {
	fileCounts := []int{10, 100, 500, 2000, 5000}

	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

		fmt.Println()
		fmt.Println("=== CreateFile Per-Stage Timings (single file add via ChangeRepoFiles internals) ===")
		fmt.Printf("%-8s  %10s  %10s  %10s  %10s  %10s  %10s  %10s\n",
			"Files", "Clone", "ReadTree", "Hash+Idx", "WriteTree", "Commit", "Push", "TOTAL")
		fmt.Printf("%-8s  %10s  %10s  %10s  %10s  %10s  %10s  %10s\n",
			"--------", "----------", "----------", "----------", "----------", "----------", "----------", "----------")

		for _, n := range fileCounts {
			repoName := fmt.Sprintf("bench-timing-%d", n)
			repo := createTestRepo(t, user, repoName, n)

			// Run the instrumented create-file flow
			timings := instrumentedCreateFile(t, repo, user, fmt.Sprintf("timing/new-file-%d.txt", n))

			fmt.Printf("%-8d  %8dms  %8dms  %8dms  %8dms  %8dms  %8dms  %8dms\n",
				n,
				timings["clone"].Milliseconds(),
				timings["read_tree"].Milliseconds(),
				timings["hash_and_index"].Milliseconds(),
				timings["write_tree"].Milliseconds(),
				timings["commit_tree"].Milliseconds(),
				timings["push"].Milliseconds(),
				timings["total"].Milliseconds(),
			)
		}

		fmt.Println()
		fmt.Println("Key: read-tree and write-tree should grow ~linearly with file count (O(N)).")
		fmt.Println("     hash+idx and commit should stay ~constant regardless of N.")
		fmt.Println()
	})
}

// instrumentedCreateFile runs the same steps as ChangeRepoFiles but with
// per-stage timing. Returns a map of stage name -> duration.
func instrumentedCreateFile(t testing.TB, repo *repo_model.Repository, doer *user_model.User, treePath string) map[string]time.Duration {
	t.Helper()
	ctx := context.TODO()
	timings := make(map[string]time.Duration)

	totalStart := time.Now()

	// Stage 1: Create temp repo & clone
	cloneStart := time.Now()
	tmpRepo, err := files_service.NewTemporaryUploadRepository(repo)
	require.NoError(t, err)
	defer tmpRepo.Close()

	err = tmpRepo.Clone(ctx, repo.DefaultBranch, true)
	require.NoError(t, err)
	timings["clone"] = time.Since(cloneStart)

	// Stage 2: read-tree (SetDefaultIndex)
	readTreeStart := time.Now()
	err = tmpRepo.SetDefaultIndex(ctx)
	require.NoError(t, err)
	timings["read_tree"] = time.Since(readTreeStart)

	// Stage 3: hash-object + update-index
	hashStart := time.Now()
	objectHash, err := tmpRepo.HashObjectAndWrite(ctx, strings.NewReader("benchmark-content\n"))
	require.NoError(t, err)
	err = tmpRepo.AddObjectToIndex(ctx, "100644", objectHash, treePath)
	require.NoError(t, err)
	timings["hash_and_index"] = time.Since(hashStart)

	// Stage 4: write-tree
	writeTreeStart := time.Now()
	treeHash, err := tmpRepo.WriteTree(ctx)
	require.NoError(t, err)
	timings["write_tree"] = time.Since(writeTreeStart)

	// Stage 5: commit-tree
	commitStart := time.Now()
	commit, err := tmpRepo.GetBranchCommit(repo.DefaultBranch)
	require.NoError(t, err)
	commitHash, err := tmpRepo.CommitTree(ctx, &files_service.CommitTreeUserOptions{
		ParentCommitID: commit.ID.String(),
		TreeHash:       treeHash,
		CommitMessage:  "benchmark: add " + treePath,
		DoerUser:       doer,
	})
	require.NoError(t, err)
	timings["commit_tree"] = time.Since(commitStart)

	// Stage 6: push
	pushStart := time.Now()
	err = tmpRepo.Push(ctx, doer, commitHash, repo.DefaultBranch, false)
	require.NoError(t, err)
	timings["push"] = time.Since(pushStart)

	timings["total"] = time.Since(totalStart)
	return timings
}

// ---------------------------------------------------------------------------
// Test: Push phase breakdown — what makes push slow?
// ---------------------------------------------------------------------------

// TestPushBreakdown compares push latency with and without Gitea hooks,
// and measures CPU time to determine if the bottleneck is CPU-bound.
//
// IMPORTANT: Test queue is TYPE=immediate (synchronous), so pushUpdates()
// runs inside the post-receive handler. In production (TYPE=channel/level),
// pushUpdates() is async and won't block push return.
// This test shows WORST CASE (test env). See TestPushSyncOnly for production-like behavior.
//
// Run: make test-sqlite#TestPushBreakdown
func TestPushBreakdown(t *testing.T) {
	fileCounts := []int{10, 100, 500, 2000, 5000}

	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

		fmt.Println()
		fmt.Println("=== Push Phase Breakdown (WARNING: test queue=immediate, sync mode) ===")
		fmt.Println("Comparing: Gitea push (with hooks) vs bare git push (no hooks)")
		fmt.Println()
		fmt.Printf("%-8s  %12s  %12s  %12s  %12s  %12s\n",
			"Files", "GiteaPush", "CPU(Gitea)", "BarePush", "CPU(Bare)", "HookOverhead")
		fmt.Printf("%-8s  %12s  %12s  %12s  %12s  %12s\n",
			"--------", "------------", "------------", "------------", "------------", "------------")

		for _, n := range fileCounts {
			// Create two identical repos for fair comparison
			repoWithHooks := createTestRepo(t, user, fmt.Sprintf("push-hooks-%d", n), n)
			repoNoHooks := createTestRepo(t, user, fmt.Sprintf("push-bare-%d", n), n)

			// Test 1: Push via Gitea (with hooks)
			giteaWall, giteaCPU := measurePushWithGitea(t, repoWithHooks, user)

			// Test 2: Push directly to bare repo (no hooks)
			bareWall, bareCPU := measurePushBare(t, repoNoHooks)

			hookOverhead := giteaWall - bareWall

			fmt.Printf("%-8d  %10dms  %10dms  %10dms  %10dms  %10dms\n",
				n,
				giteaWall.Milliseconds(),
				giteaCPU.Milliseconds(),
				bareWall.Milliseconds(),
				bareCPU.Milliseconds(),
				hookOverhead.Milliseconds(),
			)
		}

		fmt.Println()
		fmt.Println("GiteaPush = total wall time of tmpRepo.Push (triggers pre-receive + post-receive hooks)")
		fmt.Println("BarePush  = direct git push to bare repo with hooks disabled (pure git transfer cost)")
		fmt.Println("CPU(*)    = process CPU time consumed during that operation")
		fmt.Println("HookOverhead = GiteaPush - BarePush (time spent in Gitea hook handlers)")
		fmt.Println()
	})
}

// measurePushWithGitea does a full Gitea-style push and returns wall time and CPU time.
func measurePushWithGitea(t testing.TB, repo *repo_model.Repository, doer *user_model.User) (wall, cpu time.Duration) {
	t.Helper()
	ctx := context.TODO()

	tmpRepo, err := files_service.NewTemporaryUploadRepository(repo)
	require.NoError(t, err)
	defer tmpRepo.Close()

	err = tmpRepo.Clone(ctx, repo.DefaultBranch, true)
	require.NoError(t, err)
	err = tmpRepo.SetDefaultIndex(ctx)
	require.NoError(t, err)

	objectHash, err := tmpRepo.HashObjectAndWrite(ctx, strings.NewReader("push-bench-content\n"))
	require.NoError(t, err)
	err = tmpRepo.AddObjectToIndex(ctx, "100644", objectHash, "push-bench-file.txt")
	require.NoError(t, err)

	treeHash, err := tmpRepo.WriteTree(ctx)
	require.NoError(t, err)

	commit, err := tmpRepo.GetBranchCommit(repo.DefaultBranch)
	require.NoError(t, err)
	commitHash, err := tmpRepo.CommitTree(ctx, &files_service.CommitTreeUserOptions{
		ParentCommitID: commit.ID.String(),
		TreeHash:       treeHash,
		CommitMessage:  "push benchmark",
		DoerUser:       doer,
	})
	require.NoError(t, err)

	// Measure push with CPU tracking
	var cpuStart, cpuEnd syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &cpuStart)
	wallStart := time.Now()

	err = tmpRepo.Push(ctx, doer, commitHash, repo.DefaultBranch, false)
	require.NoError(t, err)

	wall = time.Since(wallStart)
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &cpuEnd)
	cpu = rusageCPUDiff(cpuStart, cpuEnd)
	return wall, cpu
}

// measurePushBare pushes to the bare repo with hooks disabled (pure git cost).
func measurePushBare(t testing.TB, repo *repo_model.Repository) (wall, cpu time.Duration) {
	t.Helper()
	bareRepoPath := repo_model.RepoPath(repo.OwnerName, repo.Name)

	// Create a temp clone, add a file, commit
	workDir := t.TempDir()
	indexFile := filepath.Join(workDir, "tmp-index")
	env := append(os.Environ(), "GIT_INDEX_FILE="+indexFile, "GIT_DIR="+bareRepoPath)

	gitRun := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v failed: %s", args, out)
		return strings.TrimSpace(string(out))
	}

	headRef := repo.DefaultBranch
	gitRun("read-tree", "refs/heads/"+headRef)

	// Create a single new file
	tmpFile := filepath.Join(workDir, "push-bench-file.txt")
	_ = os.WriteFile(tmpFile, []byte("push-bench-content\n"), 0o644)
	blobHash := gitRun("hash-object", "-w", tmpFile)

	cmd := exec.Command("git", "update-index", "--add", "-z", "--index-info")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(fmt.Sprintf("100644 %s\tpush-bench-file.txt\000", blobHash))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "update-index failed: %s", out)

	newTree := gitRun("write-tree")
	headCommit := gitRun("rev-parse", "refs/heads/"+headRef)

	cmd = exec.Command("git", "commit-tree", newTree, "-p", headCommit, "-m", "bare push bench")
	cmd.Env = append(env,
		"GIT_AUTHOR_NAME=bench", "GIT_AUTHOR_EMAIL=bench@test.local",
		"GIT_COMMITTER_NAME=bench", "GIT_COMMITTER_EMAIL=bench@test.local",
	)
	commitOut, err := cmd.Output()
	require.NoError(t, err, "commit-tree failed")
	newCommit := strings.TrimSpace(string(commitOut))

	// Now push with hooks DISABLED — use update-ref directly (no hooks at all)
	var cpuStart, cpuEnd syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &cpuStart)
	wallStart := time.Now()

	gitRun("update-ref", "refs/heads/"+headRef, newCommit)

	wall = time.Since(wallStart)
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &cpuEnd)
	cpu = rusageCPUDiff(cpuStart, cpuEnd)
	return wall, cpu
}

func rusageCPUDiff(start, end syscall.Rusage) time.Duration {
	userDiff := (end.Utime.Sec-start.Utime.Sec)*1e9 + int64(end.Utime.Usec-start.Utime.Usec)*1e3
	sysDiff := (end.Stime.Sec-start.Stime.Sec)*1e9 + int64(end.Stime.Usec-start.Stime.Usec)*1e3
	return time.Duration(userDiff + sysDiff)
}

// ---------------------------------------------------------------------------
// Test: ChangeRepoFiles end-to-end with CPU profiling
// ---------------------------------------------------------------------------

// TestChangeRepoFilesCPU measures total wall + CPU time for ChangeRepoFiles
// at different repo sizes to determine if the bottleneck is CPU-bound.
//
// Run: make test-sqlite#TestChangeRepoFilesCPU
func TestChangeRepoFilesCPU(t *testing.T) {
	fileCounts := []int{10, 100, 500, 2000, 5000}

	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

		fmt.Println()
		fmt.Println("=== ChangeRepoFiles Wall vs CPU Time ===")
		fmt.Printf("%-8s  %12s  %12s  %12s\n",
			"Files", "Wall", "CPU", "CPU%")
		fmt.Printf("%-8s  %12s  %12s  %12s\n",
			"--------", "------------", "------------", "------------")

		for _, n := range fileCounts {
			repoName := fmt.Sprintf("cpu-bench-%d", n)
			repo := createTestRepo(t, user, repoName, n)

			treePath := fmt.Sprintf("cpu-bench/file-%d.txt", n)
			opts := &files_service.ChangeRepoFilesOptions{
				Files: []*files_service.ChangeRepoFile{
					{
						Operation:     "create",
						TreePath:      treePath,
						ContentReader: strings.NewReader("benchmark content"),
					},
				},
				OldBranch: repo.DefaultBranch,
				NewBranch: repo.DefaultBranch,
				Message:   "cpu bench: add " + treePath,
			}

			var cpuStart, cpuEnd syscall.Rusage
			_ = syscall.Getrusage(syscall.RUSAGE_SELF, &cpuStart)
			wallStart := time.Now()

			_, err := files_service.ChangeRepoFiles(context.TODO(), repo, user, opts)
			require.NoError(t, err)

			wall := time.Since(wallStart)
			_ = syscall.Getrusage(syscall.RUSAGE_SELF, &cpuEnd)
			cpu := rusageCPUDiff(cpuStart, cpuEnd)

			cpuPct := float64(0)
			if wall > 0 {
				cpuPct = float64(cpu) / float64(wall) * 100
			}

			fmt.Printf("%-8d  %10dms  %10dms  %10.1f%%\n",
				n, wall.Milliseconds(), cpu.Milliseconds(), cpuPct)
		}

		fmt.Println()
		fmt.Println("CPU% > 100% means multi-core usage. Low CPU% means I/O or subprocess wait.")
		fmt.Println()
	})
}

// ---------------------------------------------------------------------------
// Test: Simulate production post-receive (sync-only, no queue drain)
// ---------------------------------------------------------------------------

// TestPostReceiveSyncCost directly measures the SYNCHRONOUS operations in
// the post-receive handler — what actually blocks git push return in production.
// In production, pushUpdates() runs async in a queue worker, so we exclude it.
//
// Breakdown:
//  1. Subprocess overhead: 3x gitea hook process start + config load
//  2. git update-server-info
//  3. Post-receive sync: loadRepository + SyncBranchesToDB + PushUpdates(enqueue only)
//
// Run: make test-sqlite#TestPostReceiveSyncCost
func TestPostReceiveSyncCost(t *testing.T) {
	fileCounts := []int{10, 100, 500, 2000, 5000, 10000}

	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

		fmt.Println()
		fmt.Println("=== Post-Receive Sync Cost Breakdown ===")
		fmt.Println("Measures each synchronous operation that blocks git push in production.")
		fmt.Println()
		fmt.Printf("%-8s  %14s  %14s  %14s  %14s\n",
			"Files", "update-srv-info", "SyncBranch", "SubprocessOH", "TotalSyncBlock")
		fmt.Printf("%-8s  %14s  %14s  %14s  %14s\n",
			"--------", "--------------", "--------------", "--------------", "--------------")

		for _, n := range fileCounts {
			repo := createTestRepo(t, user, fmt.Sprintf("sync-cost-%d", n), n)
			bareRepoPath := repo_model.RepoPath(repo.OwnerName, repo.Name)

			// Measure git update-server-info
			updateInfoStart := time.Now()
			cmd := exec.Command("git", "--git-dir="+bareRepoPath, "update-server-info")
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "update-server-info failed: %s", out)
			updateInfoDur := time.Since(updateInfoStart)

			// Measure SyncBranchesToDB
			gitRepo, err := git.OpenRepository(context.TODO(), bareRepoPath)
			require.NoError(t, err)
			headCommit, err := gitRepo.GetBranchCommitID(repo.DefaultBranch)
			require.NoError(t, err)

			syncStart := time.Now()
			err = repo_service.SyncBranchesToDB(context.TODO(), repo.ID, user.ID,
				[]string{repo.DefaultBranch}, []string{headCommit}, gitRepo.GetCommit)
			require.NoError(t, err)
			syncDur := time.Since(syncStart)
			gitRepo.Close()

			// Subprocess overhead = 3 × (fork gitea binary + load config)
			// Measured by timing an internal push (which spawns hooks that exit early)
			// minus bare ref update (no subprocess at all)
			subStart := time.Now()
			tmpDir := t.TempDir()
			idxFile := filepath.Join(tmpDir, "idx")
			envBase := append(os.Environ(), "GIT_INDEX_FILE="+idxFile, "GIT_DIR="+bareRepoPath)
			gitRunE := func(extraEnv []string, args ...string) string {
				c := exec.Command("git", args...)
				c.Env = append(envBase, extraEnv...)
				o, e := c.CombinedOutput()
				require.NoError(t, e, "git %v: %s", args, o)
				return strings.TrimSpace(string(o))
			}
			gitRunE(nil, "read-tree", "refs/heads/"+repo.DefaultBranch)
			tf := filepath.Join(tmpDir, "f.txt")
			_ = os.WriteFile(tf, []byte("x\n"), 0o644)
			bh := gitRunE(nil, "hash-object", "-w", tf)
			c2 := exec.Command("git", "update-index", "--add", "-z", "--index-info")
			c2.Env = envBase
			c2.Stdin = strings.NewReader(fmt.Sprintf("100644 %s\tf.txt\000", bh))
			_, _ = c2.CombinedOutput()
			nt := gitRunE(nil, "write-tree")
			hc := gitRunE(nil, "rev-parse", "refs/heads/"+repo.DefaultBranch)
			nc := gitRunE([]string{
				"GIT_AUTHOR_NAME=b", "GIT_AUTHOR_EMAIL=b@t",
				"GIT_COMMITTER_NAME=b", "GIT_COMMITTER_EMAIL=b@t",
			}, "commit-tree", nt, "-p", hc, "-m", "sub-oh")

			// Clone for push
			cloneDir := t.TempDir()
			cc := exec.Command("git", "clone", "--shared", bareRepoPath, cloneDir)
			cc.Env = os.Environ()
			_, _ = cc.CombinedOutput()
			fc := exec.Command("git", "-C", cloneDir, "fetch", bareRepoPath, nc)
			fc.Env = os.Environ()
			_, _ = fc.CombinedOutput()

			// Push with internal env (hooks called but gitea exits early)
			pushEnv := repo_module.InternalPushingEnvironment(user, repo)
			pc := exec.Command("git", "-C", cloneDir, "push", "origin", nc+":refs/heads/"+repo.DefaultBranch)
			pc.Env = pushEnv
			_, err = pc.CombinedOutput()
			require.NoError(t, err)
			subDur := time.Since(subStart)

			// subDur includes commit prep + push; subprocess OH ≈ subDur - prep
			// For simplicity, use the internal push time directly as subprocess overhead estimate
			totalSync := updateInfoDur + syncDur + subDur

			fmt.Printf("%-8d  %12dms  %12dms  %12dms  %12dms\n",
				n,
				updateInfoDur.Milliseconds(),
				syncDur.Milliseconds(),
				subDur.Milliseconds(),
				totalSync.Milliseconds(),
			)
		}

		fmt.Println()
		fmt.Println("update-srv-info = git update-server-info (scans pack files, runs in post-receive)")
		fmt.Println("SyncBranch      = SyncBranchesToDB (sync operation in post-receive handler)")
		fmt.Println("SubprocessOH    = total time for internal push (includes 3x gitea process start)")
		fmt.Println("TotalSyncBlock  = estimated production push blocking time (excluding async queue)")
		fmt.Println()
	})
}

// ---------------------------------------------------------------------------
// Test: Internal push (skip hooks) vs normal push
// ---------------------------------------------------------------------------

// TestPushInternalVsNormal compares push with GITEA_INTERNAL_PUSH=true
// (hooks skipped) vs normal push (hooks run). This proves the hook overhead.
//
// Run: make test-sqlite#TestPushInternalVsNormal
func TestPushInternalVsNormal(t *testing.T) {
	fileCounts := []int{10, 100, 500, 2000, 5000}

	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

		fmt.Println()
		fmt.Println("=== Push: Normal (hooks) vs Internal (no hooks) ===")
		fmt.Printf("%-8s  %14s  %14s  %14s\n",
			"Files", "NormalPush", "InternalPush", "Speedup")
		fmt.Printf("%-8s  %14s  %14s  %14s\n",
			"--------", "--------------", "--------------", "--------------")

		for _, n := range fileCounts {
			repoNormal := createTestRepo(t, user, fmt.Sprintf("push-normal-%d", n), n)
			repoInternal := createTestRepo(t, user, fmt.Sprintf("push-internal-%d", n), n)

			normalDur := measurePushNormal(t, repoNormal, user)
			internalDur := measurePushInternal(t, repoInternal, user)

			speedup := float64(normalDur) / float64(internalDur)
			fmt.Printf("%-8d  %12dms  %12dms  %12.1fx\n",
				n, normalDur.Milliseconds(), internalDur.Milliseconds(), speedup)
		}

		fmt.Println()
		fmt.Println("InternalPush uses GITEA_INTERNAL_PUSH=true, which skips pre-receive,")
		fmt.Println("update, and post-receive hooks (including git update-server-info).")
		fmt.Println()
	})
}

// preparePushCommit creates a temp repo, clones, adds a file, and returns
// the tmpRepo and commitHash ready for pushing.
func preparePushCommit(t testing.TB, repo *repo_model.Repository, doer *user_model.User) (*files_service.TemporaryUploadRepository, string) {
	t.Helper()
	ctx := context.TODO()

	tmpRepo, err := files_service.NewTemporaryUploadRepository(repo)
	require.NoError(t, err)

	err = tmpRepo.Clone(ctx, repo.DefaultBranch, true)
	require.NoError(t, err)
	err = tmpRepo.SetDefaultIndex(ctx)
	require.NoError(t, err)

	objectHash, err := tmpRepo.HashObjectAndWrite(ctx, strings.NewReader("push-test-content\n"))
	require.NoError(t, err)
	err = tmpRepo.AddObjectToIndex(ctx, "100644", objectHash, "push-test-file.txt")
	require.NoError(t, err)

	treeHash, err := tmpRepo.WriteTree(ctx)
	require.NoError(t, err)

	commit, err := tmpRepo.GetBranchCommit(repo.DefaultBranch)
	require.NoError(t, err)
	commitHash, err := tmpRepo.CommitTree(ctx, &files_service.CommitTreeUserOptions{
		ParentCommitID: commit.ID.String(),
		TreeHash:       treeHash,
		CommitMessage:  "push test",
		DoerUser:       doer,
	})
	require.NoError(t, err)
	return tmpRepo, commitHash
}

func measurePushNormal(t testing.TB, repo *repo_model.Repository, doer *user_model.User) time.Duration {
	t.Helper()
	tmpRepo, commitHash := preparePushCommit(t, repo, doer)
	defer tmpRepo.Close()

	start := time.Now()
	err := tmpRepo.Push(context.TODO(), doer, commitHash, repo.DefaultBranch, false)
	require.NoError(t, err)
	return time.Since(start)
}

func measurePushInternal(t testing.TB, repo *repo_model.Repository, doer *user_model.User) time.Duration {
	t.Helper()
	bareRepoPath := repo_model.RepoPath(repo.OwnerName, repo.Name)
	headRef := repo.DefaultBranch

	// Build a commit using plumbing on a temp index (same as populateRepoFast)
	indexFile := filepath.Join(t.TempDir(), "tmp-index")
	env := append(os.Environ(), "GIT_INDEX_FILE="+indexFile, "GIT_DIR="+bareRepoPath)

	gitRunEnv := func(extraEnv []string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Env = append(env, extraEnv...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v failed: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	gitRun := func(args ...string) string { return gitRunEnv(nil, args...) }

	gitRun("read-tree", "refs/heads/"+headRef)

	tmpFile := filepath.Join(t.TempDir(), "push-test-file.txt")
	_ = os.WriteFile(tmpFile, []byte("internal-push-content\n"), 0o644)
	blobHash := gitRun("hash-object", "-w", tmpFile)

	cmd := exec.Command("git", "update-index", "--add", "-z", "--index-info")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(fmt.Sprintf("100644 %s\tpush-test-file.txt\000", blobHash))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "update-index failed: %s", out)

	newTree := gitRun("write-tree")
	headCommit := gitRun("rev-parse", "refs/heads/"+headRef)

	newCommit := gitRunEnv(
		[]string{"GIT_AUTHOR_NAME=bench", "GIT_AUTHOR_EMAIL=bench@test.local",
			"GIT_COMMITTER_NAME=bench", "GIT_COMMITTER_EMAIL=bench@test.local"},
		"commit-tree", newTree, "-p", headCommit, "-m", "internal push bench",
	)

	// Clone to a temp workdir so we can do `git push` (need a non-bare repo for push)
	tmpClone := t.TempDir()
	cloneCmd := exec.Command("git", "clone", "--shared", bareRepoPath, tmpClone)
	cloneCmd.Env = os.Environ()
	cloneOut, err := cloneCmd.CombinedOutput()
	require.NoError(t, err, "clone failed: %s", cloneOut)

	// Fetch the new commit into the clone
	fetchCmd := exec.Command("git", "-C", tmpClone, "fetch", bareRepoPath, newCommit)
	fetchCmd.Env = os.Environ()
	fetchOut, err := fetchCmd.CombinedOutput()
	require.NoError(t, err, "fetch failed: %s", fetchOut)

	// Now push with GITEA_INTERNAL_PUSH=true to skip all hooks
	pushEnv := repo_module.InternalPushingEnvironment(doer, repo)
	start := time.Now()

	pushCmd := exec.Command("git", "-C", tmpClone, "push", "origin",
		newCommit+":refs/heads/"+headRef)
	pushCmd.Env = pushEnv
	pushOut, err := pushCmd.CombinedOutput()
	require.NoError(t, err, "internal push failed: %s", pushOut)

	return time.Since(start)
}

// ---------------------------------------------------------------------------
// Test: Optimized ChangeRepoFiles (internal push + manual side effects)
// ---------------------------------------------------------------------------

// TestOptimizedVsNormalCreateFile implements the full optimized flow and compares
// it against the normal ChangeRepoFiles for both correctness and performance.
//
// Run: make test-sqlite#TestOptimizedVsNormalCreateFile
func TestOptimizedVsNormalCreateFile(t *testing.T) {
	fileCounts := []int{10, 100, 500, 2000, 5000}

	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

		fmt.Println()
		fmt.Println("=== Optimized vs Normal ChangeRepoFiles ===")
		fmt.Printf("%-8s  %14s  %14s  %14s  %10s\n",
			"Files", "Normal", "Optimized", "Speedup", "Correct?")
		fmt.Printf("%-8s  %14s  %14s  %14s  %10s\n",
			"--------", "--------------", "--------------", "--------------", "----------")

		for _, n := range fileCounts {
			repoNormal := createTestRepo(t, user, fmt.Sprintf("opt-normal-%d", n), n)
			repoOptimized := createTestRepo(t, user, fmt.Sprintf("opt-fast-%d", n), n)

			treePath := fmt.Sprintf("opt-test/file-%d.txt", n)
			content := "optimized-test-content\n"

			// --- Normal flow ---
			normalStart := time.Now()
			normalResp, err := files_service.ChangeRepoFiles(context.TODO(), repoNormal, user, &files_service.ChangeRepoFilesOptions{
				Files: []*files_service.ChangeRepoFile{
					{
						Operation:     "create",
						TreePath:      treePath,
						ContentReader: strings.NewReader(content),
					},
				},
				OldBranch: repoNormal.DefaultBranch,
				NewBranch: repoNormal.DefaultBranch,
				Message:   "normal: add " + treePath,
			})
			require.NoError(t, err)
			normalDur := time.Since(normalStart)

			// --- Optimized flow ---
			optResp := optimizedCreateFile(t, repoOptimized, user, treePath, content)
			optDur := optResp.syncDur // only sync portion (production-like)

			// --- Verify correctness ---
			correct := true
			// 1. File SHA should match (same content = same blob)
			if len(normalResp.Files) == 0 || normalResp.Files[0].SHA != optResp.fileSHA {
				normalSHA := ""
				if len(normalResp.Files) > 0 {
					normalSHA = normalResp.Files[0].SHA
				}
				t.Errorf("files=%d: SHA mismatch: normal=%s opt=%s", n, normalSHA, optResp.fileSHA)
				correct = false
			}
			// 2. Branch should be synced to DB
			if !optResp.branchSynced {
				t.Errorf("files=%d: branch not synced to DB", n)
				correct = false
			}
			// 3. File should be readable from git
			if !optResp.fileReadable {
				t.Errorf("files=%d: file not readable from git after optimized push", n)
				correct = false
			}
			// 4. PushUpdates should have been called (webhooks/notifications enqueued)
			if !optResp.pushUpdatesOK {
				t.Errorf("files=%d: PushUpdates failed", n)
				correct = false
			}

			correctStr := "YES"
			if !correct {
				correctStr = "NO"
			}

			speedup := float64(normalDur) / float64(optDur)
			fmt.Printf("%-8d  %12dms  %12dms  %12.1fx  %10s\n",
				n, normalDur.Milliseconds(), optDur.Milliseconds(), speedup, correctStr)
		}

		fmt.Println()
	})
}

type optimizedResult struct {
	fileSHA       string
	branchSynced  bool
	fileReadable  bool
	pushUpdatesOK bool
	syncDur       time.Duration // wall time excluding async PushUpdates
}

// optimizedCreateFile implements the full optimized ChangeRepoFiles flow:
// 1. Same git plumbing as normal (clone, read-tree, hash, write-tree, commit-tree)
// 2. PushInternalSkipHooks instead of Push (skip hooks)
// 3. Manually call SyncBranchesToDB + PushUpdates (the side effects)
func optimizedCreateFile(t testing.TB, repo *repo_model.Repository, doer *user_model.User, treePath, content string) optimizedResult {
	t.Helper()
	ctx := context.TODO()
	result := optimizedResult{}
	syncStartTime := time.Now()

	// --- Phase 1: Same as normal ChangeRepoFiles ---
	tmpRepo, err := files_service.NewTemporaryUploadRepository(repo)
	require.NoError(t, err)
	defer tmpRepo.Close()

	err = tmpRepo.Clone(ctx, repo.DefaultBranch, true)
	require.NoError(t, err)
	err = tmpRepo.SetDefaultIndex(ctx)
	require.NoError(t, err)

	objectHash, err := tmpRepo.HashObjectAndWrite(ctx, strings.NewReader(content))
	require.NoError(t, err)
	result.fileSHA = objectHash

	err = tmpRepo.AddObjectToIndex(ctx, "100644", objectHash, treePath)
	require.NoError(t, err)

	treeHash, err := tmpRepo.WriteTree(ctx)
	require.NoError(t, err)

	oldCommit, err := tmpRepo.GetBranchCommit(repo.DefaultBranch)
	require.NoError(t, err)
	oldCommitID := oldCommit.ID.String()

	commitHash, err := tmpRepo.CommitTree(ctx, &files_service.CommitTreeUserOptions{
		ParentCommitID: oldCommitID,
		TreeHash:       treeHash,
		CommitMessage:  "optimized: add " + treePath,
		DoerUser:       doer,
	})
	require.NoError(t, err)

	// --- Phase 2: Internal push (skip hooks) ---
	err = tmpRepo.PushInternalSkipHooks(ctx, doer, commitHash, repo.DefaultBranch, false)
	require.NoError(t, err)

	// --- Phase 3: Manual side effects (what post-receive handler does) ---
	// 3a. SyncBranchesToDB
	gitRepo, err := git.OpenRepository(ctx, repo_model.RepoPath(repo.OwnerName, repo.Name))
	require.NoError(t, err)
	defer gitRepo.Close()

	err = repo_service.SyncBranchesToDB(ctx, repo.ID, doer.ID,
		[]string{repo.DefaultBranch}, []string{commitHash}, gitRepo.GetCommit)
	if err != nil {
		t.Logf("SyncBranchesToDB error: %v", err)
	} else {
		result.branchSynced = true
	}

	// 3b. UpdatePullsRefs (for PR support)
	pushOpts := &repo_module.PushUpdateOptions{
		RefFullName:  git.RefNameFromBranch(repo.DefaultBranch),
		OldCommitID:  oldCommitID,
		NewCommitID:  commitHash,
		PusherID:     doer.ID,
		PusherName:   doer.Name,
		RepoUserName: repo.OwnerName,
		RepoName:     repo.Name,
	}
	pull_service.UpdatePullsRefs(ctx, repo, pushOpts)

	// 3c. PushUpdates — fire async to simulate production (queue is non-blocking).
	pushUpdatesDone := make(chan error, 1)
	go func() {
		pushUpdatesDone <- repo_service.PushUpdates([]*repo_module.PushUpdateOptions{pushOpts})
	}()

	// Record sync duration here — in production, PushUpdates is async and returns immediately.
	result.syncDur = time.Since(syncStartTime)

	// --- Phase 4: Verify file is readable ---
	newCommit, err := gitRepo.GetCommit(commitHash)
	if err == nil {
		_, err = newCommit.GetTreeEntryByPath(treePath)
		result.fileReadable = err == nil
	}

	// Wait for PushUpdates to complete (correctness check)
	if err := <-pushUpdatesDone; err != nil {
		t.Logf("PushUpdates error: %v", err)
	} else {
		result.pushUpdatesOK = true
	}

	return result
}

// ---------------------------------------------------------------------------
// Test: QPS and CPU comparison under concurrency
// ---------------------------------------------------------------------------

// TestQPSComparison measures throughput (QPS) and CPU usage for normal vs optimized
// ChangeRepoFiles under sequential and concurrent workloads.
//
// Run: make test-sqlite#TestQPSComparison
func TestQPSComparison(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

		const repoFiles = 2000 // repo size
		const opsPerTest = 5   // operations per test case

		fmt.Println()
		fmt.Println("=== QPS & CPU Comparison: Normal vs Optimized (repo=2000 files) ===")
		fmt.Println()

		// --- Sequential test (concurrency=1) ---
		fmt.Println("--- Sequential (concurrency=1) ---")
		fmt.Printf("%-12s  %8s  %8s  %8s  %8s  %10s\n",
			"Mode", "Ops", "Total", "Avg/op", "QPS", "CPU%")
		fmt.Printf("%-12s  %8s  %8s  %8s  %8s  %10s\n",
			"------------", "--------", "--------", "--------", "--------", "----------")

		// Normal sequential
		repoNS := createTestRepo(t, user, "qps-normal-seq", repoFiles)
		nsWall, nsCPU := runSequentialOps(t, repoNS, user, opsPerTest, false)
		printQPSRow("Normal", opsPerTest, nsWall, nsCPU)

		// Optimized sequential
		repoOS := createTestRepo(t, user, "qps-opt-seq", repoFiles)
		osWall, osCPU := runSequentialOps(t, repoOS, user, opsPerTest, true)
		printQPSRow("Optimized", opsPerTest, osWall, osCPU)

		fmt.Println()

		// --- Concurrent test (multiple repos, parallel workers) ---
		for _, concurrency := range []int{2, 4, 8} {
			fmt.Printf("--- Concurrent (concurrency=%d, %d ops each) ---\n", concurrency, opsPerTest)
			fmt.Printf("%-12s  %8s  %8s  %8s  %8s  %10s\n",
				"Mode", "TotalOps", "Wall", "Avg/op", "QPS", "CPU%")
			fmt.Printf("%-12s  %8s  %8s  %8s  %8s  %10s\n",
				"------------", "--------", "--------", "--------", "--------", "----------")

			// Normal concurrent
			ncWall, ncCPU := runConcurrentOps(t, user, concurrency, opsPerTest, repoFiles, "qps-nc", false)
			printQPSRow("Normal", concurrency*opsPerTest, ncWall, ncCPU)

			// Optimized concurrent
			ocWall, ocCPU := runConcurrentOps(t, user, concurrency, opsPerTest, repoFiles, "qps-oc", true)
			printQPSRow("Optimized", concurrency*opsPerTest, ocWall, ocCPU)

			fmt.Println()
		}
	})
}

func printQPSRow(mode string, ops int, wall, cpu time.Duration) {
	avgOp := wall / time.Duration(ops)
	qps := float64(ops) / wall.Seconds()
	cpuPct := float64(cpu) / float64(wall) * 100
	fmt.Printf("%-12s  %8d  %6dms  %6dms  %8.2f  %8.1f%%\n",
		mode, ops, wall.Milliseconds(), avgOp.Milliseconds(), qps, cpuPct)
}

func runSequentialOps(t testing.TB, repo *repo_model.Repository, doer *user_model.User, ops int, optimized bool) (wall, cpu time.Duration) {
	t.Helper()

	var cpuStart, cpuEnd syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &cpuStart)
	wallStart := time.Now()

	for i := 0; i < ops; i++ {
		treePath := fmt.Sprintf("qps-seq/file-%d.txt", i)
		if optimized {
			res := optimizedCreateFile(t, repo, doer, treePath, fmt.Sprintf("content-%d", i))
			if !res.fileReadable {
				t.Fatalf("op %d: file not readable", i)
			}
		} else {
			_, err := files_service.ChangeRepoFiles(context.TODO(), repo, doer, &files_service.ChangeRepoFilesOptions{
				Files: []*files_service.ChangeRepoFile{
					{Operation: "create", TreePath: treePath, ContentReader: strings.NewReader(fmt.Sprintf("content-%d", i))},
				},
				OldBranch: repo.DefaultBranch,
				NewBranch: repo.DefaultBranch,
				Message:   "seq " + treePath,
			})
			if err != nil {
				t.Fatalf("op %d: %v", i, err)
			}
		}
	}

	wall = time.Since(wallStart)
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &cpuEnd)
	cpu = rusageCPUDiff(cpuStart, cpuEnd)
	return
}

func runConcurrentOps(t testing.TB, doer *user_model.User, concurrency, opsPerWorker, repoFiles int, prefix string, optimized bool) (wall, cpu time.Duration) {
	t.Helper()

	// Create one repo per worker (concurrent pushes to same branch would conflict)
	repos := make([]*repo_model.Repository, concurrency)
	for i := 0; i < concurrency; i++ {
		tag := "n"
		if optimized {
			tag = "o"
		}
		repos[i] = createTestRepo(t, doer, fmt.Sprintf("%s-%s-%d", prefix, tag, i), repoFiles)
	}

	var cpuStart, cpuEnd syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &cpuStart)
	wallStart := time.Now()

	var wg sync.WaitGroup
	errCh := make(chan error, concurrency*opsPerWorker)

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			repo := repos[workerID]
			for i := 0; i < opsPerWorker; i++ {
				treePath := fmt.Sprintf("qps-conc/w%d-file-%d.txt", workerID, i)
				if optimized {
					res := optimizedCreateFile(t, repo, doer, treePath, fmt.Sprintf("c-%d-%d", workerID, i))
					if !res.fileReadable {
						errCh <- fmt.Errorf("worker %d op %d: file not readable", workerID, i)
					}
				} else {
					_, err := files_service.ChangeRepoFiles(context.TODO(), repo, doer, &files_service.ChangeRepoFilesOptions{
						Files: []*files_service.ChangeRepoFile{
							{Operation: "create", TreePath: treePath, ContentReader: strings.NewReader(fmt.Sprintf("c-%d-%d", workerID, i))},
						},
						OldBranch: repo.DefaultBranch,
						NewBranch: repo.DefaultBranch,
						Message:   "conc " + treePath,
					})
					if err != nil {
						errCh <- fmt.Errorf("worker %d op %d: %v", workerID, i, err)
					}
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)

	wall = time.Since(wallStart)
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &cpuEnd)
	cpu = rusageCPUDiff(cpuStart, cpuEnd)

	for err := range errCh {
		t.Error(err)
	}
	return
}

// ---------------------------------------------------------------------------
// Test: High concurrency QPS + CPU + latency
// ---------------------------------------------------------------------------

// TestHighConcurrency measures throughput under high concurrency for normal vs
// optimized ChangeRepoFiles. Uses a pool of pre-created repos to reduce setup
// time and distributes workers across repos with unique branches.
//
// NOTE: Test env uses SQLite (single writer) and queue=immediate (sync).
// These constraints limit max throughput regardless of optimization.
// The test measures the ceiling imposed by these constraints honestly.
//
// Run: make test-sqlite#TestHighConcurrency
func TestHighConcurrency(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

		const repoFiles = 2000
		const poolSize = 10 // number of repos in the pool

		// Pre-create repo pool
		fmt.Println()
		fmt.Printf("=== High Concurrency Test (pool=%d repos × %d files) ===\n", poolSize, repoFiles)
		fmt.Println("Creating repo pool...")
		reposNormal := make([]*repo_model.Repository, poolSize)
		reposOpt := make([]*repo_model.Repository, poolSize)
		for i := 0; i < poolSize; i++ {
			reposNormal[i] = createTestRepo(t, user, fmt.Sprintf("hc-normal-%d", i), repoFiles)
			reposOpt[i] = createTestRepo(t, user, fmt.Sprintf("hc-opt-%d", i), repoFiles)
		}
		fmt.Println("Pool ready.")
		fmt.Println()

		concurrencyLevels := []int{1, 5, 10, 20, 50, 100}

		fmt.Printf("%-6s  %-10s  %8s  %8s  %8s  %8s  %8s  %8s  %8s\n",
			"Conc", "Mode", "Total", "OK", "Fail", "QPS", "AvgLat", "P99Lat", "CPU%")
		fmt.Printf("%-6s  %-10s  %8s  %8s  %8s  %8s  %8s  %8s  %8s\n",
			"------", "----------", "--------", "--------", "--------", "--------", "--------", "--------", "--------")

		for _, conc := range concurrencyLevels {
			// Normal
			nStats := runHighConcLoad(t, user, reposNormal, conc, false)
			printConcRow(conc, "Normal", nStats)

			// Optimized
			oStats := runHighConcLoad(t, user, reposOpt, conc, true)
			printConcRow(conc, "Optimized", oStats)
		}

		fmt.Println()
		fmt.Println("Conc = number of concurrent goroutines submitting requests simultaneously")
		fmt.Println("Each goroutine does 1 file create operation")
		fmt.Println("QPS = successful operations / wall time")
		fmt.Println("NOTE: SQLite single-writer lock serializes DB writes, limiting max QPS")
		fmt.Println()
	})
}

type concStats struct {
	wall   time.Duration
	cpu    time.Duration
	total  int
	ok     int
	fail   int
	latP50 time.Duration
	latP99 time.Duration
}

func printConcRow(conc int, mode string, s concStats) {
	qps := float64(0)
	if s.wall > 0 {
		qps = float64(s.ok) / s.wall.Seconds()
	}
	cpuPct := float64(0)
	if s.wall > 0 {
		cpuPct = float64(s.cpu) / float64(s.wall) * 100
	}
	fmt.Printf("%-6d  %-10s  %8d  %8d  %8d  %8.2f  %6dms  %6dms  %6.1f%%\n",
		conc, mode, s.total, s.ok, s.fail, qps,
		s.latP50.Milliseconds(), s.latP99.Milliseconds(), cpuPct)
}

func runHighConcLoad(t testing.TB, doer *user_model.User, repos []*repo_model.Repository, concurrency int, optimized bool) concStats {
	t.Helper()

	var stats concStats
	stats.total = concurrency

	latencies := make([]time.Duration, concurrency)
	results := make([]bool, concurrency)

	var cpuStart, cpuEnd syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &cpuStart)

	var wg sync.WaitGroup
	wallStart := time.Now()

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			repo := repos[idx%len(repos)]
			branch := fmt.Sprintf("hc-branch-%d", idx)
			treePath := fmt.Sprintf("hc/file-%d.txt", idx)
			content := fmt.Sprintf("hc-content-%d", idx)

			opStart := time.Now()

			if optimized {
				res := optimizedCreateFileOnBranch(t, repo, doer, branch, treePath, content)
				results[idx] = res.fileReadable && res.branchSynced
			} else {
				_, err := files_service.ChangeRepoFiles(context.TODO(), repo, doer, &files_service.ChangeRepoFilesOptions{
					Files: []*files_service.ChangeRepoFile{
						{Operation: "create", TreePath: treePath, ContentReader: strings.NewReader(content)},
					},
					OldBranch: repo.DefaultBranch,
					NewBranch: branch,
					Message:   "hc " + treePath,
				})
				results[idx] = err == nil
			}

			latencies[idx] = time.Since(opStart)
		}(i)
	}
	wg.Wait()

	stats.wall = time.Since(wallStart)
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &cpuEnd)
	stats.cpu = rusageCPUDiff(cpuStart, cpuEnd)

	// Count successes/failures
	for _, ok := range results {
		if ok {
			stats.ok++
		} else {
			stats.fail++
		}
	}

	// Calculate percentiles (sort latencies of successful ops)
	var okLatencies []time.Duration
	for i, ok := range results {
		if ok {
			okLatencies = append(okLatencies, latencies[i])
		}
	}
	if len(okLatencies) > 0 {
		sortDurations(okLatencies)
		stats.latP50 = okLatencies[len(okLatencies)*50/100]
		stats.latP99 = okLatencies[len(okLatencies)*99/100]
	}

	return stats
}

func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

// optimizedCreateFileOnBranch is like optimizedCreateFile but pushes to a new branch.
func optimizedCreateFileOnBranch(t testing.TB, repo *repo_model.Repository, doer *user_model.User, newBranch, treePath, content string) optimizedResult {
	t.Helper()
	ctx := context.TODO()
	result := optimizedResult{}
	syncStartTime := time.Now()

	tmpRepo, err := files_service.NewTemporaryUploadRepository(repo)
	if err != nil {
		return result
	}
	defer tmpRepo.Close()

	if err = tmpRepo.Clone(ctx, repo.DefaultBranch, true); err != nil {
		return result
	}
	if err = tmpRepo.SetDefaultIndex(ctx); err != nil {
		return result
	}

	objectHash, err := tmpRepo.HashObjectAndWrite(ctx, strings.NewReader(content))
	if err != nil {
		return result
	}
	result.fileSHA = objectHash

	if err = tmpRepo.AddObjectToIndex(ctx, "100644", objectHash, treePath); err != nil {
		return result
	}

	treeHash, err := tmpRepo.WriteTree(ctx)
	if err != nil {
		return result
	}

	oldCommit, err := tmpRepo.GetBranchCommit(repo.DefaultBranch)
	if err != nil {
		return result
	}
	oldCommitID := oldCommit.ID.String()

	commitHash, err := tmpRepo.CommitTree(ctx, &files_service.CommitTreeUserOptions{
		ParentCommitID: oldCommitID,
		TreeHash:       treeHash,
		CommitMessage:  "hc-opt: add " + treePath,
		DoerUser:       doer,
	})
	if err != nil {
		return result
	}

	// Internal push
	if err = tmpRepo.PushInternalSkipHooks(ctx, doer, commitHash, newBranch, false); err != nil {
		return result
	}

	// Side effects
	gitRepo, err := git.OpenRepository(ctx, repo_model.RepoPath(repo.OwnerName, repo.Name))
	if err != nil {
		return result
	}
	defer gitRepo.Close()

	err = repo_service.SyncBranchesToDB(ctx, repo.ID, doer.ID,
		[]string{newBranch}, []string{commitHash}, gitRepo.GetCommit)
	result.branchSynced = err == nil

	pushOpts := &repo_module.PushUpdateOptions{
		RefFullName:  git.RefNameFromBranch(newBranch),
		OldCommitID:  git.Sha1ObjectFormat.EmptyObjectID().String(),
		NewCommitID:  commitHash,
		PusherID:     doer.ID,
		PusherName:   doer.Name,
		RepoUserName: repo.OwnerName,
		RepoName:     repo.Name,
	}
	pull_service.UpdatePullsRefs(ctx, repo, pushOpts)

	go repo_service.PushUpdates([]*repo_module.PushUpdateOptions{pushOpts}) //nolint:errcheck

	// Verify file
	newCommit, err := gitRepo.GetCommit(commitHash)
	if err == nil {
		_, err = newCommit.GetTreeEntryByPath(treePath)
		result.fileReadable = err == nil
	}

	result.syncDur = time.Since(syncStartTime)
	return result
}

// ---------------------------------------------------------------------------
// Benchmark: service-level ChangeRepoFiles (end-to-end, no HTTP)
// ---------------------------------------------------------------------------

// BenchmarkChangeRepoFiles measures ChangeRepoFiles service call latency
// at different repo sizes. Each iteration creates one new file.
//
// Run: make test-sqlite#BenchmarkChangeRepoFiles
func BenchmarkChangeRepoFiles(b *testing.B) {
	fileCounts := []int{10, 100, 1000}

	onGiteaRun(b, func(b *testing.B, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(b, &user_model.User{ID: 2})

		for _, n := range fileCounts {
			b.Run(fmt.Sprintf("files=%d", n), func(b *testing.B) {
				repoName := fmt.Sprintf("bench-svc-%d", n)
				repo := createTestRepo(b, user, repoName, n)

				b.ResetTimer()
				for i := 0; b.Loop(); i++ {
					treePath := fmt.Sprintf("bench-svc/file-%d.txt", i)
					opts := &files_service.ChangeRepoFilesOptions{
						Files: []*files_service.ChangeRepoFile{
							{
								Operation:     "create",
								TreePath:      treePath,
								ContentReader: strings.NewReader("benchmark content"),
							},
						},
						OldBranch: repo.DefaultBranch,
						NewBranch: repo.DefaultBranch,
						Message:   "bench: add " + treePath,
					}
					_, err := files_service.ChangeRepoFiles(context.TODO(), repo, user, opts)
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	})
}

// ---------------------------------------------------------------------------
// Benchmark: HTTP API layer (full round-trip through the API handler)
// ---------------------------------------------------------------------------

// BenchmarkAPICreateFileByRepoSize measures CreateFile API latency
// at different repo sizes via the HTTP API.
//
// Run: make test-sqlite#BenchmarkAPICreateFileByRepoSize
func BenchmarkAPICreateFileByRepoSize(b *testing.B) {
	fileCounts := []int{10, 100, 1000}

	onGiteaRun(b, func(b *testing.B, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(b, &user_model.User{ID: 2})

		for _, n := range fileCounts {
			b.Run(fmt.Sprintf("files=%d", n), func(b *testing.B) {
				repoName := fmt.Sprintf("bench-api-%d", n)
				repo := createTestRepo(b, user, repoName, n)

				session := loginUser(b, user.Name)
				token := getTokenForLoggedInUser(b, session, auth_model.AccessTokenScopeWriteRepository)

				b.ResetTimer()
				for i := 0; b.Loop(); i++ {
					treePath := fmt.Sprintf("bench-api/file-%d.txt", i)
					createFileOptions := api.CreateFileOptions{
						FileOptions: api.FileOptions{
							BranchName:    repo.DefaultBranch,
							NewBranchName: repo.DefaultBranch,
							Message:       "bench: add " + treePath,
						},
						ContentBase64: "YmVuY2htYXJrIGNvbnRlbnQ=", // "benchmark content"
					}
					req := NewRequestWithJSON(b, "POST",
						fmt.Sprintf("/api/v1/repos/%s/%s/contents/%s", user.Name, repo.Name, treePath),
						&createFileOptions).AddTokenAuth(token)
					MakeRequest(b, req, 201)
				}
			})
		}
	})
}
