// Package gitsync keeps a local copy of the config repo up to date: it owns the
// SSH deploy key, fetches over git, and exports validated commits as checkouts
// (docs/design.md §11).
package gitsync

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	privName = "id_ed25519"
	pubName  = "id_ed25519.pub"
)

// Identity is the daemon's SSH deploy key.
type Identity struct {
	KeyPath     string // private key (OpenSSH format, mode 0600)
	PubPath     string
	PublicLine  string // authorized_keys line, ready to paste as a deploy key
	Fingerprint string // SHA256:...
}

// LoadOrCreate returns the identity in dir (DATA_DIR/ssh), generating an
// ed25519 key on first use. An existing key is never regenerated or replaced.
func LoadOrCreate(dir string) (*Identity, error) {
	id := &Identity{KeyPath: filepath.Join(dir, privName), PubPath: filepath.Join(dir, pubName)}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(id.KeyPath); errors.Is(err, os.ErrNotExist) {
		if err := generate(id); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return id, id.load()
}

func generate(id *Identity) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	const comment = "prebake" // the container hostname would only be a random id
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + comment + "\n"

	// O_EXCL: if two processes race (daemon and `show-key`), exactly one wins and the
	// other falls through to load the winner's key
	f, err := os.OpenFile(id.KeyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	if _, err := f.Write(pem.EncodeToMemory(block)); err != nil {
		f.Close()
		os.Remove(id.KeyPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(id.KeyPath)
		return err
	}
	return os.WriteFile(id.PubPath, []byte(line), 0o644)
}

// load derives the public values from the private key, and repairs a missing
// .pub file.
func (id *Identity) load() error {
	raw, err := os.ReadFile(id.KeyPath)
	if err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", id.KeyPath, err)
	}
	pub := signer.PublicKey()
	comment := "prebake"
	if b, err := os.ReadFile(id.PubPath); err == nil {
		if _, c, _, _, err := ssh.ParseAuthorizedKey(b); err == nil && c != "" {
			comment = c
		}
	}
	id.PublicLine = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + comment
	id.Fingerprint = ssh.FingerprintSHA256(pub)
	if _, err := os.Stat(id.PubPath); errors.Is(err, os.ErrNotExist) {
		return os.WriteFile(id.PubPath, []byte(id.PublicLine+"\n"), 0o644)
	}
	return nil
}
