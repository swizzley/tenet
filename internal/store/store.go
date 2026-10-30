// Package store persists fetched GitHub data in SQLite (pure Go, no cgo), so a
// repeat query is answered from disk instead of the upstream API.
//
//   - commits: one row per (repo, sha), upserted in batches by the double-buffered
//     writer's flusher, the database's only writer of commits.
//   - coverage: the periods already fetched. A period that ended at least an hour
//     before its fetch is history and is served from disk for good; a period that
//     reaches into the last hour is served from disk only within the cache TTL.
//   - commit_details: for the reconstruction, a commit's files and patches by
//     commit id, and the blobs fetched when GitHub sent no patch (key
//     "blob:<id>"). Both are immutable by id.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"

	"github.com/swizzley/tenet/internal/collect"
)

const schema = `
CREATE TABLE IF NOT EXISTS commits (
  repo TEXT NOT NULL, sha TEXT NOT NULL, login TEXT NOT NULL, name TEXT NOT NULL,
  date INTEGER NOT NULL, additions INTEGER NOT NULL, deletions INTEGER NOT NULL, headline TEXT NOT NULL,
  PRIMARY KEY (repo, sha)) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS commits_repo_date ON commits (repo, date);
CREATE TABLE IF NOT EXISTS coverage (
  repo TEXT NOT NULL, since INTEGER NOT NULL, until INTEGER NOT NULL,
  fetched_at INTEGER NOT NULL, truncated INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS coverage_repo ON coverage (repo, since, until);
CREATE TABLE IF NOT EXISTS commit_details (
  repo TEXT NOT NULL, sha TEXT NOT NULL, body BLOB NOT NULL,
  PRIMARY KEY (repo, sha)) WITHOUT ROWID;
`

// SettleTime is how old a period's end must be, at fetch time, to count as history.
const SettleTime = time.Hour

type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (and creates) the database at path.
func Open(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)") // readers never wait for the writer
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate %s: %w", path, err)
	}
	return &Store{db: db, now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// UpsertCommits writes one batch in one transaction. It is the double-buffered
// writer's flush function.
func (s *Store) UpsertCommits(batch []collect.Record) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`INSERT INTO commits (repo, sha, login, name, date, additions, deletions, headline)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo, sha) DO UPDATE SET login=excluded.login, name=excluded.name, date=excluded.date,
		additions=excluded.additions, deletions=excluded.deletions, headline=excluded.headline`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, r := range batch {
		if _, err := st.Exec(r.Repo, r.SHA, r.Login, r.Name, r.Date.UTC().UnixNano(), r.Additions, r.Deletions, r.Headline); err != nil {
			return fmt.Errorf("store: upsert %s@%s: %w", r.Repo, r.SHA, err)
		}
	}
	return tx.Commit()
}

// Commits returns a repo's stored commits from since (inclusive) to until (exclusive).
func (s *Store) Commits(ctx context.Context, repo string, since, until time.Time) ([]collect.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sha, login, name, date, additions, deletions, headline FROM commits
		WHERE repo = ? AND date >= ? AND date < ? ORDER BY date DESC, sha`, repo, since.UTC().UnixNano(), until.UTC().UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []collect.Record
	for rows.Next() {
		r := collect.Record{Repo: repo}
		var d int64
		if err := rows.Scan(&r.SHA, &r.Login, &r.Name, &d, &r.Additions, &r.Deletions, &r.Headline); err != nil {
			return nil, err
		}
		r.Date = time.Unix(0, d).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddCoverage records that the period was fetched completely (or up to the cap).
func (s *Store) AddCoverage(ctx context.Context, repo string, since, until time.Time, truncated bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO coverage (repo, since, until, fetched_at, truncated) VALUES (?, ?, ?, ?, ?)`,
		repo, since.UTC().UnixNano(), until.UTC().UnixNano(), s.now().UTC().UnixNano(), truncated)
	return err
}

// Covered reports whether a stored fetch holds the whole period and is still good:
// settled history, or a fetch younger than ttl. truncated is that fetch's flag.
func (s *Store) Covered(ctx context.Context, repo string, since, until time.Time, ttl time.Duration) (ok, truncated bool, err error) {
	now := s.now().UTC()
	var tr bool
	err = s.db.QueryRowContext(ctx, `SELECT truncated FROM coverage
		WHERE repo = ? AND since <= ? AND until >= ? AND (? <= fetched_at - ? OR fetched_at >= ?)
		ORDER BY truncated, fetched_at DESC LIMIT 1`,
		repo, since.UTC().UnixNano(), until.UTC().UnixNano(),
		until.UTC().UnixNano(), int64(SettleTime), now.Add(-ttl).UnixNano()).Scan(&tr)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return err == nil, tr, err
}

// Detail returns a cached commit detail body.
func (s *Store) Detail(ctx context.Context, repo, sha string) ([]byte, bool, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT body FROM commit_details WHERE repo = ? AND sha = ?`, repo, sha).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return b, err == nil, err
}

// PutDetail caches a commit detail body. A sha's content never changes.
func (s *Store) PutDetail(ctx context.Context, repo, sha string, body []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO commit_details (repo, sha, body) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, repo, sha, body)
	return err
}
