package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/schorsch3000/prebake/internal/config"
	"github.com/schorsch3000/prebake/internal/schedule"
)

// Job is one unit of work for the runner.
type Job struct {
	Container  string
	Flavor     string
	Kind       schedule.Kind
	ContextDir string // containers/<name>
	Config     *config.Container
}

// Result describes a successful build.
type Result struct {
	Kind     schedule.Kind // the kind actually run (a refresh may be upgraded to a rebuild)
	DateTag  string        // e.g. "default-20261022-020000"
	Alias    string        // "latest" for the default flavor, else the flavor name
	Digest   string
	Started  time.Time
	Duration time.Duration
	Note     string // e.g. why a refresh became a rebuild
}

// TestError is returned when a smoke test fails; nothing was pushed.
type TestError struct {
	Cmd    string
	Output string
	Err    error
}

func (e *TestError) Error() string {
	return fmt.Sprintf("smoke test %q failed: %v: %s", e.Cmd, e.Err, e.Output)
}

// Runner executes jobs against a Docker implementation.
type Runner struct {
	Docker   Docker
	Registry string           // host:port of the registry write listener, e.g. "localhost:5001"
	Now      func() time.Time // default time.Now
}

// Alias returns the moving tag for a flavor: "latest" for the default flavor.
func Alias(flavor string) string {
	if flavor == config.DefaultFlavor {
		return "latest"
	}
	return flavor
}

// Ref builds an image reference in the registry.
func (r *Runner) Ref(container, tag string) string {
	return r.Registry + "/" + container + ":" + tag
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]+`)

// ParentArgs returns the build args that tell a Dockerfile where its parents
// are: PARENT (first parent) and PARENT_<NAME> for each.
func (r *Runner) ParentArgs(dependsOn []string) map[string]string {
	args := map[string]string{}
	for i, p := range dependsOn {
		ref := r.Ref(p, "latest")
		if i == 0 {
			args["PARENT"] = ref
		}
		args["PARENT_"+strings.ToUpper(strings.Trim(nonAlnum.ReplaceAllString(p, "_"), "_"))] = ref
	}
	return args
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run builds, tests, pushes and promotes. A failed build or test never
// touches the moving alias tag. Build output is written to log.
func (r *Runner) Run(ctx context.Context, job Job, log io.Writer) (*Result, error) {
	// Docker's own error is just "exit status 1"; the reason is in its output. Keep the
	// tail so that the returned error (and thereby the daemon log and notifications)
	// says why a build or push failed.
	tail := &tailWriter{max: 8192}
	log = io.MultiWriter(log, tail)
	withOutput := func(op string, err error) error {
		if ex := tail.excerpt(15); ex != "" {
			return fmt.Errorf("%s: %w\n%s", op, err, ex)
		}
		return fmt.Errorf("%s: %w", op, err)
	}
	started := r.now()
	stamp := started.UTC().Format("20060102-150405")
	flavor := job.Flavor
	res := &Result{Kind: job.Kind, Started: started, Alias: Alias(flavor), DateTag: flavor + "-" + stamp}
	candidate := r.Ref(job.Container, res.DateTag+"-candidate")
	dateRef := r.Ref(job.Container, res.DateTag)
	aliasRef := r.Ref(job.Container, res.Alias)

	args := map[string]string{}
	if f, ok := job.Config.Flavors[flavor]; ok {
		for k, v := range f.Args {
			args[k] = v
		}
	}
	for k, v := range r.ParentArgs(job.Config.DependsOn) {
		args[k] = v
	}

	kind := job.Kind
	if kind == schedule.Refresh {
		switch {
		case strings.TrimSpace(job.Config.RefreshCmd) == "":
			kind, res.Note = schedule.Rebuild, "no refresh_cmd configured; rebuilding instead"
		default:
			ok, err := r.havePrevious(ctx, aliasRef, log)
			if err != nil {
				return nil, err
			}
			if !ok {
				kind, res.Note = schedule.Rebuild, "no previous image to refresh; rebuilding instead"
			}
		}
	}
	res.Kind = kind

	switch kind {
	case schedule.Rebuild:
		spec := Spec{
			Tag:        candidate,
			ContextDir: job.ContextDir,
			Dockerfile: filepath.Join(job.ContextDir, "Dockerfile"),
			Args:       args,
			NoCache:    true,
			Pull:       true,
		}
		if err := r.Docker.Build(ctx, spec, log); err != nil {
			return nil, withOutput("build", err)
		}
	case schedule.Refresh:
		empty, err := os.MkdirTemp("", "prebake-refresh-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(empty)
		spec := Spec{
			Tag:        candidate,
			ContextDir: empty,
			Body:       RefreshDockerfile(aliasRef, job.Config.RefreshCmd),
			NoCache:    true, // the point of a refresh is to see new upstream packages
		}
		if err := r.Docker.Build(ctx, spec, log); err != nil {
			return nil, withOutput("refresh", err)
		}
	default:
		return nil, fmt.Errorf("cannot run job of kind %v", kind)
	}
	// the candidate only exists locally; always drop it, promoted or not
	defer r.Docker.Remove(context.WithoutCancel(ctx), candidate)

	for _, t := range job.Config.Tests {
		out, err := r.Docker.Test(ctx, candidate, t)
		if err != nil {
			return nil, &TestError{Cmd: t, Output: out, Err: err}
		}
	}

	// promote: date tag first, alias only after the date tag is safely pushed
	if err := r.Docker.Tag(ctx, candidate, dateRef); err != nil {
		return nil, err
	}
	if err := r.Docker.Push(ctx, dateRef, log); err != nil {
		return nil, withOutput("push "+dateRef, err)
	}
	if err := r.Docker.Tag(ctx, candidate, aliasRef); err != nil {
		return nil, err
	}
	if err := r.Docker.Push(ctx, aliasRef, log); err != nil {
		return nil, withOutput("push "+aliasRef, err)
	}
	digest, err := r.Docker.Digest(ctx, dateRef)
	if err != nil {
		return nil, err
	}
	res.Digest = digest
	res.Duration = r.now().Sub(started)
	return res, nil
}

// havePrevious reports whether the previous image of a flavor is available,
// pulling it from the registry when it is not in the local store.
func (r *Runner) havePrevious(ctx context.Context, ref string, log io.Writer) (bool, error) {
	ok, err := r.Docker.Exists(ctx, ref)
	if err != nil || ok {
		return ok, err
	}
	if err := r.Docker.Pull(ctx, ref, log); err != nil {
		return false, nil // not in the registry either: caller falls back to a rebuild
	}
	return true, nil
}

// RefreshDockerfile is the generated Dockerfile of a refresh build: the previous
// image plus the configured refresh command.
func RefreshDockerfile(previous, refreshCmd string) string {
	return "FROM " + previous + "\nRUN " + strings.TrimSpace(refreshCmd) + "\n"
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	max int
	buf []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

// excerpt returns the last n non-empty lines.
func (t *tailWriter) excerpt(n int) string {
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(string(t.buf), "\r", "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
