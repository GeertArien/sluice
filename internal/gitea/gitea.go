// Package gitea is a minimal client for the Gitea API calls Sluice uses
// (spec §12.4). Auth is "Authorization: token <token>".
package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	BaseURL string // e.g. https://gitea.local
	Token   string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/api/v1"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		// Never echo the token; data is the server's response body.
		return fmt.Errorf("gitea %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type Repo struct {
	Name    string `json:"name"`
	Private bool   `json:"private"`
	SSHURL  string `json:"ssh_url"`
}

// EnsureRepo returns the repo, creating it (private) if missing. It tries
// the org endpoint first and falls back to the user endpoint (spec §12.4).
func (c *Client) EnsureRepo(ctx context.Context, owner, repo string) (*Repo, error) {
	var r Repo
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo)), nil, &r)
	if err == nil {
		return &r, nil
	}
	create := map[string]any{"name": repo, "private": true, "auto_init": false}
	if orgErr := c.do(ctx, "POST", fmt.Sprintf("/orgs/%s/repos", url.PathEscape(owner)), create, &r); orgErr == nil {
		return &r, nil
	}
	// Owner is not an org we can create under; try as the token's user.
	if userErr := c.do(ctx, "POST", "/user/repos", create, &r); userErr == nil {
		return &r, nil
	} else {
		return nil, fmt.Errorf("repo %s/%s not found and could not be created: %w", owner, repo, userErr)
	}
}

// CheckToken verifies the token works at all.
func (c *Client) CheckToken(ctx context.Context) error {
	return c.do(ctx, "GET", "/user", nil, nil)
}

type PR struct {
	Number    int64  `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Mergeable bool   `json:"mergeable"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	HTMLURL string `json:"html_url"`
}

func (c *Client) OpenPRs(ctx context.Context, owner, repo string) ([]PR, error) {
	var prs []PR
	err := c.do(ctx, "GET",
		fmt.Sprintf("/repos/%s/%s/pulls?state=open&limit=50", url.PathEscape(owner), url.PathEscape(repo)),
		nil, &prs)
	return prs, err
}

// OpenPRCount returns the number of open pull requests. It uses the issues
// endpoint with type=pulls, which — unlike the pulls list — does not compute
// ahead/behind per PR, so it stays fast on large repos with many open PRs
// (the pulls list is O(n) git rev-list calls on Forgejo). It reads the
// X-Total-Count header, falling back to counting the returned page.
func (c *Client) OpenPRCount(ctx context.Context, owner, repo string) (int, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues?state=open&type=pulls&limit=50",
		url.PathEscape(owner), url.PathEscape(repo))
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/api/v1"+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "token "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("gitea GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(data)))
	}
	if tc := strings.TrimSpace(resp.Header.Get("X-Total-Count")); tc != "" {
		if n, err := strconv.Atoi(tc); err == nil {
			return n, nil
		}
	}
	var items []json.RawMessage
	_ = json.Unmarshal(data, &items)
	return len(items), nil
}

// Version returns the forge's reported version string (e.g. "1.26.1" for Gitea
// or "15.0.7+gitea-1.22.0" for Forgejo). Display only — behaviour never branches
// on it, since both speak the same Gitea-compatible API.
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	if err := c.do(ctx, "GET", "/version", nil, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// FindOpenPRByHead returns the open PR whose head branch matches, or nil. When
// the base branch is known and neither ref contains a slash, it uses the direct
// GET /pulls/{base}/{head} lookup (one cheap request); otherwise it pages the
// open-PR list until a match or exhaustion, so mirrors with >50 open PRs don't
// silently lose matches (the fixed limit=50 list would).
func (c *Client) FindOpenPRByHead(ctx context.Context, owner, repo, base, branch string) (*PR, error) {
	if base != "" && !strings.Contains(base, "/") && !strings.Contains(branch, "/") {
		pr, err := c.pullByBaseHead(ctx, owner, repo, base, branch)
		if err != nil {
			return nil, err
		}
		// The base/head pair is authoritative: a closed hit or a 404 both mean
		// there is no open PR for this promotion's base and head.
		if pr != nil && pr.State == "open" {
			return pr, nil
		}
		return nil, nil
	}
	return c.findOpenPRByHeadPaged(ctx, owner, repo, branch)
}

// pullByBaseHead fetches the single PR for a base/head pair, returning nil on
// 404 (no such PR) rather than an error.
func (c *Client) pullByBaseHead(ctx context.Context, owner, repo, base, head string) (*PR, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%s/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(base), url.PathEscape(head))
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/api/v1"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gitea GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(data)))
	}
	var pr PR
	if err := json.Unmarshal(data, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// findOpenPRByHeadPaged scans the open-PR list page by page until it finds a
// matching head or runs out, instead of a fixed limit=50.
func (c *Client) findOpenPRByHeadPaged(ctx context.Context, owner, repo, branch string) (*PR, error) {
	const perPage = 50
	for page := 1; ; page++ {
		var prs []PR
		path := fmt.Sprintf("/repos/%s/%s/pulls?state=open&limit=%d&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), perPage, page)
		if err := c.do(ctx, "GET", path, nil, &prs); err != nil {
			return nil, err
		}
		for i := range prs {
			if prs[i].Head.Ref == branch {
				return &prs[i], nil
			}
		}
		if len(prs) < perPage {
			return nil, nil
		}
	}
}

func (c *Client) ClosePR(ctx context.Context, owner, repo string, index int64) error {
	return c.do(ctx, "PATCH",
		fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repo), index),
		map[string]string{"state": "closed"}, nil)
}

func (c *Client) CommentOnPR(ctx context.Context, owner, repo string, index int64, body string) error {
	return c.do(ctx, "POST",
		fmt.Sprintf("/repos/%s/%s/issues/%d/comments", url.PathEscape(owner), url.PathEscape(repo), index),
		map[string]string{"body": body}, nil)
}

func (c *Client) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	return c.do(ctx, "DELETE",
		fmt.Sprintf("/repos/%s/%s/branches/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch)),
		nil, nil)
}
