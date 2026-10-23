package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/schorsch3000/prebake/internal/build"
	"github.com/schorsch3000/prebake/internal/daemon"
	"github.com/schorsch3000/prebake/internal/gitsync"
	"github.com/schorsch3000/prebake/internal/notify"
	"github.com/schorsch3000/prebake/internal/registry"
	"github.com/schorsch3000/prebake/internal/state"
)

// daemonConfig is the bootstrap configuration (docs/design.md §5.1): everything
// needed to find the config repo; the rest comes from the repo itself.
type daemonConfig struct {
	RepoURL      string
	Branch       string
	PollInterval time.Duration
	DataDir      string
	ReadAddr     string
	WriteAddr    string
	RegistryHost string // host:port the Docker host uses to push to the write listener
	TLSCert      string
	TLSKey       string
	KnownHosts   string
	Verbose      bool
}

// parseDaemonConfig reads flags with environment variables as defaults.
func parseDaemonConfig(args []string, getenv func(string) string, stderr io.Writer) (*daemonConfig, error) {
	env := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	c := &daemonConfig{}
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.RepoURL, "repo-url", env("REPO_URL", ""), "git URL of the config repo (REPO_URL)")
	fs.StringVar(&c.Branch, "repo-branch", env("REPO_BRANCH", "main"), "branch to follow (REPO_BRANCH)")
	poll := fs.String("poll", env("POLL_INTERVAL", "5m"), "how often to fetch the repo (POLL_INTERVAL)")
	fs.StringVar(&c.DataDir, "data-dir", env("DATA_DIR", "/data"), "state, keys, checkouts and registry storage (DATA_DIR)")
	fs.StringVar(&c.ReadAddr, "read-addr", env("READ_ADDR", ":5000"), "read-only registry listener (READ_ADDR)")
	fs.StringVar(&c.WriteAddr, "write-addr", env("WRITE_ADDR", ":5001"), "registry write listener, keep it off the network (WRITE_ADDR)")
	fs.StringVar(&c.RegistryHost, "registry-host", env("REGISTRY_HOST", ""), "host:port the Docker host pushes to (default localhost:<write port>) (REGISTRY_HOST)")
	fs.StringVar(&c.TLSCert, "tls-cert", env("TLS_CERT", ""), "TLS certificate for the read-only listener (TLS_CERT)")
	fs.StringVar(&c.TLSKey, "tls-key", env("TLS_KEY", ""), "TLS key (TLS_KEY)")
	fs.StringVar(&c.KnownHosts, "known-hosts", env("KNOWN_HOSTS", ""), "pinned known_hosts file; default is trust on first use (KNOWN_HOSTS)")
	fs.BoolVar(&c.Verbose, "verbose", env("VERBOSE", "") != "", "log every registry request (VERBOSE)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if c.RepoURL == "" {
		return nil, errors.New("REPO_URL (or --repo-url) is required")
	}
	d, err := parseDurationEnv(*poll)
	if err != nil {
		return nil, fmt.Errorf("POLL_INTERVAL: %w", err)
	}
	c.PollInterval = d
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, errors.New("TLS_CERT and TLS_KEY must be set together")
	}
	if c.RegistryHost == "" {
		_, port, err := net.SplitHostPort(c.WriteAddr)
		if err != nil || port == "" {
			return nil, fmt.Errorf("WRITE_ADDR %q: %v", c.WriteAddr, err)
		}
		c.RegistryHost = "localhost:" + port
	}
	return c, nil
}

func parseDurationEnv(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, errors.New("must be positive")
	}
	return d, nil
}

func cmdDaemon(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseDaemonConfig(args, os.Getenv, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "prebake daemon:", err)
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runDaemon(ctx, cfg, log); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(stderr, "prebake daemon:", err)
		return 1
	}
	return 0
}

func runDaemon(ctx context.Context, cfg *daemonConfig, log *slog.Logger) error {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	id, err := gitsync.LoadOrCreate(filepath.Join(cfg.DataDir, "ssh"))
	if err != nil {
		return err
	}
	log.Info("SSH deploy key (add it as a read-only deploy key to the config repo)",
		"fingerprint", id.Fingerprint)
	// the key is also printed bare so that it can be copied from the log
	fmt.Fprintln(os.Stderr, id.PublicLine)

	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		log.Warn("cannot reach the Docker daemon; builds will fail until it is available (is the socket mounted?)",
			"err", err, "output", strings.TrimSpace(string(out)))
	}

	store, err := state.Open(filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	reg, err := registry.New(ctx, registry.Config{
		DataDir: cfg.DataDir, ReadAddr: cfg.ReadAddr, WriteAddr: cfg.WriteAddr,
		TLSCert: cfg.TLSCert, TLSKey: cfg.TLSKey, Logger: log, Verbose: cfg.Verbose,
	})
	if err != nil {
		return err
	}
	if err := reg.Start(); err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		reg.Shutdown(sctx)
	}()
	_, writePort, _ := net.SplitHostPort(reg.WriteAddr())

	src := &gitsync.Syncer{
		URL: cfg.RepoURL, Branch: cfg.Branch, Dir: filepath.Join(cfg.DataDir, "repo"),
		Identity: id, KnownHosts: cfg.KnownHosts,
	}
	notifier, notifyOn, err := notify.FromEnv(os.Getenv)
	if err != nil {
		return err
	}
	if notifyOn {
		log.Info("Pushover notifications enabled")
	} else {
		log.Info("Pushover notifications disabled (set PUSHOVER_TOKEN and PUSHOVER_USER to enable)")
	}
	runner := &build.Runner{Docker: &build.CLI{}, Registry: cfg.RegistryHost}
	d := daemon.New(daemon.Options{PollInterval: cfg.PollInterval, Logger: log}, src, runner,
		&daemon.LocalRegistry{Server: reg, Client: &registry.Client{Base: "http://127.0.0.1:" + writePort}},
		store, notifier)

	errc := make(chan error, 1)
	go func() { errc <- d.Run(ctx) }()
	select {
	case err := <-reg.Errors():
		return fmt.Errorf("registry stopped: %w", err)
	case err := <-errc:
		return err
	}
}
