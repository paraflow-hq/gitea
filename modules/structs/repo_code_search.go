// Copyright 2025 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package structs

// CodeSearchResultLine represents a single matched line in code search
type CodeSearchResultLine struct {
	LineNumber int    `json:"line_number"`
	Content    string `json:"content"`
}

// CodeSearchResultFile represents a file with matched lines in code search
type CodeSearchResultFile struct {
	Filename string                  `json:"filename"`
	Lines    []*CodeSearchResultLine `json:"lines"`
}

// CodeSearchResponse represents the response of a code search
// swagger:model
type CodeSearchResponse struct {
	TotalCount int                     `json:"total_count"`
	Items      []*CodeSearchResultFile `json:"items"`
}
