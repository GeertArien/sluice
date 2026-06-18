package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geertarien/sluice/internal/secrets"
	"github.com/geertarien/sluice/internal/sshkey"
	"github.com/geertarien/sluice/internal/store"
)

func TestEngineForMaterializesPerBridgeSSHKey(t *testing.T) {
	st := testStore(t)
	box, _ := secrets.New(strings.Repeat("ab", 32))
	priv, _, err := sshkey.Generate("test")
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := box.Encrypt(priv)
	key := &store.SSHKey{Name: "named", PublicKey: "ssh-ed25519 AAAA", PrivateKeyEnc: enc}
	if err := st.CreateSSHKey(key); err != nil {
		t.Fatal(err)
	}
	b := &store.Bridge{
		Name: "k", Slug: "k", SourceRemoteURL: "/x", GiteaBaseURL: "http://g",
		GiteaOwner: "o", GiteaRepo: "r", GiteaSSHURL: "/g",
		SSHKeyID: &key.ID, Status: "active",
	}
	if err := st.CreateBridge(b); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	svc := New(st, box, workdir, "/data/known_hosts", 1)

	bb, _ := st.BridgeBySlug("k")
	eng := svc.EngineFor(bb, "tok", func(string) {})

	// Key file written 0600 in the bridge workspace.
	keyPath := filepath.Join(workdir, "k", ".ssh_id")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("ssh key not materialized: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ssh key mode = %v, want 0600", info.Mode().Perm())
	}

	// GIT_SSH_COMMAND uses the key exclusively plus the pinned known_hosts.
	var sshCmd string
	for _, e := range eng.Runner.Env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			sshCmd = e
		}
	}
	if !strings.Contains(sshCmd, "-i "+keyPath) ||
		!strings.Contains(sshCmd, "IdentitiesOnly=yes") ||
		!strings.Contains(sshCmd, "UserKnownHostsFile=/data/known_hosts") ||
		!strings.Contains(sshCmd, "StrictHostKeyChecking=yes") {
		t.Fatalf("unexpected GIT_SSH_COMMAND: %q", sshCmd)
	}

	// The private key must be a scrub target so it never reaches logs.
	scrubbed := false
	for _, sec := range eng.Runner.Secrets {
		if sec == priv {
			scrubbed = true
		}
	}
	if !scrubbed {
		t.Fatal("private key not registered as a scrub secret")
	}
}

func TestEngineForFallsBackToMountedKey(t *testing.T) {
	st := testStore(t)
	box, _ := secrets.New(strings.Repeat("cd", 32))
	b := mkBridge(t, st, "nokey")
	svc := New(st, box, t.TempDir(), "/data/known_hosts", 1)

	eng := svc.EngineFor(b, "tok", func(string) {})
	var sshCmd string
	for _, e := range eng.Runner.Env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			sshCmd = e
		}
	}
	// No per-bridge key → no -i; ssh uses the default (mounted) identity.
	if strings.Contains(sshCmd, "-i ") || strings.Contains(sshCmd, "IdentitiesOnly") {
		t.Fatalf("expected no identity file in fallback mode: %q", sshCmd)
	}
	if !strings.Contains(sshCmd, "UserKnownHostsFile=/data/known_hosts") {
		t.Fatalf("known_hosts still expected: %q", sshCmd)
	}
}
