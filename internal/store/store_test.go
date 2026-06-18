package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrationUpgradesOldDatabase builds a pre-SSH-key bridges table, inserts
// a plain row and a row with a legacy inline SSH key, then opens it through
// Open and confirms the schema upgrades and the inline key migrates into the
// named ssh_keys table — the upgrade path real deployments hit.
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
  ssh_private_key_enc BLOB, ssh_public_key TEXT NOT NULL DEFAULT '',
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
 VALUES ('Plain','plain','/x','http://g','o','r','/g')`); err != nil {
		t.Fatal(err)
	}
	// A bridge with a legacy inline key (from the first SSH-key release).
	if _, err := db.Exec(`INSERT INTO bridges
 (name, slug, source_remote_url, gitea_base_url, gitea_owner, gitea_repo, gitea_ssh_url,
  ssh_public_key, ssh_private_key_enc)
 VALUES ('Inline','inline','/x','http://g','o','r','/g','ssh-ed25519 AAAA', ?)`,
		[]byte("encrypted-private-key")); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open/migrate old db: %v", err)
	}

	// Plain bridge: no key after migration, and the promotion prefix column
	// backfilled to the default.
	plain, err := st.BridgeBySlug("plain")
	if err != nil {
		t.Fatalf("read migrated plain row: %v", err)
	}
	if plain.PromoteBranchPrefix != "ai/" {
		t.Fatalf("promote prefix not backfilled: %q", plain.PromoteBranchPrefix)
	}
	if plain.SSHKeyID != nil {
		t.Fatalf("plain bridge should have no key, got %v", plain.SSHKeyID)
	}

	// Inline bridge: its key moved into ssh_keys and is now referenced by id.
	inline, err := st.BridgeBySlug("inline")
	if err != nil {
		t.Fatalf("read migrated inline row: %v", err)
	}
	if inline.SSHKeyID == nil {
		t.Fatal("inline key was not migrated to ssh_key_id")
	}
	k, err := st.SSHKeyByID(*inline.SSHKeyID)
	if err != nil {
		t.Fatalf("migrated key missing: %v", err)
	}
	if k.Name != "bridge-inline" || k.PublicKey != "ssh-ed25519 AAAA" || string(k.PrivateKeyEnc) != "encrypted-private-key" {
		t.Fatalf("migrated key wrong: %+v", k)
	}

	// Migration is idempotent: re-opening doesn't create a duplicate key.
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := st2.SSHKeys()
	if len(keys) != 1 {
		t.Fatalf("expected exactly 1 migrated key, got %d", len(keys))
	}
}
