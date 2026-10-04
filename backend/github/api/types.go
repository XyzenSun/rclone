// Package api has type definitions for the GitHub REST API used by the github backend
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Entry is a single item returned by the contents API.
//
// GET /repos/{owner}/{repo}/contents/{path} returns an Entry for a
// file path and a []Entry in the Entries field for a directory path.
// The same shape is also used for the individual directory entries.
type Entry struct {
	Type            string  `json:"type"` // "file", "dir", "symlink", "submodule" or "executable"
	Encoding        string  `json:"encoding,omitempty"`
	Size            int64   `json:"size"`
	Name            string  `json:"name"`
	Path            string  `json:"path"`
	Content         string  `json:"content,omitempty"`
	Sha             string  `json:"sha"`
	URL             string  `json:"url"`
	GitURL          string  `json:"git_url"`
	HTMLURL         string  `json:"html_url"`
	DownloadURL     string  `json:"download_url"`
	Entries         []Entry `json:"entries,omitempty"`
	SubmoduleGitURL string  `json:"submodule_git_url,omitempty"`
	Target          string  `json:"target,omitempty"`
}

// IsDir reports whether the entry is a directory
func (e *Entry) IsDir() bool {
	return e.Type == "dir"
}

// IsSubmodule reports whether the entry is a git submodule
func (e *Entry) IsSubmodule() bool {
	return e.Type == "submodule"
}

// IsFile reports whether the entry is a regular file.
//
// "symlink" and "executable" entries are treated as files: they are
// returned by the contents API for symlinks and executable files
// respectively and both are downloadable blobs.
func (e *Entry) IsFile() bool {
	switch e.Type {
	case "file", "symlink", "executable":
		return true
	}
	return false
}

// TreeEntry is one node of a git tree object.
//
// It is used both as a request item of TreeRequest (Sha set to nil
// deletes the entry, Content instead of Sha creates a blob inline)
// and as a response item of TreeResponse (Size and URL only appear
// in responses).
type TreeEntry struct {
	Path    string      `json:"path"`
	Mode    string      `json:"mode"`              // "100644" blob, "100755" executable, "040000" tree, "120000" symlink, "160000" commit
	Type    string      `json:"type"`              // "blob", "tree" or "commit"
	Sha     interface{} `json:"sha"`               // string sha, or nil to delete the entry
	Content *string     `json:"content,omitempty"` // inline blob content, alternative to Sha in requests
	Size    int64       `json:"size,omitempty"`
	URL     string      `json:"url,omitempty"`
}

// MarshalJSON 按 GitHub trees API 的约束序列化:
// content 条目必须完全省略 sha 字段(显式 null 会被 422 拒绝),
// 删除条目必须显式输出 "sha": null(完全省略会被当作错误而非删除)
func (t TreeEntry) MarshalJSON() ([]byte, error) {
	if t.Content != nil {
		return json.Marshal(struct {
			Path    string  `json:"path"`
			Mode    string  `json:"mode"`
			Type    string  `json:"type"`
			Content *string `json:"content"`
		}{Path: t.Path, Mode: t.Mode, Type: t.Type, Content: t.Content})
	}
	return json.Marshal(struct {
		Path string      `json:"path"`
		Mode string      `json:"mode"`
		Type string      `json:"type"`
		Sha  interface{} `json:"sha"`
	}{Path: t.Path, Mode: t.Mode, Type: t.Type, Sha: t.Sha})
}

// TreeResponse is the response of the git trees API
//
// GET /repos/{owner}/{repo}/git/trees/{sha} and
// POST /repos/{owner}/{repo}/git/trees
type TreeResponse struct {
	Sha       string      `json:"sha"`
	URL       string      `json:"url"`
	Tree      []TreeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

// TreeRequest is the request body of POST /repos/{owner}/{repo}/git/trees
type TreeRequest struct {
	BaseTree string      `json:"base_tree,omitempty"`
	Tree     []TreeEntry `json:"tree"`
}

// BlobResponse is the response of POST /repos/{owner}/{repo}/git/blobs
type BlobResponse struct {
	Sha string `json:"sha"`
	URL string `json:"url"`
}

// Committer identifies the author or committer of a commit
type Committer struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// CommitRequest is the request body of POST /repos/{owner}/{repo}/git/commits
type CommitRequest struct {
	Message   string     `json:"message"`
	Tree      string     `json:"tree"`
	Parents   []string   `json:"parents"`
	Committer *Committer `json:"committer,omitempty"`
	Author    *Committer `json:"author,omitempty"`
}

// CommitResponse is the response of POST /repos/{owner}/{repo}/git/commits
type CommitResponse struct {
	Sha string `json:"sha"`
}

// UpdateRefRequest is the request body of
// PATCH /repos/{owner}/{repo}/git/refs/{ref}
type UpdateRefRequest struct {
	Sha   string `json:"sha"`
	Force bool   `json:"force"`
}

// CreateRefRequest is the request body of POST /repos/{owner}/{repo}/git/refs,
// used to create the branch ref when committing to an empty repository
type CreateRefRequest struct {
	Ref string `json:"ref"` // full ref name, e.g. "refs/heads/main"
	Sha string `json:"sha"`
}

// BranchResponse is the response of
// GET /repos/{owner}/{repo}/branches/{branch}
type BranchResponse struct {
	Name   string         `json:"name"`
	Commit CommitResponse `json:"commit"`
}

// RepoResponse is the subset of GET /repos/{owner}/{repo} that we use
type RepoResponse struct {
	DefaultBranch string `json:"default_branch"`
}

// ErrorResponse is the standard GitHub API error body
type ErrorResponse struct {
	Message          string `json:"message"`
	DocumentationURL string `json:"documentation_url"`
	Status           string `json:"status"`
}

// Error implements the error interface for ErrorResponse
func (e *ErrorResponse) Error() string {
	if e.Status != "" {
		return fmt.Sprintf("github api error %s: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("github api error: %s", e.Message)
}

// MtimeRequest is the request body of the GraphQL API used to fetch
// the accurate last commit time of files
type MtimeRequest struct {
	Query string `json:"query"`
}

// MtimeResponse is the subset of the GraphQL response that we use.
//
// The commit history of each queried path is returned as
// data.repository.commit[alias].nodes[0].committedDate where alias is
// the "pN" alias used in the query.
type MtimeResponse struct {
	Data struct {
		Repository struct {
			Commit map[string]struct {
				Nodes []struct {
					CommittedDate time.Time `json:"committedDate"`
				} `json:"nodes"`
			} `json:"commit"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// ParseError extracts a meaningful error from an HTTP response with a
// GitHub error body, falling back to the status line if the body does
// not parse
func ParseError(resp *http.Response) error {
	apiErr := &ErrorResponse{Status: resp.Status}
	if resp.Body != nil {
		defer func() {
			_ = resp.Body.Close()
		}()
		if err := json.NewDecoder(resp.Body).Decode(apiErr); err != nil {
			return fmt.Errorf("github api error: %s", resp.Status)
		}
	}
	return apiErr
}
