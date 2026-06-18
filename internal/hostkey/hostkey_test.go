package hostkey

import (
	"crypto/ed25519"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// startSSHServer launches a minimal SSH server with a fixed ed25519 host key
// that rejects all auth (we only care about the host key handshake).
func startSSHServer(t *testing.T) (port int, fingerprint string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint = ssh.FingerprintSHA256(signer.PublicKey())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, ssh.ErrNoAuth // present host key, then refuse auth
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
				conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err == nil {
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
	port, _ = strconv.Atoi(portStr)
	return port, fingerprint
}

func TestScanCapturesHostKey(t *testing.T) {
	port, fp := startSSHServer(t)

	keys, err := Scan("127.0.0.1", port, 3*time.Second)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var ed *Key
	for i := range keys {
		if keys[i].Type == ssh.KeyAlgoED25519 {
			ed = &keys[i]
		}
	}
	if ed == nil {
		t.Fatalf("ed25519 key not captured, got %+v", keys)
	}
	if ed.Fingerprint != fp {
		t.Fatalf("fingerprint mismatch: got %s want %s", ed.Fingerprint, fp)
	}
	if !strings.Contains(ed.Line, "ssh-ed25519") {
		t.Fatalf("known_hosts line malformed: %q", ed.Line)
	}
	// The line must round-trip through ParseLine to the same fingerprint.
	_, kt, pfp, ok := ParseLine(ed.Line)
	if !ok || kt != ssh.KeyAlgoED25519 || pfp != fp {
		t.Fatalf("ParseLine round-trip failed: ok=%v type=%s fp=%s", ok, kt, pfp)
	}
}

func TestScanUnreachable(t *testing.T) {
	// Port 1 is reserved and refuses connections quickly.
	if _, err := Scan("127.0.0.1", 1, 1*time.Second); err == nil {
		t.Fatal("expected error scanning an unreachable port")
	}
}

func TestParseLineRejectsJunk(t *testing.T) {
	for _, l := range []string{"", "# comment", "host only", "host badtype notbase64"} {
		if _, _, _, ok := ParseLine(l); ok {
			t.Errorf("expected %q to be rejected", l)
		}
	}
}
