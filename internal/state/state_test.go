package state

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/schorsch3000/prebake/internal/schedule"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var t0 = time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)

func rec(kind schedule.Kind, ok bool, at time.Time) JobRecord {
	return JobRecord{Container: "dev", Flavor: "default", Kind: kind, Started: at, Finished: at.Add(time.Minute),
		OK: ok, DateTag: "default-x", Digest: "sha256:abc"}
}

func TestNeverBuiltIsZero(t *testing.T) {
	img, err := open(t).ImageState("dev", "default")
	if err != nil || !img.LastRebuild.IsZero() || !img.LastRefresh.IsZero() {
		t.Fatalf("%+v %v", img, err)
	}
}

func TestSuccessAdvancesState(t *testing.T) {
	s := open(t)
	if _, err := s.RecordJob(rec(schedule.Rebuild, true, t0)); err != nil {
		t.Fatal(err)
	}
	img, _ := s.ImageState("dev", "default")
	if !img.LastRebuild.Equal(t0.Add(time.Minute)) || !img.LastRefresh.IsZero() || img.Digest != "sha256:abc" || img.DateTag != "default-x" {
		t.Errorf("%+v", img)
	}
	if _, err := s.RecordJob(rec(schedule.Refresh, true, t0.Add(24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	img, _ = s.ImageState("dev", "default")
	if !img.LastRebuild.Equal(t0.Add(time.Minute)) || !img.LastRefresh.Equal(t0.Add(24*time.Hour+time.Minute)) {
		t.Errorf("refresh must only move the refresh clock: %+v", img)
	}
}

func TestFailureKeepsStateButIsRecorded(t *testing.T) {
	s := open(t)
	s.RecordJob(rec(schedule.Rebuild, true, t0))
	r := rec(schedule.Rebuild, false, t0.Add(time.Hour))
	r.Error = "apt exploded"
	s.RecordJob(r)
	img, _ := s.ImageState("dev", "default")
	if !img.LastRebuild.Equal(t0.Add(time.Minute)) {
		t.Errorf("failure must not advance state: %+v", img)
	}
	h, err := s.History("dev", "default", 10)
	if err != nil || len(h) != 2 || h[0].OK || h[0].Error != "apt exploded" || h[0].Kind != schedule.Rebuild || !h[1].OK {
		t.Fatalf("%+v %v", h, err)
	}
}

func TestPruneAndLogBound(t *testing.T) {
	s := open(t)
	big := strings.Repeat("x", MaxLogBytes*2)
	for i := 0; i < KeepJobs+10; i++ {
		r := rec(schedule.Refresh, true, t0.Add(time.Duration(i)*time.Hour))
		r.Log = big
		s.RecordJob(r)
	}
	h, _ := s.History("dev", "default", 1000)
	if len(h) != KeepJobs {
		t.Errorf("kept %d jobs, want %d", len(h), KeepJobs)
	}
	if len(h[0].Log) > MaxLogBytes+32 || !strings.HasPrefix(h[0].Log, "...[truncated]") {
		t.Errorf("log len %d", len(h[0].Log))
	}
	// another key is unaffected by pruning
	o := rec(schedule.Rebuild, true, t0)
	o.Flavor = "cuda"
	s.RecordJob(o)
	if h, _ := s.History("dev", "cuda", 10); len(h) != 1 {
		t.Errorf("cuda history = %d", len(h))
	}
}

func TestQueueStateRoundTrip(t *testing.T) {
	s := open(t)
	k := schedule.Key{Container: "dev", Flavor: "default"}
	q := QueueState{Key: k, Status: "parked", Failures: 2, RetryAt: t0, Error: "boom", Kind: schedule.Refresh}
	if err := s.PutQueueState(q); err != nil {
		t.Fatal(err)
	}
	q.Failures, q.Status = 1, "retrying"
	s.PutQueueState(q) // upsert
	got, err := s.QueueStates()
	if err != nil || len(got) != 1 || got[0].Failures != 1 || got[0].Status != "retrying" || !got[0].RetryAt.Equal(t0) || got[0].Kind != schedule.Refresh {
		t.Fatalf("%+v %v", got, err)
	}
	s.DeleteQueueState(k)
	if got, _ := s.QueueStates(); len(got) != 0 {
		t.Errorf("not deleted: %v", got)
	}
}

func TestPersistsAcrossReopenAndMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.RecordJob(rec(schedule.Rebuild, true, t0))
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if img, _ := s.ImageState("dev", "default"); img.LastRebuild.IsZero() {
		t.Error("state lost on reopen")
	}
	m, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := m.RecordJob(rec(schedule.Rebuild, true, t0)); err != nil {
		t.Fatal(err)
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, _ := Open(path)
	s.db.Exec(`PRAGMA user_version = 99`)
	s.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("err = %v", err)
	}
}

func TestInputsHashRecordedOnlyWhenGivenAndAdoptedOnce(t *testing.T) {
	s := open(t)
	r := rec(schedule.Rebuild, true, t0)
	r.InputsHash = "h1"
	s.RecordJob(r)
	if img, _ := s.ImageState("dev", "default"); img.InputsHash != "h1" {
		t.Fatalf("%+v", img)
	}
	// a refresh (no hash) must not touch it
	s.RecordJob(rec(schedule.Refresh, true, t0.Add(time.Hour)))
	if img, _ := s.ImageState("dev", "default"); img.InputsHash != "h1" {
		t.Errorf("refresh changed the hash: %+v", img)
	}
	// a failed build never changes it
	f := rec(schedule.Rebuild, false, t0.Add(2*time.Hour))
	f.InputsHash = "h2"
	s.RecordJob(f)
	if img, _ := s.ImageState("dev", "default"); img.InputsHash != "h1" {
		t.Errorf("failed build changed the hash: %+v", img)
	}
	// adoption: only fills an unknown hash
	s.SetInputsHash("dev", "default", "adopted")
	if img, _ := s.ImageState("dev", "default"); img.InputsHash != "h1" {
		t.Errorf("SetInputsHash must not overwrite: %+v", img)
	}
	old := rec(schedule.Rebuild, true, t0)
	old.Container = "legacy"
	s.RecordJob(old) // built without a hash
	s.SetInputsHash("legacy", "default", "adopted")
	if img, _ := s.ImageState("legacy", "default"); img.InputsHash != "adopted" {
		t.Errorf("unknown hash should be adopted: %+v", img)
	}
}

func TestMigrationAddsInputsHashToExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// simulate a database from before migration 2
	s.db.Exec(`ALTER TABLE images DROP COLUMN inputs_hash`)
	s.db.Exec(`DROP TABLE removed`)
	s.db.Exec(`PRAGMA user_version = 1`)
	s.db.Exec(`INSERT INTO images (container, flavor, last_rebuild) VALUES ('old', 'default', 5)`)
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	img, err := s.ImageState("old", "default")
	if err != nil || img.LastRebuild.IsZero() || img.InputsHash != "" {
		t.Fatalf("%+v %v", img, err)
	}
}

func TestRemovedRecordsAndForget(t *testing.T) {
	s := open(t)
	k := schedule.Key{Container: "old", Flavor: "default"}
	first, err := s.MarkRemoved(k, t0)
	if err != nil || !first.Equal(t0) {
		t.Fatalf("%v %v", first, err)
	}
	// marking again keeps the first time: the grace period counts from when it went missing
	again, _ := s.MarkRemoved(k, t0.Add(48*time.Hour))
	if !again.Equal(t0) {
		t.Errorf("second mark moved the time: %v", again)
	}
	all, _ := s.Removed()
	if len(all) != 1 || !all[k].Equal(t0) {
		t.Errorf("%v", all)
	}
	s.ClearRemoved(k)
	if all, _ := s.Removed(); len(all) != 0 {
		t.Errorf("cleared: %v", all)
	}

	// Forget removes image state, history, queue state and the record of one flavor only
	other := schedule.Key{Container: "keep", Flavor: "default"}
	for _, key := range []schedule.Key{k, other} {
		r := rec(schedule.Rebuild, true, t0)
		r.Container, r.Flavor = key.Container, key.Flavor
		s.RecordJob(r)
		s.PutQueueState(QueueState{Key: key, Status: "parked", Failures: 2})
		s.MarkRemoved(key, t0)
	}
	keys, _ := s.KnownKeys()
	if len(keys) != 2 {
		t.Fatalf("known keys: %v", keys)
	}
	if err := s.Forget(k); err != nil {
		t.Fatal(err)
	}
	keys, _ = s.KnownKeys()
	if len(keys) != 1 || keys[0] != other {
		t.Errorf("only the other key remains: %v", keys)
	}
	if h, _ := s.History("old", "default", 5); len(h) != 0 {
		t.Errorf("history: %v", h)
	}
	if img, _ := s.ImageState("old", "default"); !img.LastRebuild.IsZero() {
		t.Errorf("image state: %+v", img)
	}
	if all, _ := s.Removed(); len(all) != 1 {
		t.Errorf("removed: %v", all)
	}
}
