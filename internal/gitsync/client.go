// Package gitsync pulls GitHub activity (pull requests, issues, commits) into
// WorkPilot as project progress signals.
package gitsync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the public GitHub REST API.
	DefaultBaseURL = "https://api.github.com"

	perPage           = 100
	maxPages          = 5
	maxTitleRunes     = 500
	maxNameRunes      = 100
	maxErrorBodyRunes = 200
)

// Item is one remote activity record.
type Item struct {
	Kind       string
	ExternalID string
	Number     *int
	Title      string
	State      string
	Author     string
	URL        string
	UpdatedAt  *time.Time
}

// ErrBlobTooLarge signals that a fetched blob exceeds the configured limit.
var ErrBlobTooLarge = errors.New("github blob exceeds size limit")

// TreeEntry is one path in a repository tree.
type TreeEntry struct {
	Path string
	Type string
	SHA  string
	Size int64
}

// TreeResult is one recursive tree response.
type TreeResult struct {
	Entries     []TreeEntry
	ETag        string
	Truncated   bool
	NotModified bool
}

// RepoInfo carries the repository metadata needed to resolve the default ref.
type RepoInfo struct {
	DefaultBranch string
}

// Client is a minimal GitHub REST API v3 client.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL, token string, timeout time.Duration) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL: baseURL,
		token:   strings.TrimSpace(token),
		http:    &http.Client{Timeout: timeout},
	}
}

type ghUser struct {
	Login string `json:"login"`
}

type ghPull struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	User      ghUser     `json:"user"`
	HTMLURL   string     `json:"html_url"`
	UpdatedAt time.Time  `json:"updated_at"`
	MergedAt  *time.Time `json:"merged_at"`
	Draft     bool       `json:"draft"`
}

type ghIssue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	State       string    `json:"state"`
	User        ghUser    `json:"user"`
	HTMLURL     string    `json:"html_url"`
	UpdatedAt   time.Time `json:"updated_at"`
	PullRequest *struct{} `json:"pull_request"`
}

type ghCommit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Message string `json:"message"`
		Author  struct {
			Name string    `json:"name"`
			Date time.Time `json:"date"`
		} `json:"author"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
	Author *ghUser `json:"author"`
}

// PullRequests lists recently updated pull requests (state=all).
func (c *Client) PullRequests(ctx context.Context, owner, repo string, since time.Time, maxItems int) ([]Item, error) {
	items := make([]Item, 0)
	path := fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(repo))
	for page := 1; page <= maxPages; page++ {
		query := url.Values{
			"state":     {"all"},
			"sort":      {"updated"},
			"direction": {"desc"},
			"per_page":  {strconv.Itoa(perPage)},
			"page":      {strconv.Itoa(page)},
		}
		var batch []ghPull
		if err := c.get(ctx, path, query, &batch); err != nil {
			return nil, err
		}
		for _, pr := range batch {
			if pr.UpdatedAt.Before(since) {
				return items, nil
			}
			state := pr.State
			if pr.MergedAt != nil {
				state = "merged"
			}
			updated := pr.UpdatedAt
			number := pr.Number
			items = append(items, Item{
				Kind:       "pull_request",
				ExternalID: strconv.Itoa(pr.Number),
				Number:     &number,
				Title:      truncateRunes(pr.Title, maxTitleRunes),
				State:      state,
				Author:     truncateRunes(pr.User.Login, maxNameRunes),
				URL:        pr.HTMLURL,
				UpdatedAt:  &updated,
			})
			if maxItems > 0 && len(items) >= maxItems {
				return items, nil
			}
		}
		if len(batch) < perPage {
			break
		}
	}
	return items, nil
}

// Issues lists recently updated issues; entries that are pull requests are
// skipped because GitHub mixes both into this endpoint.
func (c *Client) Issues(ctx context.Context, owner, repo string, since time.Time, maxItems int) ([]Item, error) {
	items := make([]Item, 0)
	path := fmt.Sprintf("/repos/%s/%s/issues", url.PathEscape(owner), url.PathEscape(repo))
	for page := 1; page <= maxPages; page++ {
		query := url.Values{
			"state":     {"all"},
			"sort":      {"updated"},
			"direction": {"desc"},
			"per_page":  {strconv.Itoa(perPage)},
			"page":      {strconv.Itoa(page)},
			"since":     {since.UTC().Format(time.RFC3339)},
		}
		var batch []ghIssue
		if err := c.get(ctx, path, query, &batch); err != nil {
			return nil, err
		}
		for _, is := range batch {
			if is.PullRequest != nil || is.UpdatedAt.Before(since) {
				continue
			}
			updated := is.UpdatedAt
			number := is.Number
			items = append(items, Item{
				Kind:       "issue",
				ExternalID: strconv.Itoa(is.Number),
				Number:     &number,
				Title:      truncateRunes(is.Title, maxTitleRunes),
				State:      is.State,
				Author:     truncateRunes(is.User.Login, maxNameRunes),
				URL:        is.HTMLURL,
				UpdatedAt:  &updated,
			})
			if maxItems > 0 && len(items) >= maxItems {
				return items, nil
			}
		}
		if len(batch) < perPage {
			break
		}
	}
	return items, nil
}

// Commits lists commits authored since the given instant.
func (c *Client) Commits(ctx context.Context, owner, repo string, since time.Time, maxItems int) ([]Item, error) {
	items := make([]Item, 0)
	path := fmt.Sprintf("/repos/%s/%s/commits", url.PathEscape(owner), url.PathEscape(repo))
	for page := 1; page <= maxPages; page++ {
		query := url.Values{
			"since":    {since.UTC().Format(time.RFC3339)},
			"per_page": {strconv.Itoa(perPage)},
			"page":     {strconv.Itoa(page)},
		}
		var batch []ghCommit
		if err := c.get(ctx, path, query, &batch); err != nil {
			return nil, err
		}
		for _, commit := range batch {
			author := commit.Commit.Author.Name
			if commit.Author != nil && commit.Author.Login != "" {
				author = commit.Author.Login
			}
			updated := commit.Commit.Committer.Date
			if updated.IsZero() {
				updated = commit.Commit.Author.Date
			}
			var updatedPtr *time.Time
			if !updated.IsZero() {
				updatedPtr = &updated
			}
			items = append(items, Item{
				Kind:       "commit",
				ExternalID: truncateRunes(commit.SHA, 100),
				Title:      truncateRunes(firstLine(commit.Commit.Message), maxTitleRunes),
				Author:     truncateRunes(author, maxNameRunes),
				URL:        commit.HTMLURL,
				UpdatedAt:  updatedPtr,
			})
			if maxItems > 0 && len(items) >= maxItems {
				return items, nil
			}
		}
		if len(batch) < perPage {
			break
		}
	}
	return items, nil
}

// Repo resolves repository metadata such as its default branch.
func (c *Client) Repo(ctx context.Context, owner, repo string) (RepoInfo, error) {
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	var body struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.get(ctx, path, nil, &body); err != nil {
		return RepoInfo{}, err
	}
	return RepoInfo{DefaultBranch: body.DefaultBranch}, nil
}

// Tree lists the recursive file tree of a ref. When etag is non-empty and the
// remote returns 304 Not Modified, NotModified is set and Entries is empty.
func (c *Client) Tree(ctx context.Context, owner, repo, ref, etag string) (TreeResult, error) {
	path := fmt.Sprintf("/repos/%s/%s/git/trees/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref))
	req, err := c.newRequest(ctx, path, url.Values{"recursive": {"1"}})
	if err != nil {
		return TreeResult{}, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.do(req, path)
	if err != nil {
		return TreeResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return TreeResult{ETag: etag, NotModified: true}, nil
	}
	if err := checkStatus(resp, path); err != nil {
		return TreeResult{}, err
	}
	var body struct {
		Tree      []ghTreeEntry `json:"tree"`
		Truncated bool          `json:"truncated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return TreeResult{}, fmt.Errorf("github %s: decode response: %w", path, err)
	}
	entries := make([]TreeEntry, 0, len(body.Tree))
	for _, entry := range body.Tree {
		entries = append(entries, TreeEntry{
			Path: strings.TrimPrefix(entry.Path, "/"),
			Type: entry.Type,
			SHA:  entry.SHA,
			Size: entry.Size,
		})
	}
	return TreeResult{
		Entries:   entries,
		ETag:      strings.TrimSpace(resp.Header.Get("ETag")),
		Truncated: body.Truncated,
	}, nil
}

// Blob downloads and decodes a blob, rejecting decoded content above maxBytes.
func (c *Client) Blob(ctx context.Context, owner, repo, sha string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = 50 << 20
	}
	path := fmt.Sprintf("/repos/%s/%s/git/blobs/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha))
	req, err := c.newRequest(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req, path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp, path); err != nil {
		return nil, err
	}

	limit := maxBytes*2 + 4096
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("github %s: read response: %w", path, err)
	}
	if int64(len(raw)) >= limit {
		return nil, ErrBlobTooLarge
	}
	var body struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("github %s: decode response: %w", path, err)
	}
	if strings.EqualFold(body.Encoding, "base64") {
		content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body.Content, "\n", ""))
		if err != nil {
			return nil, fmt.Errorf("github %s: decode base64: %w", path, err)
		}
		if int64(len(content)) > maxBytes {
			return nil, ErrBlobTooLarge
		}
		return content, nil
	}
	content := []byte(body.Content)
	if int64(len(content)) > maxBytes {
		return nil, ErrBlobTooLarge
	}
	return content, nil
}

type ghTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	req, err := c.newRequest(ctx, path, query)
	if err != nil {
		return err
	}
	resp, err := c.do(req, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp, path); err != nil {
		return err
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("github %s: decode response: %w", path, err)
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, path string, query url.Values) (*http.Request, error) {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "workpilot-gitsync")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

func (c *Client) do(req *http.Request, path string) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github %s: %w", path, err)
	}
	return resp, nil
}

func checkStatus(resp *http.Response, path string) error {
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("github %s: status %d: %s", path, resp.StatusCode, truncateRunes(oneLine(string(body)), maxErrorBodyRunes))
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
