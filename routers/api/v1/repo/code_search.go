// Copyright 2025 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"net/http"
	"strings"

	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/indexer"
	"code.gitea.io/gitea/modules/indexer/code/gitgrep"
	"code.gitea.io/gitea/modules/setting"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/services/context"
)

// CodeSearch searches code within a repository using git grep
func CodeSearch(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/code_search repository repoCodeSearch
	// ---
	// summary: Search code in a repository
	// produces:
	//   - application/json
	// parameters:
	//   - name: owner
	//     in: path
	//     description: owner of the repo
	//     required: true
	//     type: string
	//   - name: repo
	//     in: path
	//     description: name of the repo
	//     required: true
	//     type: string
	//   - name: q
	//     in: query
	//     description: keyword to search
	//     required: true
	//     type: string
	//   - name: page
	//     in: query
	//     description: page number of results to return (1-based)
	//     type: integer
	//   - name: limit
	//     in: query
	//     description: page size of results
	//     type: integer
	//   - name: search_mode
	//     in: query
	//     description: "search mode: exact, words, regexp"
	//     type: string
	// responses:
	//   "200":
	//     description: code search results
	//     schema:
	//       "$ref": "#/definitions/CodeSearchResponse"
	//   "400":
	//     "$ref": "#/responses/invalidTopicsError"
	//   "404":
	//     "$ref": "#/responses/notFound"

	keyword := strings.TrimSpace(ctx.FormString("q"))
	if keyword == "" {
		ctx.JSON(http.StatusOK, api.CodeSearchResponse{
			TotalCount: 0,
			Items:      []*api.CodeSearchResultFile{},
		})
		return
	}

	page := ctx.FormInt("page")
	if page <= 0 {
		page = 1
	}

	limit := ctx.FormInt("limit")
	if limit <= 0 || limit > setting.UI.RepoSearchPagingNum {
		limit = setting.UI.RepoSearchPagingNum
	}

	searchMode := indexer.SearchModeType(ctx.FormString("search_mode"))

	searchRef := git.RefNameFromBranch(ctx.Repo.Repository.DefaultBranch)
	searchResults, total, err := gitgrep.PerformSearch(ctx, page, ctx.Repo.Repository.ID, ctx.Repo.GitRepo, searchRef, keyword, searchMode)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}

	items := make([]*api.CodeSearchResultFile, 0, len(searchResults))
	for _, result := range searchResults {
		lines := make([]*api.CodeSearchResultLine, 0, len(result.Lines))
		for _, line := range result.Lines {
			lines = append(lines, &api.CodeSearchResultLine{
				LineNumber: line.Num,
				Content:    string(line.FormattedContent),
			})
		}
		items = append(items, &api.CodeSearchResultFile{
			Filename: result.Filename,
			Lines:    lines,
		})
	}

	ctx.JSON(http.StatusOK, api.CodeSearchResponse{
		TotalCount: total,
		Items:      items,
	})
}
