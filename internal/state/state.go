// Package state persists build history and scheduling state in SQLite
// (docs/design.md §14), using a pure-Go driver so the binary stays static.
package state

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/schorsch3000/prebake/internal/schedule"
)

const (
	// MaxLogBytes bounds the stored build log per job (the tail is kept).
	MaxLogBytes = 64 << 10
	// KeepJobs is how many job records are kept per (container, flavor).
	KeepJobs = 50
)

type Store struct{ db *sql.DB }

var migrations = []string{
	`CREATE TABLE images (
		container TEXT NOT NULL, flavor TEXT NOT NULL,
		last_rebuild INTEGER NOT NULL DEFAULT 0, -- unix nanoseconds, 0 = never
		last_refresh INTEGER NOT NULL DEFAULT 0,
		date_tag TEXT NOT NULL DEFAULT '', digest TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (container, flavor)
	);
	CREATE TABLE jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		container TEXT NOT NULL, flavor TEXT NOT NULL, kind TEXT NOT NULL,
		started INTEGER NOT NULL, finished INTEGER NOT NULL,
		ok INTEGER NOT NULL, error TEXT NOT NULL DEFAULT '',
		date_tag TEXT NOT NULL DEFAULT '', digest TEXT NOT NULL DEFAULT '',
		note TEXT NOT NULL DEFAULT '', log TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX jobs_key ON jobs (container, flavor, id);
	CREATE TABLE queue_state (
		container TEXT NOT NULL, flavor TEXT NOT NULL,
		status TEXT NOT NULL, failures INTEGER NOT NULL, retry_at INTEGER NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (container, flavor)
	);`,
	// 2: fingerprint of the build inputs (config.InputsHash) of the last successful build
	`ALTER TABLE images ADD COLUMN inputs_hash TEXT NOT NULL DEFAULT '';`,
	// 3: containers and flavors that are no longer in the config, and since when
	`CREATE TABLE IF NOT EXISTS removed (
		container TEXT NOT NULL, flavor TEXT NOT NULL, since INTEGER NOT NULL,
		PRIMARY KEY (container, flavor)
	);`,
	// 4: fingerprint of the refresh_cmd that the last successful rebuild verified (config.RefreshHash)
	`ALTER TABLE images ADD COLUMN refresh_checked TEXT NOT NULL DEFAULT '';`,
}

// Open opens (and migrates) the database at path; ":memory:" works for tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		dsn = "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; also keeps :memory: on a single connection
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func nano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// JobRecord is one finished job.
type JobRecord struct {
	ID                int64
	Container, Flavor string
	Kind              schedule.Kind
	Started, Finished time.Time
	OK                bool
	Error             string
	DateTag, Digest   string
	Note              string
	Log               string
	// InputsHash is the config.InputsHash the image was built from. Set it only for rebuilds:
	// a refresh stacks on the previous image and says nothing about the current inputs. Empty
	// leaves the stored value alone.
	InputsHash string
	// RefreshChecked is the config.RefreshHash of the refresh_cmd the rebuild verified. Set it for
	// rebuilds only; empty leaves the stored value alone.
	RefreshChecked string
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...[truncated]\n" + s[len(s)-n:]
}

func parseKind(s string) schedule.Kind {
	if s == "rebuild" {
		return schedule.Rebuild
	}
	if s == "refresh" {
		return schedule.Refresh
	}
	return schedule.None
}

// RecordJob stores a finished job. A successful job also advances the
// scheduling state (last rebuild/refresh, current date tag and digest). Old
// records beyond KeepJobs are pruned.
func (s *Store) RecordJob(r JobRecord) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	ok := 0
	if r.OK {
		ok = 1
	}
	res, err := tx.Exec(`INSERT INTO jobs (container, flavor, kind, started, finished, ok, error, date_tag, digest, note, log)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`, r.Container, r.Flavor, r.Kind.String(), nano(r.Started), nano(r.Finished),
		ok, r.Error, r.DateTag, r.Digest, r.Note, tail(r.Log, MaxLogBytes))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if r.OK {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO images (container, flavor) VALUES (?,?)`, r.Container, r.Flavor); err != nil {
			return 0, err
		}
		col := "last_refresh"
		if r.Kind == schedule.Rebuild {
			col = "last_rebuild"
		}
		if _, err := tx.Exec(`UPDATE images SET `+col+` = ?, date_tag = ?, digest = ? WHERE container = ? AND flavor = ?`,
			nano(r.Finished), r.DateTag, r.Digest, r.Container, r.Flavor); err != nil {
			return 0, err
		}
		if r.RefreshChecked != "" {
			if _, err := tx.Exec(`UPDATE images SET refresh_checked = ? WHERE container = ? AND flavor = ?`,
				r.RefreshChecked, r.Container, r.Flavor); err != nil {
				return 0, err
			}
		}
		if r.InputsHash != "" {
			if _, err := tx.Exec(`UPDATE images SET inputs_hash = ? WHERE container = ? AND flavor = ?`,
				r.InputsHash, r.Container, r.Flavor); err != nil {
				return 0, err
			}
		}
	}
	if _, err := tx.Exec(`DELETE FROM jobs WHERE container = ? AND flavor = ? AND id NOT IN
		(SELECT id FROM jobs WHERE container = ? AND flavor = ? ORDER BY id DESC LIMIT ?)`,
		r.Container, r.Flavor, r.Container, r.Flavor, KeepJobs); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// Image is the current state of a (container, flavor).
type Image struct {
	Container, Flavor string
	schedule.State
	DateTag, Digest string
	InputsHash      string // empty: unknown (built before hashes were recorded)
	RefreshChecked  string // empty: unknown
}

// ImageState returns the scheduling state; a never-built image has zero times.
func (s *Store) ImageState(container, flavor string) (Image, error) {
	img := Image{Container: container, Flavor: flavor}
	var rb, rf int64
	err := s.db.QueryRow(`SELECT last_rebuild, last_refresh, date_tag, digest, inputs_hash, refresh_checked FROM images WHERE container = ? AND flavor = ?`,
		container, flavor).Scan(&rb, &rf, &img.DateTag, &img.Digest, &img.InputsHash, &img.RefreshChecked)
	if err == sql.ErrNoRows {
		return img, nil
	}
	if err != nil {
		return img, err
	}
	img.LastRebuild, img.LastRefresh = fromNano(rb), fromNano(rf)
	return img, nil
}

// SetInputsHash records the inputs of an image whose hash was unknown, without touching
// anything else. It does nothing when a hash is already stored.
func (s *Store) SetInputsHash(container, flavor, hash string) error {
	_, err := s.db.Exec(`UPDATE images SET inputs_hash = ? WHERE container = ? AND flavor = ? AND inputs_hash = ''`,
		hash, container, flavor)
	return err
}

// SetRefreshChecked records which refresh_cmd an image is considered verified for. Without force it
// only fills an unknown value (adoption of images built before this was recorded); with force it
// overwrites (the command was removed, so there is nothing left to verify).
func (s *Store) SetRefreshChecked(container, flavor, hash string, force bool) error {
	q := `UPDATE images SET refresh_checked = ? WHERE container = ? AND flavor = ?`
	if !force {
		q += ` AND refresh_checked = ''`
	}
	_, err := s.db.Exec(q, hash, container, flavor)
	return err
}

// History returns the newest jobs first.
func (s *Store) History(container, flavor string, limit int) ([]JobRecord, error) {
	rows, err := s.db.Query(`SELECT id, kind, started, finished, ok, error, date_tag, digest, note, log
		FROM jobs WHERE container = ? AND flavor = ? ORDER BY id DESC LIMIT ?`, container, flavor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRecord
	for rows.Next() {
		r := JobRecord{Container: container, Flavor: flavor}
		var kind string
		var st, fin int64
		var ok int
		if err := rows.Scan(&r.ID, &kind, &st, &fin, &ok, &r.Error, &r.DateTag, &r.Digest, &r.Note, &r.Log); err != nil {
			return nil, err
		}
		r.Kind, r.Started, r.Finished, r.OK = parseKind(kind), fromNano(st), fromNano(fin), ok == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueueState is the persisted retry/park state of one key, so parked jobs
// survive a restart.
type QueueState struct {
	Key      schedule.Key
	Status   string
	Failures int
	RetryAt  time.Time
	Error    string
	Kind     schedule.Kind
}

func (s *Store) PutQueueState(q QueueState) error {
	_, err := s.db.Exec(`INSERT INTO queue_state (container, flavor, status, failures, retry_at, error, kind)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (container, flavor) DO UPDATE SET status=excluded.status, failures=excluded.failures,
		retry_at=excluded.retry_at, error=excluded.error, kind=excluded.kind`,
		q.Key.Container, q.Key.Flavor, q.Status, q.Failures, nano(q.RetryAt), q.Error, q.Kind.String())
	return err
}

func (s *Store) DeleteQueueState(k schedule.Key) error {
	_, err := s.db.Exec(`DELETE FROM queue_state WHERE container = ? AND flavor = ?`, k.Container, k.Flavor)
	return err
}

func (s *Store) QueueStates() ([]QueueState, error) {
	rows, err := s.db.Query(`SELECT container, flavor, status, failures, retry_at, error, kind FROM queue_state ORDER BY container, flavor`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueueState
	for rows.Next() {
		var q QueueState
		var ra int64
		var kind string
		if err := rows.Scan(&q.Key.Container, &q.Key.Flavor, &q.Status, &q.Failures, &ra, &q.Error, &kind); err != nil {
			return nil, err
		}
		q.RetryAt, q.Kind = fromNano(ra), parseKind(kind)
		out = append(out, q)
	}
	return out, rows.Err()
}

// MarkRemoved records that a container flavor is no longer in the config, since now, unless it
// was recorded before; it returns the time of the first record.
func (s *Store) MarkRemoved(k schedule.Key, now time.Time) (time.Time, error) {
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO removed (container, flavor, since) VALUES (?,?,?)`,
		k.Container, k.Flavor, nano(now)); err != nil {
		return time.Time{}, err
	}
	var since int64
	if err := s.db.QueryRow(`SELECT since FROM removed WHERE container = ? AND flavor = ?`, k.Container, k.Flavor).Scan(&since); err != nil {
		return time.Time{}, err
	}
	return fromNano(since), nil
}

// ClearRemoved forgets that a flavor was missing (it is back in the config).
func (s *Store) ClearRemoved(k schedule.Key) error {
	_, err := s.db.Exec(`DELETE FROM removed WHERE container = ? AND flavor = ?`, k.Container, k.Flavor)
	return err
}

// Removed returns all flavors recorded as no longer in the config.
func (s *Store) Removed() (map[schedule.Key]time.Time, error) {
	rows, err := s.db.Query(`SELECT container, flavor, since FROM removed`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[schedule.Key]time.Time{}
	for rows.Next() {
		var k schedule.Key
		var since int64
		if err := rows.Scan(&k.Container, &k.Flavor, &since); err != nil {
			return nil, err
		}
		out[k] = fromNano(since)
	}
	return out, rows.Err()
}

// KnownKeys returns every (container, flavor) the state remembers an image or a job for.
func (s *Store) KnownKeys() ([]schedule.Key, error) {
	rows, err := s.db.Query(`SELECT container, flavor FROM images UNION SELECT container, flavor FROM jobs UNION SELECT container, flavor FROM queue_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []schedule.Key
	for rows.Next() {
		var k schedule.Key
		if err := rows.Scan(&k.Container, &k.Flavor); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Forget deletes everything stored for a container flavor: its image state, job history, queue
// state and the removal record.
func (s *Store) Forget(k schedule.Key) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"images", "jobs", "queue_state", "removed"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE container = ? AND flavor = ?`, k.Container, k.Flavor); err != nil {
			return err
		}
	}
	return tx.Commit()
}
