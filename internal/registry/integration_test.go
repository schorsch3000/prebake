//go:build integration

package registry

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func docker(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// TestIntegrationDockerPushPull uses the real Docker CLI: push works through
// the write listener, is refused through the read-only listener, and pulling
// from the read-only listener works. Docker treats localhost registries as
// insecure-OK, so no TLS setup is needed.
func TestIntegrationDockerPushPull(t *testing.T) {
	if out, err := docker(t, "info"); err != nil {
		t.Skipf("docker not available: %v: %s", err, out)
	}
	s := start(t, nil)
	port := func(addr string) string { return addr[strings.LastIndex(addr, ":")+1:] }
	write := "localhost:" + port(s.WriteAddr())
	read := "localhost:" + port(s.ReadAddr())

	if out, err := docker(t, "pull", "busybox:stable"); err != nil {
		t.Fatalf("pull busybox: %v: %s", err, out)
	}
	wref, rref := write+"/it/busybox:t1", read+"/it/busybox:t1"
	for _, ref := range []string{wref, rref} {
		if out, err := docker(t, "tag", "busybox:stable", ref); err != nil {
			t.Fatalf("tag: %v: %s", err, out)
		}
		t.Cleanup(func() { docker(t, "image", "rm", "-f", ref) })
	}

	if out, err := docker(t, "push", wref); err != nil {
		t.Fatalf("push via write listener: %v\n%s", err, out)
	}
	// the same content through the read-only listener must be refused
	if out, err := docker(t, "push", rref); err == nil {
		t.Fatalf("push via read-only listener must fail, got:\n%s", out)
	}

	// pull what was pushed, through the read-only listener, from a clean local state
	docker(t, "image", "rm", "-f", rref)
	if out, err := docker(t, "pull", rref); err != nil {
		t.Fatalf("pull via read-only listener: %v\n%s", err, out)
	}
	if out, err := docker(t, "run", "--rm", rref, "echo", "ok"); err != nil || out != "ok" {
		t.Fatalf("run pulled image: %v %q", err, out)
	}
}
