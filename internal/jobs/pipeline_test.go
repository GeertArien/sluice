package jobs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geertarien/sluice/internal/gitea"
	"github.com/geertarien/sluice/internal/secrets"
	"github.com/geertarien/sluice/internal/store"
)

// fakeGitea records API calls; the git side of "Gitea" is a bare repo on disk.
type fakeGitea struct {
	prs             []gitea.PR
	comments        []string
	closedPRs       []int64
	deletedBranches []string
}

func (f *fakeGitea) EnsureRepo(ctx context.Context, owner, repo string) (*gitea.Repo, error) {
	return &gitea.Repo{Name: repo, Private: true}, nil
}
func (f *fakeGitea) CheckToken(ctx context.Context) error { return nil }
func (f *fakeGitea) OpenPRs(ctx context.Context, owner, repo string) ([]gitea.PR, error) {
	return f.prs, nil
}
func (f *fakeGitea) FindOpenPRByHead(ctx context.Context, owner, repo, branch string) (*gitea.PR, error) {
	for i := range f.prs {
		if f.prs[i].Head.Ref == branch {
			return &f.prs[i], nil
		}
	}
	return nil, nil
}
func (f *fakeGitea) ClosePR(ctx context.Context, owner, repo string, index int64) error {
	f.closedPRs = append(f.closedPRs, index)
	return nil
}
func (f *fakeGitea) CommentOnPR(ctx context.Context, owner, repo string, index int64, body string) error {
	f.comments = append(f.comments, body)
	return nil
}
func (f *fakeGitea) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	f.deletedBranches = append(f.deletedBranches, branch)
	return nil
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeCommit(t *testing.T, repo, file, content, msg string) {
	t.Helper()
	full := filepath.Join(repo, file)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "--", file)
	gitRun(t, repo, "commit", "-m", msg)
}

// TestFullPipeline drives init → sync → promote → upstream merge → sync
// (auto-finalize) through the job runner, asserting job statuses, promotion
// state transitions, the Gitea API side effects, and log secret hygiene.
func TestFullPipeline(t *testing.T) {
	if err := exec.Command("git", "filter-repo", "--version").Run(); err != nil {
		t.Skip("git-filter-repo not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.git")
	mirror := filepath.Join(dir, "gitea.git")
	dev := filepath.Join(dir, "dev")
	gitRun(t, dir, "init", "--bare", "-b", "main", src)
	gitRun(t, dir, "init", "--bare", "-b", "main", mirror)
	gitRun(t, dir, "clone", src, dev)
	gitRun(t, dev, "config", "user.name", "Dev")
	gitRun(t, dev, "config", "user.email", "dev@x")
	writeCommit(t, dev, "public/a.txt", "hello\n", "init")
	writeCommit(t, dev, "secret/s.txt", "TOPSECRET\n", "secret stuff")
	gitRun(t, dev, "push", "origin", "main")

	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	tokenEnc, _ := box.Encrypt("tok-secret-value")
	bridge := &store.Bridge{
		Name: "Pipe", Slug: "pipe", SourceRemoteURL: src,
		GiteaBaseURL: "http://gitea.test", GiteaOwner: "ai", GiteaRepo: "pipe",
		GiteaSSHURL: mirror, GiteaTokenEnc: tokenEnc,
		ExcludedPaths: []string{"secret"}, SyncBranches: []string{"main"},
		Status: "paused",
	}
	if err := st.CreateBridge(bridge); err != nil {
		t.Fatal(err)
	}

	fake := &fakeGitea{}
	svc := New(st, box, filepath.Join(dir, "work"), "", 1)
	svc.NewGitea = func(baseURL, token string) GiteaAPI {
		if token != "tok-secret-value" {
			t.Errorf("decrypted token wrong: %q", token)
		}
		return fake
	}

	runKind := func(kind string, payload any) *store.Job {
		t.Helper()
		id, err := st.EnqueueJob(bridge.ID, kind, payload)
		if err != nil {
			t.Fatal(err)
		}
		job, err := st.ClaimJob()
		if err != nil || job.ID != id {
			t.Fatalf("claim: %v (job %v)", err, job)
		}
		svc.runJob(context.Background(), job)
		job, err = st.JobByID(id)
		if err != nil {
			t.Fatal(err)
		}
		return job
	}

	// init (includes first sync + verify)
	job := runKind("init", nil)
	if job.Status != "success" {
		t.Fatalf("init failed: %s\n%s", job.ErrorSummary, job.Log)
	}
	b, _ := st.BridgeByID(bridge.ID)
	if b.LastVerifyOK == nil || !*b.LastVerifyOK {
		t.Fatal("init did not record a passing verification")
	}

	// Agent work on the mirror + an open PR for it.
	agent := filepath.Join(dir, "agent")
	gitRun(t, dir, "clone", mirror, agent)
	gitRun(t, agent, "config", "user.name", "Agent")
	gitRun(t, agent, "config", "user.email", "agent@vm")
	gitRun(t, agent, "checkout", "-b", "feat")
	writeCommit(t, agent, "public/feat.txt", "new\n", "agent: feat")
	gitRun(t, agent, "push", "origin", "feat")
	pr := gitea.PR{Number: 7}
	pr.Head.Ref = "feat"
	pr.Base.Ref = "main"
	fake.prs = []gitea.PR{pr}

	// promote
	job = runKind("promote", map[string]string{"branch": "feat", "base": "main"})
	if job.Status != "success" {
		t.Fatalf("promote failed: %s\n%s", job.ErrorSummary, job.Log)
	}
	if len(fake.comments) != 1 || !strings.Contains(fake.comments[0], "Promoted upstream as `feat`") {
		t.Fatalf("missing promotion PR comment: %v", fake.comments)
	}
	promos, _ := st.PromotionsForBridge(bridge.ID)
	if len(promos) != 1 || promos[0].Status != "promoted" || *promos[0].GiteaPRNumber != 7 {
		t.Fatalf("promotion record wrong: %+v", promos[0])
	}
	if gitRun(t, src, "rev-parse", "refs/heads/feat") != promos[0].RealTipSHA {
		t.Fatal("recorded tip does not match upstream branch")
	}

	// Merge upstream, then sync — finalization must auto-trigger.
	gitRun(t, dev, "fetch", "origin")
	gitRun(t, dev, "merge", "--no-ff", "-m", "merge feat", "origin/feat")
	gitRun(t, dev, "push", "origin", "main")
	job = runKind("sync", nil)
	if job.Status != "success" {
		t.Fatalf("sync failed: %s\n%s", job.ErrorSummary, job.Log)
	}
	promos, _ = st.PromotionsForBridge(bridge.ID)
	if promos[0].Status != "finalized" {
		t.Fatalf("promotion not auto-finalized after sync: %+v\n%s", promos[0], job.Log)
	}
	if len(fake.closedPRs) != 1 || fake.closedPRs[0] != 7 {
		t.Fatalf("PR not closed: %v", fake.closedPRs)
	}
	if len(fake.deletedBranches) != 1 || fake.deletedBranches[0] != "feat" {
		t.Fatalf("gitea branch not deleted: %v", fake.deletedBranches)
	}
	if !strings.Contains(fake.comments[1], "not a rejection") {
		t.Fatalf("close comment must explain close-not-merge: %q", fake.comments[1])
	}
	out, err := exec.Command("git", "-C", src, "rev-parse", "--verify", "refs/heads/feat").CombinedOutput()
	if err == nil {
		t.Fatalf("upstream feat not deleted: %s", out)
	}

	// §13.9: no token material in any job log.
	jobsList, _ := st.JobsForBridge(bridge.ID, 100)
	for _, j := range jobsList {
		if strings.Contains(j.Log, "tok-secret-value") {
			t.Fatalf("token leaked in job %d log", j.ID)
		}
	}

	// Guard violation through the job runner: rejected promotion + audit.
	gitRun(t, agent, "fetch", "origin")
	gitRun(t, agent, "checkout", "-b", "sneaky", "origin/main")
	writeCommit(t, agent, "secret/evil.txt", "x\n", "agent: sneak")
	gitRun(t, agent, "push", "origin", "sneaky")
	job = runKind("promote", map[string]string{"branch": "sneaky", "base": "main"})
	if job.Status != "failed" || !strings.Contains(job.Log, "SECURITY GUARD") {
		t.Fatalf("guard violation not surfaced: %s\n%s", job.Status, job.Log)
	}
	promos, _ = st.PromotionsForBridge(bridge.ID)
	if promos[0].Status != "rejected" { // newest first
		t.Fatalf("guard violation promotion not rejected: %+v", promos[0])
	}
	entries, _ := st.AuditEntries(bridge.ID, "", 10)
	found := false
	for _, e := range entries {
		if e.Action == "promotion_rejected" {
			found = true
		}
	}
	if !found {
		t.Fatal("no audit entry for rejected promotion")
	}
}

func TestWebhookDebounceCoalesces(t *testing.T) {
	st := testStore(t)
	b := mkBridge(t, st, "hook")
	box, _ := secrets.New(strings.Repeat("cd", 32))
	svc := New(st, box, t.TempDir(), "", 1)
	svc.WebhookDebounce = 50 * time.Millisecond

	for i := 0; i < 5; i++ {
		svc.WebhookSync(b.ID)
	}
	time.Sleep(200 * time.Millisecond)
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE bridge_id=? AND kind='sync'`, b.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 coalesced sync job, got %d", n)
	}
}
