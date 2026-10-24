package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/schorsch3000/prebake/internal/config"
	"github.com/schorsch3000/prebake/internal/schedule"
)

// fake records calls and fails on demand.
type fake struct {
	calls  []string
	specs  []Spec
	local  map[string]bool // refs in the local store
	remote map[string]bool // refs pullable
	failOn map[string]error
	runOut map[string]string

	buildOutput string // written to the build log before a failing build returns
	pushOutput  string // same for a failing push
}

func newFake() *fake {
	return &fake{local: map[string]bool{}, remote: map[string]bool{}, failOn: map[string]error{}, runOut: map[string]string{}}
}

func (f *fake) rec(op string, a ...string) error {
	f.calls = append(f.calls, strings.Join(append([]string{op}, a...), " "))
	return f.failOn[op]
}

func (f *fake) Build(_ context.Context, s Spec, log io.Writer) error {
	f.specs = append(f.specs, s)
	if f.failOn["build"] != nil && f.buildOutput != "" {
		io.WriteString(log, f.buildOutput)
	}
	if err := f.rec("build", s.Tag); err != nil {
		return err
	}
	f.local[s.Tag] = true
	return nil
}
func (f *fake) Test(_ context.Context, image, cmd string) (string, error) {
	err := f.rec("run", cmd)
	if _, bad := f.failOn["run:"+cmd]; bad {
		return f.runOut[cmd], f.failOn["run:"+cmd]
	}
	return "", err
}
func (f *fake) Tag(_ context.Context, src, dst string) error {
	if err := f.rec("tag", dst); err != nil {
		return err
	}
	f.local[dst] = true
	return nil
}
func (f *fake) Push(_ context.Context, ref string, log io.Writer) error {
	if f.failOn["push:"+ref] != nil && f.pushOutput != "" {
		io.WriteString(log, f.pushOutput)
	}
	if err := f.rec("push", ref); err != nil {
		return err
	}
	return f.failOn["push:"+ref]
}
func (f *fake) Pull(_ context.Context, ref string, _ io.Writer) error {
	f.rec("pull", ref)
	if !f.remote[ref] {
		return errors.New("not found")
	}
	f.local[ref] = true
	return nil
}
func (f *fake) Exists(_ context.Context, ref string) (bool, error) { return f.local[ref], nil }
func (f *fake) Digest(_ context.Context, ref string) (string, error) {
	f.rec("digest", ref)
	return "sha256:abc", f.failOn["digest"]
}
func (f *fake) Remove(_ context.Context, ref string) error {
	f.rec("remove", ref)
	delete(f.local, ref)
	return nil
}

var fixed = time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)

func runner(f *fake) *Runner {
	return &Runner{Docker: f, Registry: "localhost:5001", Now: func() time.Time { return fixed }}
}

func job(kind schedule.Kind, c *config.Container) Job {
	return Job{Container: "dev", Flavor: "default", Kind: kind, ContextDir: "/repo/containers/dev", Config: c}
}

func index(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func TestRebuildHappyPath(t *testing.T) {
	f := newFake()
	c := &config.Container{Tests: []string{"git --version"}, DependsOn: []string{"base-img"},
		Flavors: map[string]config.Flavor{"default": {Args: map[string]string{"X": "1"}}}}
	res, err := runner(f).Run(context.Background(), job(schedule.Rebuild, c), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != schedule.Rebuild || res.DateTag != "default-20261022-020000" || res.Alias != "latest" || res.Digest != "sha256:abc" {
		t.Errorf("result = %+v", res)
	}
	s := f.specs[0]
	if !s.NoCache || !s.Pull || s.Dockerfile != "/repo/containers/dev/Dockerfile" || s.Tag != "localhost:5001/dev:default-20261022-020000-candidate" {
		t.Errorf("spec = %+v", s)
	}
	if s.Args["X"] != "1" || s.Args["PARENT"] != "localhost:5001/base-img:latest" || s.Args["PARENT_BASE_IMG"] != "localhost:5001/base-img:latest" {
		t.Errorf("args = %v", s.Args)
	}
	// order: build, test, push date tag, then alias, candidate removed
	b, ru := index(f.calls, "build"), index(f.calls, "run")
	pd, pa := index(f.calls, "push localhost:5001/dev:default-20261022-020000"), index(f.calls, "push localhost:5001/dev:latest")
	if !(b < ru && ru < pd && pd < pa) {
		t.Errorf("order wrong: %v", f.calls)
	}
	if index(f.calls, "remove localhost:5001/dev:default-20261022-020000-candidate") < 0 {
		t.Errorf("candidate not removed: %v", f.calls)
	}
}

func TestFailedBuildNeverPromotes(t *testing.T) {
	f := newFake()
	f.failOn["build"] = errors.New("apt exploded")
	_, err := runner(f).Run(context.Background(), job(schedule.Rebuild, &config.Container{}), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "apt exploded") {
		t.Fatalf("err = %v", err)
	}
	if index(f.calls, "push") >= 0 || index(f.calls, "tag") >= 0 {
		t.Errorf("nothing may be tagged or pushed: %v", f.calls)
	}
}

func TestFailedTestNeverPromotes(t *testing.T) {
	f := newFake()
	f.failOn["run:bad"] = errors.New("exit 1")
	f.runOut["bad"] = "nope"
	_, err := runner(f).Run(context.Background(), job(schedule.Rebuild, &config.Container{Tests: []string{"good", "bad", "never"}}), io.Discard)
	var te *TestError
	if !errors.As(err, &te) || te.Cmd != "bad" || te.Output != "nope" {
		t.Fatalf("err = %v", err)
	}
	if index(f.calls, "push") >= 0 || index(f.calls, "run never") >= 0 {
		t.Errorf("calls = %v", f.calls)
	}
	if index(f.calls, "remove") < 0 {
		t.Errorf("candidate must be cleaned up: %v", f.calls)
	}
}

func TestAliasOnlyMovesAfterDateTagPush(t *testing.T) {
	f := newFake()
	f.failOn["push:localhost:5001/dev:default-20261022-020000"] = errors.New("registry down")
	_, err := runner(f).Run(context.Background(), job(schedule.Rebuild, &config.Container{}), io.Discard)
	if err == nil {
		t.Fatal("want error")
	}
	if index(f.calls, "push localhost:5001/dev:latest") >= 0 || index(f.calls, "tag localhost:5001/dev:latest") >= 0 {
		t.Errorf("alias moved despite failed push: %v", f.calls)
	}
}

func TestNoTestsStillPromotes(t *testing.T) {
	f := newFake()
	if _, err := runner(f).Run(context.Background(), job(schedule.Rebuild, &config.Container{}), io.Discard); err != nil {
		t.Fatal(err)
	}
	if index(f.calls, "push localhost:5001/dev:latest") < 0 {
		t.Errorf("calls = %v", f.calls)
	}
}

func TestFlavorAlias(t *testing.T) {
	f := newFake()
	j := job(schedule.Rebuild, &config.Container{Flavors: map[string]config.Flavor{"cuda": {Args: map[string]string{"CUDA": "1"}}}})
	j.Flavor = "cuda"
	res, err := runner(f).Run(context.Background(), j, io.Discard)
	if err != nil || res.Alias != "cuda" || res.DateTag != "cuda-20261022-020000" {
		t.Fatalf("%+v %v", res, err)
	}
	if f.specs[0].Args["CUDA"] != "1" {
		t.Errorf("flavor args missing: %v", f.specs[0].Args)
	}
}

func TestRefresh(t *testing.T) {
	f := newFake()
	f.local["localhost:5001/dev:latest"] = true
	c := &config.Container{RefreshCmd: "apt-get update && apt-get upgrade -y"}
	res, err := runner(f).Run(context.Background(), job(schedule.Refresh, c), io.Discard)
	if err != nil || res.Kind != schedule.Refresh {
		t.Fatalf("%+v %v", res, err)
	}
	s := f.specs[0]
	want := RefreshDockerfile("localhost:5001/dev:latest", "apt-get update && apt-get upgrade -y")
	if want != "FROM localhost:5001/dev:latest\nRUN [\"sh\", \"-c\", \"apt-get update \\u0026\\u0026 apt-get upgrade -y\"]\n" {
		t.Fatalf("the refresh step is the exec-form step shared with the tests: %q", want)
	}
	if s.Body != want || s.Dockerfile != "" || !s.NoCache {
		t.Errorf("spec = %+v", s)
	}
}

func TestRefreshFallsBackToRebuild(t *testing.T) {
	// no refresh_cmd
	f := newFake()
	res, err := runner(f).Run(context.Background(), job(schedule.Refresh, &config.Container{}), io.Discard)
	if err != nil || res.Kind != schedule.Rebuild || !strings.Contains(res.Note, "no refresh_cmd") || !f.specs[0].NoCache || f.specs[0].Body != "" {
		t.Errorf("%+v %v %+v", res, err, f.specs)
	}
	// no previous image locally or in the registry
	f = newFake()
	res, err = runner(f).Run(context.Background(), job(schedule.Refresh, &config.Container{RefreshCmd: "x"}), io.Discard)
	if err != nil || res.Kind != schedule.Rebuild || !strings.Contains(res.Note, "no previous image") {
		t.Errorf("%+v %v", res, err)
	}
	// previous only in the registry: pulled, then refreshed
	f = newFake()
	f.remote["localhost:5001/dev:latest"] = true
	res, err = runner(f).Run(context.Background(), job(schedule.Refresh, &config.Container{RefreshCmd: "x"}), io.Discard)
	if err != nil || res.Kind != schedule.Refresh || index(f.calls, "pull localhost:5001/dev:latest") < 0 {
		t.Errorf("%+v %v %v", res, err, f.calls)
	}
}

func TestParentArgs(t *testing.T) {
	r := runner(newFake())
	got := r.ParentArgs([]string{"base", "tools.v2"})
	if got["PARENT"] != "localhost:5001/base:latest" || got["PARENT_TOOLS_V2"] != "localhost:5001/tools.v2:latest" || got["PARENT_BASE"] == "" {
		t.Errorf("%v", got)
	}
	if len(r.ParentArgs(nil)) != 0 {
		t.Error("no parents => no args")
	}
}

// --- CLI wrapper: assert the exact docker invocations -----------------------------------

type recorded struct {
	args  []string
	stdin string
}

func cli(out string, err error) (*CLI, *[]recorded) {
	var rec []recorded
	return &CLI{Exec: func(_ context.Context, stdin io.Reader, stdout, _ io.Writer, name string, args ...string) error {
		r := recorded{args: append([]string{name}, args...)}
		if stdin != nil {
			b, _ := io.ReadAll(stdin)
			r.stdin = string(b)
		}
		rec = append(rec, r)
		io.WriteString(stdout, out)
		return err
	}}, &rec
}

func TestCLIBuildArgs(t *testing.T) {
	c, rec := cli("", nil)
	err := c.Build(context.Background(), Spec{Tag: "t:1", ContextDir: "/ctx", Dockerfile: "/ctx/Dockerfile",
		Args: map[string]string{"B": "2", "A": "1"}, NoCache: true, Pull: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join((*rec)[0].args, " ")
	want := "docker buildx build --platform linux/amd64 --load --progress=plain -t t:1 --no-cache --pull --build-arg A=1 --build-arg B=2 -f /ctx/Dockerfile /ctx"
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
	c, rec = cli("", nil)
	c.Build(context.Background(), Spec{Tag: "t:2", ContextDir: "/e", Body: "FROM x\n"}, io.Discard)
	if got := strings.Join((*rec)[0].args, " "); !strings.HasSuffix(got, "-f - /e") || (*rec)[0].stdin != "FROM x\n" {
		t.Errorf("stdin build: %s %q", got, (*rec)[0].stdin)
	}
}

func TestTestDockerfileKeepsCommandIntact(t *testing.T) {
	got := TestDockerfile("localhost:5001/dev:x-candidate", `echo "a b" && test -n '$HOME' # <&>`+"\nfalse")
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 || lines[0] != "FROM localhost:5001/dev:x-candidate" {
		t.Fatalf("a multi-line command must stay on one Dockerfile line:\n%s", got)
	}
	var argv []string
	if !strings.HasPrefix(lines[1], "RUN ") || json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "RUN ")), &argv) != nil {
		t.Fatalf("not an exec-form RUN: %q", lines[1])
	}
	if len(argv) != 3 || argv[0] != "sh" || argv[1] != "-c" || argv[2] != `echo "a b" && test -n '$HOME' # <&>`+"\nfalse" {
		t.Errorf("argv = %q", argv)
	}
}

func TestCLITestRunsAsBuildAndCleansUp(t *testing.T) {
	var calls [][]string
	var stdins []string
	c := &CLI{Exec: func(_ context.Context, stdin io.Reader, stdout, _ io.Writer, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		in := ""
		if stdin != nil {
			b, _ := io.ReadAll(stdin)
			in = string(b)
		}
		stdins = append(stdins, in)
		if len(args) > 0 && args[0] == "buildx" {
			io.WriteString(stdout, "#5 RUN sh -c false\n#5 ERROR: process did not complete successfully: exit code: 1\n")
			return fmt.Errorf("exit 1")
		}
		return nil
	}}
	out, err := c.Test(context.Background(), "reg/dev:c", "false")
	if err == nil || !strings.Contains(out, "exit code: 1") {
		t.Fatalf("failing test must return the build output: %q %v", out, err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
	build := strings.Join(calls[0], " ")
	for _, want := range []string{"buildx build", "--no-cache", "-t reg/dev:c-test", "-f -"} {
		if !strings.Contains(build, want) {
			t.Errorf("build call lacks %q: %s", want, build)
		}
	}
	if strings.Contains(build, "--load=false") || strings.Contains(strings.Join(calls[0], " "), " run ") {
		t.Errorf("no container may be started: %s", build)
	}
	if stdins[0] != TestDockerfile("reg/dev:c", "false") {
		t.Errorf("dockerfile on stdin = %q", stdins[0])
	}
	if got := strings.Join(calls[1], " "); got != "docker image rm reg/dev:c-test" {
		t.Errorf("the test image must be removed even after a failure: %s", got)
	}
	for _, c := range calls {
		for _, a := range c {
			if a == "run" {
				t.Errorf("docker run used: %v", c)
			}
		}
	}
}

func TestCLIOthers(t *testing.T) {
	ctx := context.Background()
	c, _ := cli(`["localhost:5001/dev@sha256:aaa","other/dev@sha256:bbb"]`, nil)
	if d, err := c.Digest(ctx, "localhost:5001/dev:latest"); err != nil || d != "sha256:aaa" {
		t.Errorf("digest %q %v", d, err)
	}
	c, _ = cli(`[]`, nil)
	if _, err := c.Digest(ctx, "localhost:5001/dev:latest"); err == nil {
		t.Error("no digest must error")
	}
	c, _ = cli("Error: No such image: x", fmt.Errorf("exit 1"))
	if ok, err := c.Exists(ctx, "x"); ok || err != nil {
		t.Errorf("exists: %v %v", ok, err)
	}
	c, _ = cli("daemon not running", fmt.Errorf("exit 1"))
	if _, err := c.Exists(ctx, "x"); err == nil {
		t.Error("other failures must surface")
	}
}

func TestFailuresCarryTheDockerOutput(t *testing.T) {
	f := newFake()
	f.failOn["build"] = errors.New("exit status 1")
	f.buildOutput = "noise\n\nCannot connect to the Docker daemon at tcp://docker-proxy:2375. Is the docker daemon running?\n"
	_, err := runner(f).Run(context.Background(), job(schedule.Rebuild, &config.Container{}), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "exit status 1") || !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Fatalf("the reason for a failed build must be in the error: %v", err)
	}

	f = newFake()
	f.failOn["push:localhost:5001/dev:default-20261022-020000"] = errors.New("exit status 1")
	f.pushOutput = "denied: requested access to the resource is denied\n"
	_, err = runner(f).Run(context.Background(), job(schedule.Rebuild, &config.Container{}), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "denied: requested access") {
		t.Fatalf("push errors carry the output too: %v", err)
	}

	// the excerpt is bounded to the last lines
	var long strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&long, "line %d\n", i)
	}
	f = newFake()
	f.failOn["build"] = errors.New("exit status 1")
	f.buildOutput = long.String()
	_, err = runner(f).Run(context.Background(), job(schedule.Rebuild, &config.Container{}), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "line 99") || strings.Contains(err.Error(), "line 50\n") || strings.Count(err.Error(), "\n") > 16 {
		t.Errorf("excerpt not bounded: %v", err)
	}
}

// --- refresh_cmd check on rebuilds -----------------------------------------------------

func TestRebuildVerifiesTheRefreshCommandBeforePromoting(t *testing.T) {
	f := newFake()
	c := &config.Container{Tests: []string{"git --version"}, RefreshCmd: "apt-get update && apt-get upgrade -y"}
	res, err := runner(f).Run(context.Background(), job(schedule.Rebuild, c), io.Discard)
	if err != nil || !res.RefreshChecked {
		t.Fatalf("%+v %v", res, err)
	}
	smoke, refresh, push := index(f.calls, "run git --version"), index(f.calls, "run apt-get update && apt-get upgrade -y"), index(f.calls, "push")
	if !(smoke >= 0 && smoke < refresh && refresh < push) {
		t.Errorf("order: smoke test, then the refresh_cmd check, then promote: %v", f.calls)
	}
}

func TestBrokenRefreshCommandStopsThePromotion(t *testing.T) {
	f := newFake()
	f.failOn["run:apt-get updat"] = errors.New("exit 127")
	f.runOut["apt-get updat"] = "sh: 1: apt-get updat: not found"
	c := &config.Container{RefreshCmd: "apt-get updat"}
	_, err := runner(f).Run(context.Background(), job(schedule.Rebuild, c), io.Discard)
	var te *TestError
	if !errors.As(err, &te) || te.Kind != "refresh_cmd check" || te.Cmd != "apt-get updat" || !strings.Contains(te.Output, "not found") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), `refresh_cmd check "apt-get updat" failed`) {
		t.Errorf("the message names the check: %v", err)
	}
	if index(f.calls, "push") >= 0 || index(f.calls, "tag") >= 0 {
		t.Errorf("a broken refresh must not be promoted: %v", f.calls)
	}
	if index(f.calls, "remove") < 0 {
		t.Errorf("candidate cleaned up: %v", f.calls)
	}
	// the smoke-test error keeps its wording
	f2 := newFake()
	f2.failOn["run:bad"] = errors.New("exit 1")
	_, err = runner(f2).Run(context.Background(), job(schedule.Rebuild, &config.Container{Tests: []string{"bad"}}), io.Discard)
	if !strings.Contains(err.Error(), `smoke test "bad" failed`) {
		t.Errorf("%v", err)
	}
}

func TestRefreshJobsAndCommandlessContainersSkipTheCheck(t *testing.T) {
	// a refresh IS the refresh_cmd: no second run
	f := newFake()
	f.local["localhost:5001/dev:latest"] = true
	res, err := runner(f).Run(context.Background(), job(schedule.Refresh, &config.Container{RefreshCmd: "echo hi"}), io.Discard)
	if err != nil || res.RefreshChecked || index(f.calls, "run echo hi") >= 0 {
		t.Errorf("refresh: %+v %v %v", res, err, f.calls)
	}
	// nothing to verify without a refresh_cmd
	f = newFake()
	res, err = runner(f).Run(context.Background(), job(schedule.Rebuild, &config.Container{}), io.Discard)
	if err != nil || res.RefreshChecked {
		t.Errorf("no refresh_cmd: %+v %v", res, err)
	}
	f = newFake()
	if res, _ := runner(f).Run(context.Background(), job(schedule.Rebuild, &config.Container{RefreshCmd: "   "}), io.Discard); res.RefreshChecked || index(f.calls, "run") >= 0 {
		t.Errorf("a blank refresh_cmd is none: %v", f.calls)
	}
	// a refresh that falls back to a rebuild (no previous image) is a rebuild: it is checked
	f = newFake()
	res, err = runner(f).Run(context.Background(), job(schedule.Refresh, &config.Container{RefreshCmd: "echo hi"}), io.Discard)
	if err != nil || res.Kind != schedule.Rebuild || !res.RefreshChecked {
		t.Errorf("fallback rebuild: %+v %v", res, err)
	}
}
