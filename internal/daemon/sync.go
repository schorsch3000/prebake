package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/schorsch3000/prebake/internal/config"
	"github.com/schorsch3000/prebake/internal/gitsync"
	"github.com/schorsch3000/prebake/internal/notify"
	"github.com/schorsch3000/prebake/internal/schedule"
	"github.com/schorsch3000/prebake/internal/validate"
)

// RepoState is the health of the config repo connection.
type RepoState string

const (
	RepoStarting       RepoState = "starting"
	RepoOK             RepoState = "ok"
	RepoWaitingForKey  RepoState = "waiting-for-deploy-key"
	RepoDegraded       RepoState = "degraded" // newest commit invalid; running on the last good one
	RepoFetchError     RepoState = "fetch-error"
	RepoHostKeyChanged RepoState = "host-key-error"
)

// RepoStatus is what the status page shows about the config repo.
type RepoStatus struct {
	State       RepoState
	Head        string // newest commit seen
	Active      string // commit the daemon is running on
	Error       string
	LastFetch   time.Time
	LastChanged time.Time
}

const (
	backoffMin = 10 * time.Second
	backoffMax = 5 * time.Minute
)

// loadCurrent activates the last promoted checkout, if any.
func (d *Daemon) loadCurrent() {
	path, sha, ok := d.src.Current()
	if !ok {
		return
	}
	repo, err := config.LoadRepo(path)
	if err != nil {
		d.log.Error("cannot load the current checkout", "path", path, "err", err)
		return
	}
	d.activate(repo, sha, false)
	d.restoreQueueState()
	d.log.Info("loaded config repo from disk", "commit", short(sha))
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// activate makes repo the live configuration. unpark re-queues parked jobs
// (a valid push is the signal that something may have been fixed).
func (d *Daemon) activate(repo *config.Repo, sha string, unpark bool) {
	d.mu.Lock()
	d.repo, d.repoSHA = repo, sha
	d.repoStatus.Active = sha
	d.mu.Unlock()
	d.queue.SetParents(repo.Parents())
	if unpark {
		d.queue.UnparkAll()
		d.clearQueueState()
	}
}

func (d *Daemon) syncLoop(ctx context.Context) {
	for {
		err := d.SyncOnce(ctx)
		wait := d.opts.PollInterval
		var ae *gitsync.AuthError
		if errors.As(err, &ae) || errors.Is(err, errRetrySoon) {
			d.backoff = nextBackoff(d.backoff)
			wait = d.backoff
		} else {
			d.backoff = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// nextBackoff doubles the previous backoff within [backoffMin, backoffMax].
func nextBackoff(prev time.Duration) time.Duration {
	if prev < backoffMin {
		return backoffMin
	}
	if n := prev * 2; n < backoffMax {
		return n
	}
	return backoffMax
}

var errRetrySoon = errors.New("retry soon")

func (d *Daemon) setRepoStatus(st RepoState, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.repoStatus.State = st
	d.repoStatus.Error = ""
	if err != nil {
		d.repoStatus.Error = err.Error()
	}
}

// SyncOnce fetches the repo and, when the branch moved, validates and activates
// the new commit. An invalid commit never replaces the last good one.
func (d *Daemon) SyncOnce(ctx context.Context) error {
	head, err := d.src.Fetch(ctx)
	d.mu.Lock()
	d.repoStatus.LastFetch = d.now()
	d.mu.Unlock()
	if err != nil {
		var ae *gitsync.AuthError
		var he *gitsync.HostKeyError
		switch {
		case errors.As(err, &ae):
			d.setRepoStatus(RepoWaitingForKey, err)
			d.log.Warn("waiting for the deploy key to be accepted", "err", err)
		case errors.As(err, &he):
			d.setRepoStatus(RepoHostKeyChanged, err)
			d.log.Error("host key problem", "err", err)
		default:
			d.setRepoStatus(RepoFetchError, err)
			d.log.Error("fetch failed", "err", err)
		}
		return err
	}
	d.mu.Lock()
	d.repoStatus.Head = head
	active := d.repoSHA
	d.mu.Unlock()
	if head == active {
		d.mu.Lock()
		if d.repoStatus.State != RepoDegraded { // stay degraded until a valid commit arrives
			d.repoStatus.State, d.repoStatus.Error = RepoOK, ""
		}
		d.mu.Unlock()
		return nil
	}
	if head == d.badSHA {
		return nil // already reported; wait for the next push
	}

	path, err := d.src.Export(ctx, head)
	if err != nil {
		d.setRepoStatus(RepoFetchError, err)
		return fmt.Errorf("%w: %v", errRetrySoon, err)
	}
	if res := validate.Dir(path); res.Errors() > 0 {
		var b strings.Builder
		for _, is := range res.Issues {
			if is.Severity == validate.Error {
				b.WriteString(is.String() + "\n")
			}
		}
		msg := strings.TrimSpace(b.String())
		d.badSHA = head
		d.setRepoStatus(RepoDegraded, errors.New(msg))
		d.log.Error("new commit is invalid, keeping the last good one", "commit", short(head), "issues", res.Errors())
		d.notify(ctx, notify.Event{Type: notify.RepoInvalid, Title: "config repo invalid",
			Message: fmt.Sprintf("commit %s fails validation; still running on %s\n%s", short(head), short(active), msg)})
		return nil
	}
	repo, err := config.LoadRepo(path)
	if err != nil {
		d.badSHA = head
		d.setRepoStatus(RepoDegraded, err)
		d.notify(ctx, notify.Event{Type: notify.RepoInvalid, Title: "config repo unreadable",
			Message: fmt.Sprintf("commit %s: %v", short(head), err)})
		return nil
	}
	if err := d.src.Promote(head); err != nil {
		d.setRepoStatus(RepoFetchError, err)
		return err
	}
	wasDegraded := d.badSHA != ""
	d.badSHA = ""
	d.activate(repo, head, true)
	d.mu.Lock()
	d.repoStatus.State, d.repoStatus.Error, d.repoStatus.LastChanged = RepoOK, "", d.now()
	d.mu.Unlock()
	d.log.Info("activated config repo commit", "commit", short(head), "containers", len(repo.Containers))
	if wasDegraded {
		d.notify(ctx, notify.Event{Type: notify.Recovered, Title: "config repo valid again",
			Message: "commit " + short(head) + " is active"})
	}
	if err := d.src.Cleanup(d.opts.KeepCheckouts); err != nil {
		d.log.Warn("checkout cleanup failed", "err", err)
	}
	d.Wake()
	return nil
}

// restoreQueueState re-parks jobs that were parked before a restart.
func (d *Daemon) restoreQueueState() {
	if d.store == nil {
		return
	}
	states, err := d.store.QueueStates()
	if err != nil {
		d.log.Error("cannot read queue state", "err", err)
		return
	}
	for _, s := range states {
		if s.Status == schedule.Parked.String() {
			d.queue.RestoreParked(s.Key, s.Kind, s.Failures, s.Error)
			d.mu.Lock()
			d.failing[s.Key] = true // the failure was reported before the restart
			d.mu.Unlock()
		}
	}
}

func (d *Daemon) clearQueueState() {
	if d.store == nil {
		return
	}
	states, _ := d.store.QueueStates()
	for _, s := range states {
		d.store.DeleteQueueState(s.Key)
	}
}
