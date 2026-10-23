// Package build runs one build job end to end: build a candidate image, smoke
// test it, push it to the registry and promote it (docs/design.md §7). All
// Docker access goes through the Docker interface so the pipeline is unit
// testable without a Docker daemon.
package build

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
)

// Spec describes one image build.
type Spec struct {
	Tag        string            // image reference to tag the result with
	ContextDir string            // build context
	Dockerfile string            // path of the Dockerfile; ignored if Body is set
	Body       string            // Dockerfile content, passed on stdin (for generated Dockerfiles)
	Args       map[string]string // --build-arg
	NoCache    bool
	Pull       bool
}

// Docker is the subset of the Docker CLI the pipeline needs.
type Docker interface {
	// Build builds Spec for linux/amd64 into the local image store. Output goes to log.
	Build(ctx context.Context, s Spec, log io.Writer) error
	// Run runs `sh -c cmd` in a throwaway container of image and returns combined output.
	Run(ctx context.Context, image, cmd string) (string, error)
	Tag(ctx context.Context, src, dst string) error
	Push(ctx context.Context, ref string, log io.Writer) error
	// Pull fetches ref into the local store.
	Pull(ctx context.Context, ref string, log io.Writer) error
	// Exists reports whether ref is in the local image store.
	Exists(ctx context.Context, ref string) (bool, error)
	// Digest returns the registry digest ("sha256:...") of a pushed ref.
	Digest(ctx context.Context, ref string) (string, error)
	Remove(ctx context.Context, ref string) error
}

// ExecFunc runs a command; it is replaced in tests.
type ExecFunc func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error

func osExec(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

// CLI implements Docker by shelling out to the docker binary.
type CLI struct {
	Bin  string   // default "docker"
	Exec ExecFunc // default os/exec
}

func (c *CLI) run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	bin, ex := c.Bin, c.Exec
	if bin == "" {
		bin = "docker"
	}
	if ex == nil {
		ex = osExec
	}
	return ex(ctx, stdin, stdout, stderr, bin, args...)
}

func (c *CLI) capture(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	err := c.run(ctx, nil, &out, &out, args...)
	return strings.TrimSpace(out.String()), err
}

func (c *CLI) Build(ctx context.Context, s Spec, log io.Writer) error {
	args := []string{"buildx", "build", "--platform", "linux/amd64", "--load", "--progress=plain", "-t", s.Tag}
	if s.NoCache {
		args = append(args, "--no-cache")
	}
	if s.Pull {
		args = append(args, "--pull")
	}
	keys := make([]string, 0, len(s.Args))
	for k := range s.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+s.Args[k])
	}
	var stdin io.Reader
	if s.Body != "" {
		args = append(args, "-f", "-")
		stdin = strings.NewReader(s.Body)
	} else if s.Dockerfile != "" {
		args = append(args, "-f", s.Dockerfile)
	}
	args = append(args, s.ContextDir)
	return c.run(ctx, stdin, log, log, args...)
}

func (c *CLI) Run(ctx context.Context, image, cmd string) (string, error) {
	return c.capture(ctx, "run", "--rm", "--platform", "linux/amd64", "--entrypoint", "sh", image, "-c", cmd)
}

func (c *CLI) Tag(ctx context.Context, src, dst string) error {
	out, err := c.capture(ctx, "tag", src, dst)
	if err != nil {
		return fmt.Errorf("docker tag %s %s: %v: %s", src, dst, err, out)
	}
	return nil
}

func (c *CLI) Push(ctx context.Context, ref string, log io.Writer) error {
	return c.run(ctx, nil, log, log, "push", ref)
}

func (c *CLI) Pull(ctx context.Context, ref string, log io.Writer) error {
	return c.run(ctx, nil, log, log, "pull", "--platform", "linux/amd64", ref)
}

func (c *CLI) Exists(ctx context.Context, ref string) (bool, error) {
	var out bytes.Buffer
	if err := c.run(ctx, nil, &out, &out, "image", "inspect", "--format", "{{.Id}}", ref); err != nil {
		if strings.Contains(out.String(), "No such image") {
			return false, nil
		}
		return false, fmt.Errorf("docker image inspect %s: %v: %s", ref, err, strings.TrimSpace(out.String()))
	}
	return true, nil
}

func (c *CLI) Digest(ctx context.Context, ref string) (string, error) {
	out, err := c.capture(ctx, "image", "inspect", "--format", "{{json .RepoDigests}}", ref)
	if err != nil {
		return "", fmt.Errorf("docker image inspect %s: %v: %s", ref, err, out)
	}
	var digests []string
	if err := json.Unmarshal([]byte(out), &digests); err != nil {
		return "", fmt.Errorf("parse RepoDigests %q: %v", out, err)
	}
	repo := ref[:strings.LastIndex(ref, ":")]
	for _, d := range digests {
		if r, dg, ok := strings.Cut(d, "@"); ok && r == repo {
			return dg, nil
		}
	}
	return "", fmt.Errorf("no registry digest for %s (pushed?)", ref)
}

func (c *CLI) Remove(ctx context.Context, ref string) error {
	out, err := c.capture(ctx, "image", "rm", ref)
	if err != nil {
		return fmt.Errorf("docker image rm %s: %v: %s", ref, err, out)
	}
	return nil
}
