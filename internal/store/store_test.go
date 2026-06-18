package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrationUpgradesOldDatabase builds a pre-SSH-key bridges table, inserts
// a row, then opens it through Open and confirms the new columns are added and
// usable — the upgrade path real deployments hit.
func TestMigrationUpgradesOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE bridges (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL, slug TEXT NOT NULL UNIQUE,
  source_remote_url TEXT NOT NULL, gitea_base_url TEXT NOT NULL,
  gitea_owner TEXT NOT NULL, gitea_repo TEXT NOT NULL, gitea_ssh_url TEXT NOT NULL,
  gitea_token_enc BLOB, excluded_paths TEXT NOT NULL DEFAULT '[]',
  sync_branches TEXT NOT NULL DEFAULT '[]', sync_globs TEXT NOT NULL DEFAULT '[]',
  tripwire_strings TEXT NOT NULL DEFAULT '[]',
  promote_name TEXT NOT NULL DEFAULT '', promote_email TEXT NOT NULL DEFAULT '',
  promote_keep_trailer INTEGER NOT NULL DEFAULT 1, promote_signoff INTEGER NOT NULL DEFAULT 0,
  schedule_cron TEXT NOT NULL DEFAULT '', webhook_secret_enc BLOB,
  status TEXT NOT NULL DEFAULT 'paused',
  last_sync_at TIMESTAMP, last_sync_ok INTEGER,
  last_verified_at TIMESTAMP, last_verify_ok INTEGER,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO bridges
 (name, slug, source_remote_url, gitea_base_url, gitea_owner, gitea_repo, gitea_ssh_url)
 VALUES ('Old','old','/x','http://g','o','r','/g')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open/migrate old db: %v", err)
	}
	b, err := st.BridgeBySlug("old")
	if err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if b.SSHPublicKey != "" || b.SSHPrivateKeyEnc != nil {
		t.Fatalf("expected empty ssh defaults, got pub=%q priv=%v", b.SSHPublicKey, b.SSHPrivateKeyEnc)
	}
	b.SSHPublicKey = "ssh-ed25519 AAAA"
	b.SSHPrivateKeyEnc = []byte("enc")
	if err := st.UpdateBridge(b); err != nil {
		t.Fatalf("update with ssh fields: %v", err)
	}
	got, _ := st.BridgeBySlug("old")
	if got.SSHPublicKey != "ssh-ed25519 AAAA" || string(got.SSHPrivateKeyEnc) != "enc" {
		t.Fatalf("ssh fields not persisted: pub=%q", got.SSHPublicKey)
	}
}
