package web

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/geertarien/sluice/internal/gitea"
	"github.com/geertarien/sluice/internal/jobs"
	"github.com/geertarien/sluice/internal/secrets"
	"github.com/geertarien/sluice/internal/store"
)

type stubGitea struct{}

func (stubGitea) EnsureRepo(ctx context.Context, o, r string) (*gitea.Repo, error) {
	return &gitea.Repo{}, nil
}
func (stubGitea) CheckToken(ctx context.Context) error { return nil }
func (stubGitea) OpenPRs(ctx context.Context, o, r string) ([]gitea.PR, error) {
	pr := gitea.PR{Number: 3, Title: "Add feature", Mergeable: true}
	pr.Head.Ref = "feat"
	pr.Base.Ref = "main"
	pr.User.Login = "agent"
	return []gitea.PR{pr}, nil
}
func (stubGitea) FindOpenPRByHead(ctx context.Context, o, r, b string) (*gitea.PR, error) {
	return nil, nil
}
func (stubGitea) ClosePR(ctx context.Context, o, r string, i int64) error               { return nil }
func (stubGitea) CommentOnPR(ctx context.Context, o, r string, i int64, b string) error { return nil }
func (stubGitea) DeleteBranch(ctx context.Context, o, r, b string) error                { return nil }

func setup(t *testing.T) (*httptest.Server, *http.Client, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(strings.Repeat("ef", 32))
	svc := jobs.New(st, box, t.TempDir(), "", 1)
	svc.NewGitea = func(string, string) jobs.GiteaAPI { return stubGitea{} }
	srv, err := NewServer(st, box, svc, "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar := newJar()
	client := &http.Client{Jar: jar}
	return ts, client, st
}

type jarT struct{ cookies map[string][]*http.Cookie }

func newJar() *jarT { return &jarT{cookies: map[string][]*http.Cookie{}} }
func (j *jarT) SetCookies(u *url.URL, cs []*http.Cookie) {
	j.cookies[u.Host] = append(j.cookies[u.Host], cs...)
}
func (j *jarT) Cookies(u *url.URL) []*http.Cookie { return j.cookies[u.Host] }

func login(t *testing.T, ts *httptest.Server, c *http.Client) string {
	t.Helper()
	resp, err := c.PostForm(ts.URL+"/login", url.Values{"password": {"hunter2"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}
	// Pull the CSRF token out of the dashboard's logout form.
	html := string(body)
	idx := strings.Index(html, `name="csrf" value="`)
	if idx < 0 {
		t.Fatalf("no csrf token on dashboard:\n%s", html)
	}
	tok := html[idx+len(`name="csrf" value="`):]
	return tok[:strings.Index(tok, `"`)]
}

func get(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPagesRenderAndAuthIsEnforced(t *testing.T) {
	ts, client, st := setup(t)

	// Unauthenticated requests are redirected to /login.
	noRedir := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noRedir.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected redirect for anonymous user, got %d", resp.StatusCode)
	}

	csrf := login(t, ts, client)

	// Seed data covering all template branches.
	box, _ := secrets.New(strings.Repeat("ef", 32))
	tokEnc, _ := box.Encrypt("tkn")
	whEnc, _ := box.Encrypt("whsec")
	ok := true
	b := &store.Bridge{
		Name: "Demo", Slug: "demo", SourceRemoteURL: "git@github.com:o/r.git",
		GiteaBaseURL: "http://g", GiteaOwner: "ai", GiteaRepo: "demo", GiteaSSHURL: "git@g:ai/demo.git",
		GiteaTokenEnc: tokEnc, WebhookSecretEnc: whEnc,
		ExcludedPaths: []string{"secret"}, SyncBranches: []string{"main"},
		TripwireStrings: []string{"CODENAME"}, PromoteName: "Bot", PromoteEmail: "bot@x",
		Status: "active", LastVerifyOK: &ok,
	}
	if err := st.CreateBridge(b); err != nil {
		t.Fatal(err)
	}
	jobID, _ := st.EnqueueJob(b.ID, "sync", nil)
	pr := int64(3)
	_ = st.CreatePromotion(&store.Promotion{
		BridgeID: b.ID, GiteaBranch: "feat", GiteaPRNumber: &pr, RealBranch: "ai/feat",
		RealTipSHA: strings.Repeat("a", 40), BaseBranch: "main", Status: "promoted",
	})
	_ = st.CreatePromotion(&store.Promotion{
		BridgeID: b.ID, GiteaBranch: "broken", RealBranch: "ai/broken",
		BaseBranch: "main", Status: "conflict",
	})
	st.Audit(b.ID, "admin", "test_entry", map[string]string{"k": "v"})

	pages := []string{"/", "/bridges/new", "/bridges/demo", "/bridges/demo/settings",
		"/audit", "/audit?bridge=demo&action=test_entry", "/jobs/1",
		// No gitea-clone exists in the temp workdir, so this exercises the
		// preflight template's error branch.
		"/bridges/demo/preflight?branch=feat&base=main"}
	for _, p := range pages {
		code, body := get(t, client, ts.URL+p)
		if code != 200 {
			t.Errorf("GET %s = %d:\n%s", p, code, body)
		}
	}
	// Settings page must show the webhook secret, never the token.
	_, body := get(t, client, ts.URL+"/bridges/demo/settings")
	if !strings.Contains(body, "whsec") {
		t.Error("webhook secret not shown on settings page")
	}
	if strings.Contains(body, "tkn") {
		t.Error("gitea token leaked into settings page")
	}

	// POST without CSRF fails; with CSRF it succeeds.
	resp, err = client.PostForm(ts.URL+"/bridges/demo/sync", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without CSRF should 403, got %d", resp.StatusCode)
	}
	resp, err = client.PostForm(ts.URL+"/bridges/demo/sync", url.Values{"csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 { // redirected to the job page
		t.Fatalf("POST with CSRF failed: %d", resp.StatusCode)
	}

	// Job log endpoint serves text + status header.
	resp, err = client.Get(ts.URL + "/jobs/" + strconv.FormatInt(jobID, 10) + "/log")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Job-Status") == "" {
		t.Error("job log missing status header")
	}
}

func TestGenerateKeyThenSelectOnBridgeSkipsAutoInit(t *testing.T) {
	ts, client, st := setup(t)
	csrf := login(t, ts, client)

	// Generate a named key on the keys page.
	resp, err := client.PostForm(ts.URL+"/keys", url.Values{
		"csrf": {csrf}, "name": {"shared-deploy"}, "mode": {"generate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	keysBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("key create returned %d:\n%s", resp.StatusCode, keysBody)
	}
	keys, _ := st.SSHKeys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0].PublicKey, "ssh-ed25519 ") {
		t.Fatalf("named key not stored: %+v", keys)
	}
	key := keys[0]
	// Keys page shows the public key but never the private key material.
	// (HTML-unescape first: html/template encodes '+' as &#43; etc.)
	keysText := html.UnescapeString(string(keysBody))
	if !strings.Contains(keysText, key.PublicKey) {
		t.Fatal("keys page missing public key")
	}
	priv, _ := secrets.New(strings.Repeat("ef", 32))
	if pk, _ := priv.Decrypt(key.PrivateKeyEnc); pk == "" || strings.Contains(keysText, pk) {
		t.Fatal("private key leaked on keys page")
	}

	// Create a bridge that selects the named key.
	resp, err = client.PostForm(ts.URL+"/bridges", url.Values{
		"csrf":              {csrf},
		"name":              {"Keyed"},
		"slug":              {"keyed"},
		"source_remote_url": {"git@github.com:o/r.git"},
		"gitea_base_url":    {"http://192.168.1.50:3000"},
		"gitea_owner":       {"ai"},
		"gitea_repo":        {"keyed"},
		"gitea_token":       {"tok"},
		"excluded_paths":    {"secret"},
		"sync_branches":     {"main"},
		"ssh_key_id":        {strconv.FormatInt(key.ID, 10)},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("create returned %d:\n%s", resp.StatusCode, body)
	}
	// Lands on the bridge page (managed key), not a job page.
	if !strings.Contains(resp.Request.URL.Path, "/bridges/keyed") {
		t.Fatalf("expected redirect to bridge page, got %s", resp.Request.URL.Path)
	}

	b, err := st.BridgeBySlug("keyed")
	if err != nil {
		t.Fatal(err)
	}
	if b.SSHKeyID == nil || *b.SSHKeyID != key.ID {
		t.Fatalf("bridge not linked to the named key: %v", b.SSHKeyID)
	}
	// No init job — the key must be registered first.
	jobsList, _ := st.JobsForBridge(b.ID, 10)
	if len(jobsList) != 0 {
		t.Fatalf("expected no auto-init job, got %d", len(jobsList))
	}
	// Bridge page shows the key name, its public key, and a Run init action.
	bridgeText := html.UnescapeString(string(body))
	if !strings.Contains(bridgeText, key.PublicKey) || !strings.Contains(bridgeText, "Run init") || !strings.Contains(bridgeText, "shared-deploy") {
		t.Fatal("bridge page missing key details or Run init button")
	}

	// A key in use cannot be deleted.
	resp, err = client.PostForm(ts.URL+"/keys/"+strconv.FormatInt(key.ID, 10)+"/delete", url.Values{"csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	delBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(delBody), "cannot delete") {
		t.Fatal("expected in-use key deletion to be blocked")
	}
	if keys, _ := st.SSHKeys(); len(keys) != 1 {
		t.Fatal("in-use key was deleted")
	}
}

func TestWebhookSecretAuth(t *testing.T) {
	ts, _, st := setup(t)
	box, _ := secrets.New(strings.Repeat("ef", 32))
	whEnc, _ := box.Encrypt("hooksecret")
	b := &store.Bridge{
		Name: "Hooked", Slug: "hooked", SourceRemoteURL: "/s", GiteaBaseURL: "http://g",
		GiteaOwner: "o", GiteaRepo: "r", GiteaSSHURL: "/g",
		WebhookSecretEnc: whEnc, SyncBranches: []string{"main"},
		ExcludedPaths: []string{"x"}, Status: "active",
	}
	if err := st.CreateBridge(b); err != nil {
		t.Fatal(err)
	}
	post := func(secret string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/hooks/hooked", nil)
		if secret != "" {
			req.Header.Set("X-Sluice-Secret", secret)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(""); code != http.StatusForbidden {
		t.Fatalf("missing secret should 403, got %d", code)
	}
	if code := post("wrong"); code != http.StatusForbidden {
		t.Fatalf("wrong secret should 403, got %d", code)
	}
	if code := post("hooksecret"); code != http.StatusAccepted {
		t.Fatalf("correct secret should 202, got %d", code)
	}
}
