package jobs

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geertarien/sluice/internal/execx"
	"github.com/geertarien/sluice/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func mkBridge(t *testing.T, st *store.Store, slug string) *store.Bridge {
	t.Helper()
	b := &store.Bridge{Name: slug, Slug: slug, SourceRemoteURL: "/x", GiteaBaseURL: "http://g",
		GiteaOwner: "o", GiteaRepo: "r", GiteaSSHURL: "/g", Status: "active"}
	if err := st.CreateBridge(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// §13.8: jobs for one bridge are serialized; different bridges run in parallel.
func TestClaimJobSerializesPerBridge(t *testing.T) {
	st := testStore(t)
	b1 := mkBridge(t, st, "one")
	b2 := mkBridge(t, st, "two")

	for _, id := range []int64{b1.ID, b1.ID, b2.ID} {
		if _, err := st.EnqueueJob(id, "sync", nil); err != nil {
			t.Fatal(err)
		}
	}
	j1, err := st.ClaimJob()
	if err != nil {
		t.Fatal(err)
	}
	if j1.BridgeID != b1.ID {
		t.Fatalf("expected bridge one first, got %d", j1.BridgeID)
	}
	// Second claim must skip bridge one (busy) and take bridge two.
	j2, err := st.ClaimJob()
	if err != nil {
		t.Fatal(err)
	}
	if j2.BridgeID != b2.ID {
		t.Fatalf("expected bridge two, got bridge %d", j2.BridgeID)
	}
	// Nothing else claimable while bridge one is running.
	if _, err := st.ClaimJob(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected no claimable job, got %v", err)
	}
	if err := st.FinishJob(j1.ID, "success", ""); err != nil {
		t.Fatal(err)
	}
	j3, err := st.ClaimJob()
	if err != nil {
		t.Fatal(err)
	}
	if j3.BridgeID != b1.ID {
		t.Fatalf("expected queued bridge-one job after finish, got %d", j3.BridgeID)
	}
}

// §13.9: secret material never reaches logs.
func TestRunnerScrubsSecrets(t *testing.T) {
	var logged []string
	r := &execx.Runner{
		Secrets: []string{"tok-supersecret-123", "whsec-abc"},
		Log:     func(s string) { logged = append(logged, s) },
	}
	r.Log(r.Scrub("Authorization: token tok-supersecret-123"))
	r.Log(r.Scrub("pushing to https://user:tok-supersecret-123@gitea/x.git"))
	r.Log(r.Scrub("webhook secret whsec-abc rejected"))
	all := strings.Join(logged, "\n")
	if strings.Contains(all, "tok-supersecret-123") || strings.Contains(all, "whsec-abc") {
		t.Fatalf("secret leaked into log:\n%s", all)
	}
	if !strings.Contains(all, "[REDACTED]") {
		t.Fatalf("expected redaction markers:\n%s", all)
	}
}

func TestCompareURL(t *testing.T) {
	cases := map[string]string{
		"git@github.com:acme/widgets.git":    "https://github.com/acme/widgets/compare/main...ai%2Ffeature",
		"https://github.com/acme/widgets":    "https://github.com/acme/widgets/compare/main...ai%2Ffeature",
		"git@gitlab.com:acme/widgets.git":    "https://gitlab.com/acme/widgets/-/compare/main...ai%2Ffeature",
		"ssh://git@internal-host/x/y.git":    "",
		"https://gitea.corp.example/o/r.git": "https://gitea.corp.example/o/r/compare/main...ai%2Ffeature",
	}
	for in, want := range cases {
		if got := CompareURL(in, "main", "ai/feature"); got != want {
			t.Errorf("CompareURL(%q) = %q, want %q", in, got, want)
		}
	}
}
