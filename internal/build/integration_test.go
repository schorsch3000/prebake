//go:build integration

// Integration tests run the pipeline against a real Docker daemon and a real
// registry. Run with:
//
//	docker run -d -p 5001:5000 registry:2
//	go test -tags integration -count=1 ./internal/build/
//
// PREBAKE_TEST_REGISTRY overrides the registry address (default localhost:5001).
// The tests skip themselves when Docker or the registry is not reachable.
package build

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/schorsch3000/prebake/internal/config"
	"github.com/schorsch3000/prebake/internal/schedule"
)

func registryAddr() string {
	if a := os.Getenv("PREBAKE_TEST_REGISTRY"); a != "" {
		return a
	}
	return "localhost:5001"
}

// integrationRunner skips the test unless Docker and the registry work, and
// returns a runner whose clock advances one minute per call so that date tags
// never collide.
func integrationRunner(t *testing.T) *Runner {
	t.Helper()
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker not available: %v: %s", err, bytes.TrimSpace(out))
	}
	resp, err := http.Get("http://" + registryAddr() + "/v2/")
	if err != nil {
		t.Skipf("registry %s not reachable: %v", registryAddr(), err)
	}
	resp.Body.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &Runner{
		Docker:   &CLI{},
		Registry: registryAddr(),
		Now: func() time.Time {
			now = now.Add(time.Minute)
			return now
		},
	}
}

// uniqueName avoids clashes between test runs sharing one registry.
func uniqueName(t *testing.T, base string) string {
	return fmt.Sprintf("%s-%d", base, time.Now().UnixNano()%1_000_000_000)
}

func tags(t *testing.T, name string) []string {
	t.Helper()
	resp, err := http.Get("http://" + registryAddr() + "/v2/" + name + "/tags/list")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	var body struct{ Tags []string }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Tags
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func writeCtx(t *testing.T, dockerfile string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func cleanup(t *testing.T, r *Runner, name string, res *Result) {
	t.Cleanup(func() {
		for _, tag := range []string{res.DateTag, res.Alias} {
			exec.Command("docker", "image", "rm", "-f", r.Ref(name, tag)).Run()
		}
	})
}

func TestIntegrationRebuildRefreshAndFailedTest(t *testing.T) {
	r := integrationRunner(t)
	ctx := context.Background()
	name := uniqueName(t, "itest")
	cfg := &config.Container{RefreshCmd: "echo refreshed > /refreshed", Tests: []string{"true"}}
	job := Job{Container: name, Flavor: "default", Kind: schedule.Rebuild, Config: cfg,
		ContextDir: writeCtx(t, "FROM busybox:stable\nRUN echo base > /base\n")}
	var log bytes.Buffer

	// rebuild: pushes the date tag and moves the alias
	res, err := r.Run(ctx, job, &log)
	if err != nil {
		t.Fatalf("rebuild: %v\n%s", err, log.String())
	}
	cleanup(t, r, name, res)
	got := tags(t, name)
	if !contains(got, "latest") || !contains(got, res.DateTag) || !strings.HasPrefix(res.Digest, "sha256:") {
		t.Fatalf("after rebuild: tags=%v result=%+v", got, res)
	}

	// refresh: layered on the previous image, with the refresh command applied
	job.Kind = schedule.Refresh
	job.Config = &config.Container{RefreshCmd: cfg.RefreshCmd, Tests: []string{"test -f /base && test -f /refreshed"}}
	log.Reset()
	res2, err := r.Run(ctx, job, &log)
	if err != nil {
		t.Fatalf("refresh: %v\n%s", err, log.String())
	}
	cleanup(t, r, name, res2)
	if res2.Kind != schedule.Refresh || res2.Digest == res.Digest || res2.DateTag == res.DateTag {
		t.Fatalf("refresh result: %+v (first: %+v)", res2, res)
	}

	// failing smoke test: error, nothing new pushed, alias unchanged
	before := tags(t, name)
	job.Kind = schedule.Rebuild
	job.Config = &config.Container{Tests: []string{"false"}}
	_, err = r.Run(ctx, job, io.Discard)
	if _, ok := err.(*TestError); !ok {
		t.Fatalf("want *TestError, got %v", err)
	}
	if after := tags(t, name); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("failed test changed tags: before=%v after=%v", before, after)
	}

	// failing build: also leaves the registry untouched
	job.Config = &config.Container{}
	job.ContextDir = writeCtx(t, "FROM busybox:stable\nRUN exit 1\n")
	if _, err = r.Run(ctx, job, io.Discard); err == nil {
		t.Fatal("broken Dockerfile must fail")
	}
	if after := tags(t, name); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("failed build changed tags: before=%v after=%v", before, after)
	}
}

func TestIntegrationParentChildAndFlavor(t *testing.T) {
	r := integrationRunner(t)
	ctx := context.Background()
	parent, child := uniqueName(t, "iparent"), uniqueName(t, "ichild")
	var log bytes.Buffer

	pres, err := r.Run(ctx, Job{Container: parent, Flavor: "default", Kind: schedule.Rebuild,
		Config:     &config.Container{},
		ContextDir: writeCtx(t, "FROM busybox:stable\nRUN echo parent > /parent\n")}, &log)
	if err != nil {
		t.Fatalf("parent: %v\n%s", err, log.String())
	}
	cleanup(t, r, parent, pres)

	// the child starts FROM the parent's latest through the PARENT build arg and
	// is built as a non-default flavor
	cres, err := r.Run(ctx, Job{Container: child, Flavor: "cuda", Kind: schedule.Rebuild,
		Config: &config.Container{
			DependsOn: []string{parent},
			Flavors:   map[string]config.Flavor{"cuda": {Args: map[string]string{"FLAVOR_MARK": "cuda"}}},
			Tests:     []string{"test -f /parent && test \"$(cat /mark)\" = cuda"},
		},
		ContextDir: writeCtx(t, "ARG PARENT\nARG FLAVOR_MARK\nFROM ${PARENT}\nARG FLAVOR_MARK\nRUN echo $FLAVOR_MARK > /mark\n")}, &log)
	if err != nil {
		t.Fatalf("child: %v\n%s", err, log.String())
	}
	cleanup(t, r, child, cres)
	if cres.Alias != "cuda" || !contains(tags(t, child), "cuda") {
		t.Fatalf("flavor alias: %+v tags=%v", cres, tags(t, child))
	}
}

// A rebuild runs the refresh_cmd once on the new image; a broken one stops the promotion.
func TestIntegrationRebuildVerifiesTheRefreshCommand(t *testing.T) {
	r := integrationRunner(t)
	ctx := context.Background()
	name := uniqueName(t, "irefresh")
	ctxDir := writeCtx(t, "FROM busybox:stable\nRUN echo base > /base\n")

	broken := &config.Container{RefreshCmd: "echo apt-get-not-here >&2; exit 3"}
	_, err := r.Run(ctx, Job{Container: name, Flavor: "default", Kind: schedule.Rebuild, Config: broken, ContextDir: ctxDir}, io.Discard)
	te, ok := err.(*TestError)
	if !ok || te.Kind != "refresh_cmd check" || !strings.Contains(te.Output, "apt-get-not-here") {
		t.Fatalf("a broken refresh_cmd must fail the rebuild with its output: %v", err)
	}
	if got := tags(t, name); len(got) != 0 {
		t.Errorf("nothing may be pushed: %v", got)
	}

	working := &config.Container{RefreshCmd: "echo refreshed > /refreshed"}
	res, err := r.Run(ctx, Job{Container: name, Flavor: "default", Kind: schedule.Rebuild, Config: working, ContextDir: ctxDir}, io.Discard)
	if err != nil || !res.RefreshChecked {
		t.Fatalf("%+v %v", res, err)
	}
	cleanup(t, r, name, res)
	if got := tags(t, name); !contains(got, "latest") {
		t.Errorf("promoted: %v", got)
	}
}
