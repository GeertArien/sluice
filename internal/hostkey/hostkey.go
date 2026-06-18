// Package hostkey scans a remote SSH server for its host keys so the
// operator can review the fingerprints and pin them into the managed
// known_hosts (spec §9.4: host keys are pinned, never auto-accepted).
package hostkey

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Key is a discovered host key, ready to review and store.
type Key struct {
	Host        string // known_hosts host pattern (e.g. "bitbucket.org" or "[h]:2222")
	Type        string // e.g. ssh-ed25519
	Fingerprint string // SHA256:...
	Line        string // full known_hosts line
}

// scanAlgos are the host-key algorithms we probe for, mirroring ssh-keyscan.
var scanAlgos = []string{
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoRSA,
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoECDSA384,
	ssh.KeyAlgoECDSA521,
}

// Scan connects to host:port once per algorithm and captures each offered
// host key during the handshake (authentication is expected to fail and is
// irrelevant). It returns the unique keys with their fingerprints and
// known_hosts lines.
func Scan(host string, port int, timeout time.Duration) ([]Key, error) {
	if port == 0 {
		port = 22
	}
	if timeout == 0 {
		timeout = 6 * time.Second
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	pattern := knownhosts.Normalize(addr)

	seen := map[string]bool{}
	var keys []Key
	var lastErr error
	dialed := false

	for _, algo := range scanAlgos {
		var captured ssh.PublicKey
		conf := &ssh.ClientConfig{
			User: "sluice-hostkey-scan",
			HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
				captured = key
				return nil // accept so the handshake completes and we keep the key
			},
			HostKeyAlgorithms: []string{algo},
			Timeout:           timeout,
		}
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			if dialed {
				continue // transient; we already reached the host
			}
			return nil, fmt.Errorf("connect to %s: %w", addr, err)
		}
		dialed = true
		_ = conn.SetDeadline(time.Now().Add(timeout))
		c, _, _, err := ssh.NewClientConn(conn, addr, conf)
		if c != nil {
			_ = c.Close()
		}
		_ = conn.Close()
		if captured == nil {
			if err != nil {
				lastErr = err // e.g. server doesn't offer this algorithm
			}
			continue
		}
		fp := ssh.FingerprintSHA256(captured)
		if seen[fp] {
			continue
		}
		seen[fp] = true
		keys = append(keys, Key{
			Host:        pattern,
			Type:        captured.Type(),
			Fingerprint: fp,
			Line:        strings.TrimRight(knownhosts.Line([]string{pattern}, captured), "\n"),
		})
	}
	if len(keys) == 0 {
		if lastErr != nil {
			return nil, fmt.Errorf("no host keys retrieved from %s: %w", addr, lastErr)
		}
		return nil, fmt.Errorf("no host keys retrieved from %s", addr)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Type < keys[j].Type })
	return keys, nil
}

// ParseLine extracts the host pattern, key type and SHA256 fingerprint from a
// known_hosts line. ok is false for comments, blanks or unparseable lines.
func ParseLine(line string) (host, keyType, fingerprint string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", "", false
	}
	fields := strings.Fields(line)
	// Optional marker like @cert-authority / @revoked precedes the host.
	if len(fields) > 0 && strings.HasPrefix(fields[0], "@") {
		fields = fields[1:]
	}
	if len(fields) < 3 {
		return "", "", "", false
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(fields[1] + " " + fields[2]))
	if err != nil {
		return "", "", "", false
	}
	return fields[0], fields[1], ssh.FingerprintSHA256(pub), true
}
