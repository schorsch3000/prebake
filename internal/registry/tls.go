package registry

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certReloader serves a certificate from disk and picks up a replaced
// certificate or key file (certbot, an internal CA) without a restart. It
// stats both files at most once per checkEvery on a handshake.
type certReloader struct {
	certPath, keyPath string
	log               *slog.Logger
	checkEvery        time.Duration

	mu        sync.Mutex
	cert      *tls.Certificate
	certMod   time.Time
	keyMod    time.Time
	lastCheck time.Time
}

func newCertReloader(certPath, keyPath string, log *slog.Logger) (*certReloader, error) {
	r := &certReloader{certPath: certPath, keyPath: keyPath, log: log, checkEvery: 5 * time.Second}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func modTime(path string) (time.Time, error) {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return st.ModTime(), nil
}

// load reads the pair from disk. The caller holds mu, or is the constructor.
func (r *certReloader) load() error {
	cm, err := modTime(r.certPath)
	if err != nil {
		return fmt.Errorf("tls certificate: %w", err)
	}
	km, err := modTime(r.keyPath)
	if err != nil {
		return fmt.Errorf("tls key: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return fmt.Errorf("tls key pair: %w", err)
	}
	r.cert, r.certMod, r.keyMod, r.lastCheck = &cert, cm, km, time.Now()
	return nil
}

// GetCertificate is used as tls.Config.GetCertificate. A reload that fails
// (for example a half-written file) keeps serving the previous certificate and
// is retried on the next check.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.lastCheck) >= r.checkEvery {
		r.lastCheck = time.Now()
		cm, cerr := modTime(r.certPath)
		km, kerr := modTime(r.keyPath)
		if cerr == nil && kerr == nil && (!cm.Equal(r.certMod) || !km.Equal(r.keyMod)) {
			if err := r.load(); err != nil {
				r.log.Error("tls reload failed, keeping the previous certificate", "err", err)
			} else {
				r.log.Info("tls certificate reloaded", "cert", r.certPath)
			}
		}
	}
	return r.cert, nil
}
