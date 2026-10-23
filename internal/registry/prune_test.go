package registry

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// copyTag makes dst point to the same manifest as src (same digest).
func copyTag(t *testing.T, base, repo, src, dst string) {
	t.Helper()
	resp := do(t, http.DefaultClient, "GET", base+"/v2/"+repo+"/manifests/"+src, nil, map[string]string{"Accept": ociType})
	body, _ := io.ReadAll(resp.Body)
	resp = do(t, http.DefaultClient, "PUT", base+"/v2/"+repo+"/manifests/"+dst, body, map[string]string{"Content-Type": ociType})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("copy %s -> %s: %d", src, dst, resp.StatusCode)
	}
}

func sorted(s []string) []string { sort.Strings(s); return s }

func TestPruneAndGC(t *testing.T) {
	s := start(t, nil)
	base := "http://" + s.WriteAddr()
	c := &Client{Base: base}
	ctx := context.Background()
	const repo = "demo/app"

	d := map[string]string{} // tag -> digest
	for _, tag := range []string{"default-20261001-020000", "default-20261002-020000", "default-20261003-020000", "default-20261004-020000"} {
		d[tag] = push(t, base, repo, tag)
	}
	// the alias points at an OLD image (as after a failed newer build is cleaned up)
	copyTag(t, base, repo, "default-20261002-020000", "latest")
	// another repository that the config does not manage
	push(t, base, "other/thing", "default-20260901-020000")
	push(t, base, "other/thing", "default-20260902-020000")

	removed, err := PruneAll(ctx, c, func(r string) (int, bool) { return 2, r == repo })
	if err != nil {
		t.Fatal(err)
	}
	// keep newest 2 (…03, …04), …02 is promoted, so only …01 goes
	if want := map[string][]string{repo: {"default-20261001-020000"}}; !reflect.DeepEqual(removed, want) {
		t.Fatalf("removed = %v, want %v", removed, want)
	}
	tags, _ := c.Tags(ctx, repo)
	if got := strings.Join(sorted(tags), ","); got != "default-20261002-020000,default-20261003-020000,default-20261004-020000,latest" {
		t.Errorf("tags = %s", got)
	}
	if tags, _ := c.Tags(ctx, "other/thing"); len(tags) != 2 {
		t.Errorf("unmanaged repo was touched: %v", tags)
	}

	// untagged manifest is still there until GC runs, gone afterwards, rest intact
	manifest := func(ref string) int {
		return do(t, http.DefaultClient, "HEAD", base+"/v2/"+repo+"/manifests/"+ref, nil, map[string]string{"Accept": ociType}).StatusCode
	}
	if got := manifest(d["default-20261001-020000"]); got != 200 {
		t.Errorf("before GC the untagged manifest should remain, got %d", got)
	}
	if err := s.GC(ctx); err != nil {
		t.Fatal(err)
	}
	if got := manifest(d["default-20261001-020000"]); got != 404 {
		t.Errorf("after GC the pruned manifest must be gone, got %d", got)
	}
	for _, tag := range []string{"default-20261002-020000", "default-20261003-020000", "default-20261004-020000", "latest"} {
		if got := manifest(tag); got != 200 {
			t.Errorf("%s: %d after GC", tag, got)
		}
	}
}

func TestPruneSharedDigestDoesNotHurtKeptTag(t *testing.T) {
	s := start(t, nil)
	base := "http://" + s.WriteAddr()
	c := &Client{Base: base}
	ctx := context.Background()
	const repo = "demo/shared"

	push(t, base, repo, "default-20261001-020000")
	push(t, base, repo, "default-20261004-020000")
	// a newer tag with exactly the same content as …04 (identical digest)
	copyTag(t, base, repo, "default-20261004-020000", "default-20261005-020000")

	done, err := PruneRepo(ctx, c, repo, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"default-20261001-020000", "default-20261004-020000"}; !reflect.DeepEqual(done, want) {
		t.Fatalf("done = %v, want %v", done, want)
	}
	if err := s.GC(ctx); err != nil {
		t.Fatal(err)
	}
	// …05 shares its digest with the untagged …04 and must survive GC
	resp := do(t, http.DefaultClient, "GET", base+"/v2/"+repo+"/manifests/default-20261005-020000", nil, map[string]string{"Accept": ociType})
	if resp.StatusCode != 200 {
		t.Fatalf("kept tag lost its manifest: %d", resp.StatusCode)
	}
	if err := func() error { _, err := c.Digest(ctx, repo, "default-20261005-020000"); return err }(); err != nil {
		t.Error(err)
	}
}

func TestGCOnEmptyStore(t *testing.T) {
	s := start(t, nil)
	if err := s.GC(context.Background()); err != nil {
		t.Errorf("empty store: %v", err)
	}
}

func TestPruneKeepsEverythingWhenKeepIsHigh(t *testing.T) {
	s := start(t, nil)
	base := "http://" + s.WriteAddr()
	push(t, base, "demo/x", "default-20261001-020000")
	push(t, base, "demo/x", "latest")
	done, err := PruneRepo(context.Background(), &Client{Base: base}, "demo/x", 5)
	if err != nil || len(done) != 0 {
		t.Errorf("%v %v", done, err)
	}
}

func TestRepositoriesPagination(t *testing.T) {
	s := start(t, nil)
	base := "http://" + s.WriteAddr()
	// more repositories than one catalog page (100)
	const n = 120
	for i := 0; i < n; i++ {
		push(t, base, fmt.Sprintf("many/r%03d", i), "latest")
	}
	repos, err := (&Client{Base: base}).Repositories(context.Background())
	if err != nil || len(repos) != n {
		t.Fatalf("got %d repos, err %v", len(repos), err)
	}
}

// A container that was removed from the config: after all its tags are removed and GC has run,
// its repository must be gone from the catalog too, not linger as an empty entry.
func TestRemovedRepositoryDisappearsFromTheCatalogAfterGC(t *testing.T) {
	s := start(t, nil)
	base := "http://" + s.WriteAddr()
	c := &Client{Base: base}
	ctx := context.Background()
	push(t, base, "gone/app", "latest")
	push(t, base, "gone/app", "default-20261001-020000")
	push(t, base, "stays/app", "latest")
	repos, _ := c.Repositories(ctx)
	if len(repos) != 2 {
		t.Fatalf("setup: %v", repos)
	}
	tags, _ := c.Tags(ctx, "gone/app")
	for _, tag := range tags {
		if err := c.Untag(ctx, "gone/app", tag); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.GC(ctx); err != nil {
		t.Fatal(err)
	}
	repos, err := c.Repositories(ctx)
	if err != nil || len(repos) != 1 || repos[0] != "stays/app" {
		t.Errorf("catalog after GC: %v %v", repos, err)
	}
}
