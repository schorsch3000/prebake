package daemon

import (
	"context"

	"github.com/schorsch3000/prebake/internal/registry"
)

// LocalRegistry adapts the embedded registry server to the Registry interface.
type LocalRegistry struct {
	Server *registry.Server
	Client *registry.Client // talks to the write listener
}

func (r *LocalRegistry) Prune(ctx context.Context, repo string, keep int) ([]string, error) {
	return registry.PruneRepo(ctx, r.Client, repo, keep)
}

func (r *LocalRegistry) GC(ctx context.Context) error { return r.Server.GC(ctx) }

func (r *LocalRegistry) Tags(ctx context.Context, repo string) ([]string, error) {
	return r.Client.Tags(ctx, repo)
}

func (r *LocalRegistry) Repositories(ctx context.Context) ([]string, error) {
	return r.Client.Repositories(ctx)
}

func (r *LocalRegistry) Untag(ctx context.Context, repo, tag string) error {
	return r.Client.Untag(ctx, repo, tag)
}
