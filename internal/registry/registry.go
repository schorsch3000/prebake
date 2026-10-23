// Package registry embeds distribution/distribution and serves it on two
// listeners: a read-only one for everybody and a write one that only the
// daemon's own builds use (docs/design.md §9).
package registry

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/handlers"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/filesystem"
	"github.com/sirupsen/logrus"
)

// Config configures the embedded registry.
type Config struct {
	DataDir   string // storage root; the registry lives in DataDir/registry
	ReadAddr  string // read-only listener, e.g. ":5000"
	WriteAddr string // write listener, e.g. ":5001"; expose it to the host loopback only
	TLSCert   string // optional; both or neither of TLSCert/TLSKey
	TLSKey    string
	Logger    *slog.Logger
	Verbose   bool // log every registry request (distribution logs at info level)
}

// StorageDir is where the registry keeps its data.
func (c Config) StorageDir() string { return filepath.Join(c.DataDir, "registry") }

// Server is the running registry.
type Server struct {
	cfg       Config
	log       *slog.Logger
	readSrv   *http.Server
	writeSrv  *http.Server
	readLn    net.Listener
	writeLn   net.Listener
	certs     *certReloader
	wg        sync.WaitGroup
	serveErrs chan error
}

// ReadOnly wraps h so that only GET and HEAD get through; every other method
// is answered with 405. It is what makes the external listener read-only.
func ReadOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "this registry is read-only", http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func newApp(ctx context.Context, c Config) (http.Handler, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	cfg := &configuration.Configuration{}
	cfg.Storage = configuration.Storage{
		"filesystem": configuration.Parameters{"rootdirectory": c.StorageDir()},
		"delete":     configuration.Parameters{"enabled": true},
	}
	cfg.HTTP.Addr = c.ReadAddr // unused: we run our own servers, but the field must be set
	cfg.HTTP.Secret = hex.EncodeToString(secret)
	// defaults normally come from the YAML parser; built in code the page limit would be 0
	cfg.Catalog.MaxEntries = 1000
	return handlers.NewApp(ctx, cfg), nil
}

// New prepares the server; call Start to listen.
func New(ctx context.Context, c Config) (*Server, error) {
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, errors.New("registry: TLS needs both certificate and key")
	}
	if c.DataDir == "" {
		return nil, errors.New("registry: DataDir is required")
	}
	// distribution logs through the global logrus logger and is very chatty at info
	if c.Verbose {
		logrus.SetLevel(logrus.InfoLevel)
	} else {
		logrus.SetLevel(logrus.WarnLevel)
	}
	log := c.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{cfg: c, log: log, serveErrs: make(chan error, 2)}
	if c.TLSCert != "" {
		cr, err := newCertReloader(c.TLSCert, c.TLSKey, log)
		if err != nil {
			return nil, err
		}
		s.certs = cr
	}
	app, err := newApp(ctx, c)
	if err != nil {
		return nil, err
	}
	s.readSrv = &http.Server{Handler: ReadOnly(app), ReadHeaderTimeout: 10 * time.Second}
	s.writeSrv = &http.Server{Handler: app, ReadHeaderTimeout: 10 * time.Second}
	if s.certs != nil {
		s.readSrv.TLSConfig = &tls.Config{GetCertificate: s.certs.GetCertificate, MinVersion: tls.VersionTLS12}
	}
	return s, nil
}

// Start binds both listeners and serves in the background. Once it returns
// the addresses from ReadAddr and WriteAddr are valid.
func (s *Server) Start() error {
	var err error
	if s.readLn, err = net.Listen("tcp", s.cfg.ReadAddr); err != nil {
		return fmt.Errorf("registry read listener: %w", err)
	}
	if s.writeLn, err = net.Listen("tcp", s.cfg.WriteAddr); err != nil {
		s.readLn.Close()
		return fmt.Errorf("registry write listener: %w", err)
	}
	if s.certs == nil {
		s.log.Warn("registry serves plain HTTP on the read-only listener; set TLS_CERT and TLS_KEY to enable TLS",
			"addr", s.ReadAddr())
	}
	s.serve(s.readSrv, s.readLn, s.certs != nil)
	s.serve(s.writeSrv, s.writeLn, false)
	s.log.Info("registry listening", "read", s.ReadAddr(), "write", s.WriteAddr(), "tls", s.certs != nil)
	return nil
}

func (s *Server) serve(srv *http.Server, ln net.Listener, useTLS bool) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		var err error
		if useTLS {
			err = srv.ServeTLS(ln, "", "")
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveErrs <- err
		}
	}()
}

// Errors reports a listener that stopped unexpectedly.
func (s *Server) Errors() <-chan error { return s.serveErrs }

func (s *Server) ReadAddr() string  { return s.readLn.Addr().String() }
func (s *Server) WriteAddr() string { return s.writeLn.Addr().String() }

// Shutdown stops both listeners gracefully.
func (s *Server) Shutdown(ctx context.Context) error {
	err := errors.Join(s.readSrv.Shutdown(ctx), s.writeSrv.Shutdown(ctx))
	s.wg.Wait()
	return err
}
