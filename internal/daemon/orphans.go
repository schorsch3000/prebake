package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/schorsch3000/prebake/internal/config"
	"github.com/schorsch3000/prebake/internal/retention"
	"github.com/schorsch3000/prebake/internal/schedule"
)

// Orphan is a container flavor that is no longer in the config but still has images in the
// registry or a state: it is removed after a grace period.
type Orphan struct {
	Key      schedule.Key
	Since    time.Time // when it was first seen missing from the config
	RemoveAt time.Time // when it will be removed; zero when pruning is switched off
	Tags     []string  // what the registry holds for it
}

// reconcileEvery bounds how often the registry is listed for removed containers.
const reconcileEvery = 5 * time.Minute

// flavorOfTag says which flavor a registry tag belongs to: a date tag to the flavor in its name,
// every other tag is the moving alias of a flavor ("latest" is the default flavor's).
func flavorOfTag(tag string) string {
	if f, _, ok := retention.Parse(tag); ok {
		return f
	}
	if tag == "latest" {
		return config.DefaultFlavor
	}
	return tag
}

// observed lists, per (container, flavor), the tags the registry holds.
func (d *Daemon) observed(ctx context.Context) (map[schedule.Key][]string, error) {
	repos, err := d.registry.Repositories(ctx)
	if err != nil {
		return nil, err
	}
	out := map[schedule.Key][]string{}
	for _, repo := range repos {
		tags, err := d.registry.Tags(ctx, repo)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", repo, err)
		}
		for _, t := range tags {
			k := schedule.Key{Container: repo, Flavor: flavorOfTag(t)}
			out[k] = append(out[k], t)
		}
	}
	return out, nil
}

func (d *Daemon) managedKeys() map[schedule.Key]bool {
	m := map[schedule.Key]bool{}
	for _, k := range d.keys() {
		m[k] = true
	}
	return m
}

// reconcileRemoved finds what is in the registry or the state but no longer in the config and
// removes it once it has been missing for the grace period. A config that lists no containers
// at all is never trusted for this: that is far more likely a mistake than a wish.
func (d *Daemon) reconcileRemoved(ctx context.Context, repo *config.Repo, force bool) {
	now := d.now()
	d.mu.Lock()
	if !force && now.Sub(d.lastReconcile) < reconcileEvery {
		d.mu.Unlock()
		return
	}
	d.lastReconcile = now
	d.mu.Unlock()

	if len(repo.Containers) == 0 {
		d.log.Warn("the config lists no containers: not removing any images")
		d.setOrphans(nil)
		return
	}
	obs, err := d.observed(ctx)
	if err != nil {
		d.log.Warn("cannot list the registry, skipping the check for removed containers", "err", err)
		return
	}
	managed := d.managedKeys()
	all := map[schedule.Key]bool{}
	for k := range obs {
		all[k] = true
	}
	if known, err := d.store.KnownKeys(); err == nil {
		for _, k := range known {
			all[k] = true
		}
	}
	// back in the config: forget that it was missing
	if removed, err := d.store.Removed(); err == nil {
		for k := range removed {
			if managed[k] {
				d.store.ClearRemoved(k)
			}
		}
	}
	prune := repo.Global.Registry.PruneRemoved != nil && *repo.Global.Registry.PruneRemoved
	grace := repo.Global.Registry.PruneRemovedAfter.Std()

	var keys []schedule.Key
	for k := range all {
		if !managed[k] {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	var orphans []Orphan
	for _, k := range keys {
		since, err := d.store.MarkRemoved(k, now)
		if err != nil {
			d.log.Error("cannot record a removed container", "key", k, "err", err)
			continue
		}
		o := Orphan{Key: k, Since: since, Tags: obs[k]}
		if prune {
			o.RemoveAt = since.Add(grace)
			if !now.Before(o.RemoveAt) {
				if err := d.removeOrphan(ctx, o); err != nil {
					d.log.Error("cannot remove the images of a removed container", "key", k, "err", err)
				} else {
					continue
				}
			}
		}
		orphans = append(orphans, o)
	}
	for _, o := range orphans {
		if !o.Since.Equal(now) {
			continue
		}
		d.log.Info("no longer in the config; its images are kept for now", "key", o.Key.String(), "tags", len(o.Tags), "remove_at", o.RemoveAt)
	}
	d.setOrphans(orphans)
}

func (d *Daemon) setOrphans(o []Orphan) {
	d.mu.Lock()
	d.orphans = o
	d.mu.Unlock()
}

// removeOrphan untags everything the registry holds for the flavor and forgets its state. The
// manifests and blobs go with the next garbage collection.
func (d *Daemon) removeOrphan(ctx context.Context, o Orphan) error {
	var errs []error
	for _, tag := range o.Tags {
		if err := d.registry.Untag(ctx, o.Key.Container, tag); err != nil {
			errs = append(errs, fmt.Errorf("untag %s:%s: %w", o.Key.Container, tag, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err // keep the record: it is tried again on the next pass
	}
	if err := d.store.Forget(o.Key); err != nil {
		return err
	}
	d.mu.Lock()
	delete(d.logs, o.Key)
	delete(d.lastRun, o.Key)
	delete(d.needs, o.Key)
	delete(d.failing, o.Key)
	d.gcPending = true
	d.mu.Unlock()
	d.log.Info("removed the images of a container that is no longer in the config", "key", o.Key.String(), "tags", len(o.Tags))
	return nil
}

// Purge asks for the images of a container (or one of its flavors) that is no longer in the
// config to be removed now instead of after the grace period. It refuses anything that is still
// in the config. The removal happens in the next scheduler pass, which is woken. The returned
// keys are what will be removed.
func (d *Daemon) Purge(ctx context.Context, container, flavor string, dryRun bool) ([]schedule.Key, error) {
	d.mu.Lock()
	repo := d.repo
	d.mu.Unlock()
	if repo == nil {
		return nil, ErrNotReady
	}
	managed := d.managedKeys()
	obs, err := d.observed(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot list the registry: %w", err)
	}
	all := map[schedule.Key]bool{}
	for k := range obs {
		all[k] = true
	}
	if known, err := d.store.KnownKeys(); err == nil {
		for _, k := range known {
			all[k] = true
		}
	}
	var plan []schedule.Key
	for k := range all {
		if k.Container != container || (flavor != "" && k.Flavor != flavor) {
			continue
		}
		if managed[k] {
			return nil, fmt.Errorf("%w: %s is still in the config; remove it there first", ErrConflict, k)
		}
		plan = append(plan, k)
	}
	if len(plan) == 0 {
		return nil, fmt.Errorf("%w: nothing is stored for %q", ErrNotFound, container)
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].String() < plan[j].String() })
	if dryRun {
		return plan, nil
	}
	d.mu.Lock()
	d.purgeReqs = append(d.purgeReqs, plan...)
	d.mu.Unlock()
	d.Wake()
	return plan, nil
}

// runPurges executes the queued purge requests. It runs in the scheduler pass, so it never
// overlaps a build or a garbage collection.
func (d *Daemon) runPurges(ctx context.Context, repo *config.Repo) {
	d.mu.Lock()
	reqs := d.purgeReqs
	d.purgeReqs = nil
	d.mu.Unlock()
	if len(reqs) == 0 {
		return
	}
	obs, err := d.observed(ctx)
	if err != nil {
		d.log.Error("purge: cannot list the registry, trying again later", "err", err)
		d.mu.Lock()
		d.purgeReqs = append(reqs, d.purgeReqs...)
		d.mu.Unlock()
		return
	}
	managed := d.managedKeys()
	for _, k := range reqs {
		if managed[k] { // it came back into the config in the meantime
			d.log.Warn("purge skipped: the flavor is back in the config", "key", k.String())
			continue
		}
		o := Orphan{Key: k, Tags: obs[k]}
		if err := d.removeOrphan(ctx, o); err != nil {
			d.log.Error("purge failed", "key", k.String(), "err", err)
		}
	}
	d.mu.Lock()
	d.gcForce = true // a manual removal is about disk space now
	d.mu.Unlock()
	d.reconcileRemoved(ctx, repo, true)
}
