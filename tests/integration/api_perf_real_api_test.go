// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !windows

package integration

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	api "code.gitea.io/gitea/modules/structs"
)

// TestRealAPIPerf measures the actual HTTP API latency and QPS for
// POST /repos/{owner}/{repo}/contents/{filepath} (CreateFile API).
// This is the end-user-visible performance number.
func TestRealAPIPerf(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		session := loginUser(t, user.Name)
		token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository)

		content := base64.StdEncoding.EncodeToString([]byte("api perf test content"))

		t.Log("=== Real HTTP API Performance: POST /repos/{owner}/{repo}/contents/{filepath} ===")
		t.Log("")

		// --- Latency vs repo size ---
		t.Log("--- Single request latency vs repo file count ---")
		t.Logf("%-8s  %12s", "Files", "Latency")
		for _, n := range []int{10, 100, 500, 2000, 5000} {
			repo := createTestRepo(t, user, fmt.Sprintf("api-perf-%d", n), n)

			tp := fmt.Sprintf("api-test/file-%d.txt", n)
			opts := api.CreateFileOptions{
				FileOptions: api.FileOptions{
					BranchName:    repo.DefaultBranch,
					NewBranchName: repo.DefaultBranch,
					Message:       "api perf: " + tp,
				},
				ContentBase64: content,
			}

			start := time.Now()
			req := NewRequestWithJSON(t, "POST",
				fmt.Sprintf("/api/v1/repos/%s/%s/contents/%s", user.Name, repo.Name, tp),
				&opts).AddTokenAuth(token)
			MakeRequest(t, req, http.StatusCreated)
			dur := time.Since(start)

			t.Logf("%-8d  %10dms", n, dur.Milliseconds())
		}

		// --- QPS at different concurrency ---
		t.Log("")
		t.Log("--- QPS vs concurrency (2000 files per repo) ---")
		t.Logf("%-6s  %6s  %6s  %8s  %8s", "Conc", "Ops", "OK", "Avg/op", "QPS")

		const repoFiles = 2000
		const opsPerWorker = 3

		for _, conc := range []int{1, 2, 4, 8} {
			repos := make([]struct {
				name  string
				owner string
			}, conc)
			for i := range conc {
				r := createTestRepo(t, user, fmt.Sprintf("api-qps-c%d-w%d", conc, i), repoFiles)
				repos[i].name = r.Name
				repos[i].owner = user.Name
			}

			var (
				wg    sync.WaitGroup
				mu    sync.Mutex
				okCnt int
			)
			start := time.Now()

			for w := range conc {
				wg.Add(1)
				go func(wid int) {
					defer wg.Done()
					for i := range opsPerWorker {
						tp := fmt.Sprintf("qps/w%d-f%d.txt", wid, i)
						opts := api.CreateFileOptions{
							FileOptions: api.FileOptions{
								Message: tp,
							},
							ContentBase64: content,
						}
						req := NewRequestWithJSON(t, "POST",
							fmt.Sprintf("/api/v1/repos/%s/%s/contents/%s",
								repos[wid].owner, repos[wid].name, tp),
							&opts).AddTokenAuth(token)
						resp := MakeRequest(t, req, 0) // accept any status
						if resp.Code == http.StatusCreated {
							mu.Lock()
							okCnt++
							mu.Unlock()
						}
					}
				}(w)
			}
			wg.Wait()
			wall := time.Since(start)

			totalOps := conc * opsPerWorker
			qps := float64(okCnt) / wall.Seconds()
			avg := wall / time.Duration(totalOps)
			t.Logf("%-6d  %6d  %6d  %6dms  %8.2f",
				conc, totalOps, okCnt, avg.Milliseconds(), qps)
		}
	})
}
