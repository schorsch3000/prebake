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
}

// ImageState returns the scheduling state; a never-built image has zero times.
func (s *Store) ImageState(container, flavor string) (Image, error) {
	img := Image{Container: container, Flavor: flavor}
	var rb, rf int64
	err := s.db.QueryRow(`SELECT last_rebuild, last_refresh, date_tag, digest, inputs_hash FROM images WHERE container = ? AND flavor = ?`,
		container, flavor).Scan(&rb, &rf, &img.DateTag, &img.Digest, &img.InputsHash)
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
