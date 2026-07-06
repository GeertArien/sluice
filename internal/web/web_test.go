package web

import (
	"context"
	"crypto/ed25519"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

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
	ts, client, st, _ := setupKH(t)
	return ts, client, st
}

// setupKH also returns the managed known_hosts path for tests that exercise
// trusted-host management.
func setupKH(t *testing.T) (*httptest.Server, *http.Client, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(strings.Repeat("ef", 32))
	knownHosts := filepath.Join(dir, "known_hosts")
	svc := jobs.New(st, box, dir, knownHosts, 1)
	svc.NewGitea = func(string, string) jobs.GiteaAPI { return stubGitea{} }
	srv, err := NewServer(st, box, svc, "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar := newJar()
	client := &http.Client{Jar: jar}
	return ts, client, st, knownHosts
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

func TestGiteaTokenReuseAcrossBridges(t *testing.T) {
	ts, client, st := setup(t)
	csrf := login(t, ts, client)

	// Add a shared token on the tokens page.
	resp, err := client.PostForm(ts.URL+"/tokens", url.Values{
		"csrf": {csrf}, "name": {"shared-token"}, "token": {"s3cr3t-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tokBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("token create returned %d:\n%s", resp.StatusCode, tokBody)
	}
	tokens, _ := st.GiteaTokens()
	if len(tokens) != 1 || tokens[0].Name != "shared-token" {
		t.Fatalf("token not stored: %+v", tokens)
	}
	tok := tokens[0]
	// The tokens page shows the name but never the secret value.
	if strings.Contains(string(tokBody), "s3cr3t-value") {
		t.Fatal("token value leaked on the tokens page")
	}
	if !strings.Contains(string(tokBody), "shared-token") {
		t.Fatal("tokens page missing token name")
	}

	// Two bridges both select the shared token from the list.
	for _, slug := range []string{"alpha", "beta"} {
		resp, err := client.PostForm(ts.URL+"/bridges", url.Values{
			"csrf":              {csrf},
			"name":              {slug},
			"slug":              {slug},
			"source_remote_url": {"git@github.com:o/r.git"},
			"gitea_base_url":    {"http://192.168.1.50:3000"},
			"gitea_owner":       {"ai"},
			"gitea_repo":        {slug},
			"gitea_token_id":    {strconv.FormatInt(tok.ID, 10)},
			"excluded_paths":    {"secret"},
			"sync_branches":     {"main"},
		})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("create %s returned %d:\n%s", slug, resp.StatusCode, body)
		}
		b, err := st.BridgeBySlug(slug)
		if err != nil {
			t.Fatal(err)
		}
		if b.GiteaTokenID == nil || *b.GiteaTokenID != tok.ID {
			t.Fatalf("bridge %s not linked to the shared token: %v", slug, b.GiteaTokenID)
		}
		if len(b.GiteaTokenEnc) != 0 {
			t.Fatalf("bridge %s kept an inline token copy", slug)
		}
	}

	// The token was reused, not duplicated, and both bridges reference it.
	if tokens, _ := st.GiteaTokens(); len(tokens) != 1 {
		t.Fatalf("expected the token to be reused, got %d tokens", len(tokens))
	}
	if users, _ := st.BridgesUsingGiteaToken(tok.ID); len(users) != 2 {
		t.Fatalf("expected 2 bridges using the token, got %v", users)
	}

	// A token in use cannot be deleted.
	resp, err = client.PostForm(ts.URL+"/tokens/"+strconv.FormatInt(tok.ID, 10)+"/delete", url.Values{"csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	delBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(delBody), "cannot delete") {
		t.Fatal("expected in-use token deletion to be blocked")
	}
	if toks, _ := st.GiteaTokens(); len(toks) != 1 {
		t.Fatal("in-use token was deleted")
	}
}

func TestBridgeCreateSavesNewInlineToken(t *testing.T) {
	ts, client, st := setup(t)
	csrf := login(t, ts, client)

	// Pasting a new token in the bridge form saves it to the shared list.
	resp, err := client.PostForm(ts.URL+"/bridges", url.Values{
		"csrf":              {csrf},
		"name":              {"Fresh"},
		"slug":              {"fresh"},
		"source_remote_url": {"git@github.com:o/r.git"},
		"gitea_base_url":    {"http://192.168.1.50:3000"},
		"gitea_owner":       {"ai"},
		"gitea_repo":        {"fresh"},
		"new_token_name":    {"fresh-token"},
		"gitea_token":       {"paste-me"},
		"excluded_paths":    {"secret"},
		"sync_branches":     {"main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("create returned %d:\n%s", resp.StatusCode, body)
	}

	tokens, _ := st.GiteaTokens()
	if len(tokens) != 1 || tokens[0].Name != "fresh-token" {
		t.Fatalf("inline token not saved to the shared list: %+v", tokens)
	}
	b, err := st.BridgeBySlug("fresh")
	if err != nil {
		t.Fatal(err)
	}
	if b.GiteaTokenID == nil || *b.GiteaTokenID != tokens[0].ID {
		t.Fatalf("bridge not linked to the new token: %v", b.GiteaTokenID)
	}
	if len(b.GiteaTokenEnc) != 0 {
		t.Fatal("bridge kept an inline token copy instead of linking the shared token")
	}
	// The saved value is encrypted and round-trips to the pasted plaintext.
	box, _ := secrets.New(strings.Repeat("ef", 32))
	if v, _ := box.Decrypt(tokens[0].TokenEnc); v != "paste-me" {
		t.Fatalf("token value not stored correctly: %q", v)
	}
}

func TestBridgeCreateRequiresAToken(t *testing.T) {
	ts, client, _ := setup(t)
	csrf := login(t, ts, client)

	resp, err := client.PostForm(ts.URL+"/bridges", url.Values{
		"csrf":              {csrf},
		"name":              {"NoTok"},
		"slug":              {"notok"},
		"source_remote_url": {"git@github.com:o/r.git"},
		"gitea_base_url":    {"http://192.168.1.50:3000"},
		"gitea_owner":       {"ai"},
		"gitea_repo":        {"notok"},
		"excluded_paths":    {"secret"},
		"sync_branches":     {"main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Gitea API token is required") {
		t.Fatalf("expected a token-required error, got:\n%s", body)
	}
}

func TestTrustedHostsScanTrustAndDelete(t *testing.T) {
	ts, client, st, knownHosts := setupKH(t)
	csrf := login(t, ts, client)

	port, fp := startTestSSHServer(t)

	// Scan the local server — the result page shows the fingerprint.
	resp, err := client.PostForm(ts.URL+"/hosts/scan", url.Values{
		"csrf": {csrf}, "host": {"127.0.0.1"}, "port": {strconv.Itoa(port)},
	})
	if err != nil {
		t.Fatal(err)
	}
	scanBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// Unescape: a base64 fingerprint may contain '+', rendered as &#43;.
	if !strings.Contains(html.UnescapeString(string(scanBody)), fp) {
		t.Fatalf("scan page missing fingerprint %s:\n%s", fp, scanBody)
	}

	// Extract the hidden known_hosts line and trust it.
	line := extractHidden(t, string(scanBody), "line")
	resp, err = client.PostForm(ts.URL+"/hosts", url.Values{"csrf": {csrf}, "line": {line}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Stored, and the managed known_hosts file now contains the line.
	hosts, _ := st.HostKeys()
	if len(hosts) != 1 || hosts[0].Fingerprint != fp {
		t.Fatalf("host key not stored: %+v", hosts)
	}
	data, err := os.ReadFile(knownHosts)
	if err != nil || !strings.Contains(string(data), "127.0.0.1") {
		t.Fatalf("known_hosts not rendered: err=%v contents=%q", err, data)
	}
	if !strings.Contains(html.UnescapeString(string(body)), fp) {
		t.Fatal("hosts page does not list the trusted key")
	}

	// Delete removes it from the DB and the file.
	resp, err = client.PostForm(ts.URL+"/hosts/"+strconv.FormatInt(hosts[0].ID, 10)+"/delete", url.Values{"csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hs, _ := st.HostKeys(); len(hs) != 0 {
		t.Fatal("host key not deleted")
	}
	data, _ = os.ReadFile(knownHosts)
	if strings.Contains(string(data), "127.0.0.1") {
		t.Fatalf("known_hosts still contains deleted host:\n%s", data)
	}
}

func extractHidden(t *testing.T, page, name string) string {
	t.Helper()
	marker := `name="` + name + `" value="`
	i := strings.Index(page, marker)
	if i < 0 {
		t.Fatalf("hidden field %q not found", name)
	}
	rest := page[i+len(marker):]
	end := strings.IndexByte(rest, '"')
	// Reverse html/template attribute escaping (e.g. base64 '+' -> &#43;).
	return html.UnescapeString(rest[:end])
}

// startTestSSHServer launches a minimal SSH server with a fixed ed25519 host
// key that refuses auth; returns its port and the host key's SHA256 fingerprint.
func startTestSSHServer(t *testing.T) (int, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, ssh.ErrNoAuth
		},
	}
	cfg.AddHostKey(signer)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				if conn, chans, reqs, err := ssh.NewServerConn(c, cfg); err == nil {
					go ssh.DiscardRequests(reqs)
					for ch := range chans {
						_ = ch.Reject(ssh.Prohibited, "no")
					}
					conn.Close()
				}
				c.Close()
			}(c)
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return port, ssh.FingerprintSHA256(signer.PublicKey())
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
