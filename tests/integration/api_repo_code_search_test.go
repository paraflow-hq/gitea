// Copyright 2025 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"net/http"
	"testing"

	auth_model "code.gitea.io/gitea/models/auth"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIRepoCodeSearch(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	// user2/repo1 has README.md with content "# repo1\n\nDescription for repo1"
	token := getUserToken(t, "user2", auth_model.AccessTokenScopeReadRepository)

	// Test basic keyword search
	req := NewRequest(t, "GET", "/api/v1/repos/user2/repo1/code_search?q=repo1").
		AddTokenAuth(token)
	resp := MakeRequest(t, req, http.StatusOK)

	var result api.CodeSearchResponse
	DecodeJSON(t, resp, &result)
	assert.Positive(t, result.TotalCount)
	assert.NotEmpty(t, result.Items)

	// Verify result contains README.md
	found := false
	for _, item := range result.Items {
		if item.Filename == "README.md" {
			found = true
			assert.NotEmpty(t, item.Lines)
			break
		}
	}
	assert.True(t, found, "expected README.md in search results")

	// Test empty keyword returns empty results
	req = NewRequest(t, "GET", "/api/v1/repos/user2/repo1/code_search?q=").
		AddTokenAuth(token)
	resp = MakeRequest(t, req, http.StatusOK)

	var emptyResult api.CodeSearchResponse
	DecodeJSON(t, resp, &emptyResult)
	assert.Equal(t, 0, emptyResult.TotalCount)
	assert.Empty(t, emptyResult.Items)

	// Test no-match keyword returns empty results
	req = NewRequest(t, "GET", "/api/v1/repos/user2/repo1/code_search?q=absolutely_nonexistent_keyword_xyz").
		AddTokenAuth(token)
	resp = MakeRequest(t, req, http.StatusOK)

	var noMatchResult api.CodeSearchResponse
	DecodeJSON(t, resp, &noMatchResult)
	assert.Equal(t, 0, noMatchResult.TotalCount)
	assert.Empty(t, noMatchResult.Items)

	// Test search_mode=exact
	req = NewRequest(t, "GET", "/api/v1/repos/user2/repo1/code_search?q=Description+for+repo1&search_mode=exact").
		AddTokenAuth(token)
	resp = MakeRequest(t, req, http.StatusOK)

	var exactResult api.CodeSearchResponse
	DecodeJSON(t, resp, &exactResult)
	assert.Positive(t, exactResult.TotalCount)

	// Test unauthenticated access to public repo (repo1 is public)
	req = NewRequest(t, "GET", "/api/v1/repos/user2/repo1/code_search?q=repo1")
	resp = MakeRequest(t, req, http.StatusOK)

	var publicResult api.CodeSearchResponse
	DecodeJSON(t, resp, &publicResult)
	assert.Positive(t, publicResult.TotalCount)

	// Test access to non-existent repo returns 404
	req = NewRequest(t, "GET", "/api/v1/repos/user2/nonexistent/code_search?q=test").
		AddTokenAuth(token)
	MakeRequest(t, req, http.StatusNotFound)

	// Test line numbers are positive
	for _, item := range result.Items {
		for _, line := range item.Lines {
			require.Positive(t, line.LineNumber, "line number should be positive")
		}
	}
}
