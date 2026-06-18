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

// FindOpenPRByHead returns the open PR whose head branch matches, or nil.
func (c *Client) FindOpenPRByHead(ctx context.Context, owner, repo, branch string) (*PR, error) {
	prs, err := c.OpenPRs(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	for i := range prs {
		if prs[i].Head.Ref == branch {
			return &prs[i], nil
		}
	}
	return nil, nil
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
