package registry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/registry/storage"
	"github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/distribution/distribution/v3/registry/storage/driver/factory"
	"github.com/distribution/reference"

	"github.com/schorsch3000/prebake/internal/retention"
)

// Tags and the digests they point to, split into date tags and the rest.
type repoTags struct {
	all    []string
	digest map[string]string
}

func loadTags(ctx context.Context, c *Client, repo string) (*repoTags, error) {
	tags, err := c.Tags(ctx, repo)
	if err != nil {
		return nil, err
	}
	rt := &repoTags{all: tags, digest: map[string]string{}}
	for _, t := range tags {
		d, err := c.Digest(ctx, repo, t)
		if err != nil {
			return nil, err
		}
		rt.digest[t] = d
	}
	return rt, nil
}

// protectedTags returns the date tags whose manifest an alias currently points
// to (every non-date tag counts as an alias). Those are the promoted images.
func (rt *repoTags) protectedTags() map[string]bool {
	aliasDigests := map[string]bool{}
	for _, t := range rt.all {
		if _, _, ok := retention.Parse(t); !ok {
			aliasDigests[rt.digest[t]] = true
		}
	}
	protected := map[string]bool{}
	for _, t := range rt.all {
		if _, _, ok := retention.Parse(t); ok && aliasDigests[rt.digest[t]] {
			protected[t] = true
		}
	}
	return protected
}

// PruneRepo untags the date tags of one repository beyond keep (see
// retention.Plan) and returns them. It does not collect garbage; call GC
// afterwards, while holding the build lock.
func PruneRepo(ctx context.Context, c *Client, repo string, keep int) ([]string, error) {
	rt, err := loadTags(ctx, c, repo)
	if err != nil {
		return nil, err
	}
	del := retention.Plan(rt.all, keep, rt.protectedTags())
	var done []string
	for _, t := range del {
		if err := c.Untag(ctx, repo, t); err != nil {
			return done, err
		}
		done = append(done, t)
	}
	return done, nil
}

// PruneAll prunes every repository for which keepFor reports a keep value;
// repositories the config does not manage are left alone. The result maps
// repository to the tags removed.
func PruneAll(ctx context.Context, c *Client, keepFor func(repo string) (int, bool)) (map[string][]string, error) {
	repos, err := c.Repositories(ctx)
	if err != nil {
		return nil, err
	}
	sort.Strings(repos)
	removed := map[string][]string{}
	var errs []error
	for _, repo := range repos {
		keep, managed := keepFor(repo)
		if !managed {
			continue
		}
		done, err := PruneRepo(ctx, c, repo, keep)
		if len(done) > 0 {
			removed[repo] = done
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repo, err))
		}
	}
	return removed, errors.Join(errs...)
}

// GC removes manifests no tag points to and every blob nothing references. The
// caller must hold the build lock: the daemon serializes its own pushes and the
// external listener is read-only, so nothing writes while this runs.
func (s *Server) GC(ctx context.Context) error {
	return GC(ctx, s.cfg.StorageDir())
}

// GC runs garbage collection on the registry storage at dir.
func GC(ctx context.Context, dir string) error {
	drv, err := factory.Create(ctx, "filesystem", map[string]any{"rootdirectory": dir})
	if err != nil {
		return err
	}
	reg, err := storage.NewRegistry(ctx, drv, storage.EnableDelete)
	if err != nil {
		return err
	}
	err = storage.MarkAndSweep(ctx, drv, reg, storage.GCOpts{RemoveUntagged: true, Quiet: true})
	var nf driver.PathNotFoundError
	if errors.As(err, &nf) || (err != nil && strings.Contains(err.Error(), "Path not found") && strings.Contains(err.Error(), "/repositories")) {
		return nil // nothing was ever pushed: nothing to collect
	}
	if err != nil {
		return err
	}
	return removeEmptyRepositories(ctx, drv, reg)
}

const repositoriesRoot = "/docker/registry/v2/repositories"

// removeEmptyRepositories deletes the repositories that have no tag left. The sweep above removes
// their manifests and blobs but leaves the empty repository in the catalog for ever, so a
// container that was removed from the config would still be listed. Nothing pushes while this
// runs (the caller holds the build lock), so a repository without tags is really empty.
func removeEmptyRepositories(ctx context.Context, drv driver.StorageDriver, reg distribution.Namespace) error {
	enum, ok := reg.(distribution.RepositoryEnumerator)
	if !ok {
		return errors.New("registry cannot enumerate repositories")
	}
	var empty []string
	err := enum.Enumerate(ctx, func(name string) error {
		named, err := reference.WithName(name)
		if err != nil {
			return nil // not a repository name we could have created
		}
		repo, err := reg.Repository(ctx, named)
		if err != nil {
			return err
		}
		tags, err := repo.Tags(ctx).All(ctx)
		var unknown distribution.ErrRepositoryUnknown
		if err != nil && !errors.As(err, &unknown) {
			return err
		}
		if len(tags) == 0 {
			empty = append(empty, name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range empty {
		if err := drv.Delete(ctx, repositoriesRoot+"/"+name); err != nil {
			errs = append(errs, fmt.Errorf("remove empty repository %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
