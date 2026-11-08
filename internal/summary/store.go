// Package summary counts the pull requests opened, merged and closed over a window, from the
// open listings and from the pull requests it keeps in SQLite, which it brings up to date by
// reading what changed since its last read, and back to a window's start where the store
// does not reach it.
package summary

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// schema is the store's tables. A record is kept whatever its state, so a reopened pull
// request's newer open record stands over its closed one, and closed_at is null while open.
const schema = `
CREATE TABLE pull_requests (
  slug_key     TEXT    NOT NULL,
  number       INTEGER NOT NULL,
  slug         TEXT    NOT NULL,
  title        TEXT    NOT NULL,
  url          TEXT    NOT NULL,
  author       TEXT    NOT NULL,
  bot          INTEGER,
  state        TEXT    NOT NULL,
  created_at   TEXT    NOT NULL,
  updated_at   TEXT    NOT NULL,
  merged_at    TEXT,
  closed_at    TEXT,
  review_round INTEGER NOT NULL,
  PRIMARY KEY (slug_key, number)
);
CREATE INDEX pull_requests_closed_at ON pull_requests (closed_at);
CREATE TABLE coverage (
  only         INTEGER PRIMARY KEY CHECK (only = 1),
  covered_from TEXT NOT NULL,
  synced_at    TEXT NOT NULL
);`

// schemaVersion is a digest of schema, kept in the file's user_version, so any change to the
// tables changes it. The store is a cache, so a file holding another shape is emptied and
// read again rather than migrated.
var schemaVersion = func() int {
	digest := sha256.Sum256([]byte(schema))
	return int(binary.BigEndian.Uint32(digest[:4]) >> 1)
}()

// connectionOptions ride in the DSN, because a pragma set through Exec reaches only the one
// pooled connection that ran it. A board and a prs summary can write one file at once, so a
// writer waits out the other, and a transaction takes the write lock when it begins.
const connectionOptions = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate"

// stamp is how a time is kept: RFC 3339 in UTC to the second, so text order is time order.
const stamp = time.RFC3339

// Store is the pull requests one scope's repos hold, and how far back and how lately they
// were read.
type Store struct {
	db   *sql.DB
	path string
}

// Coverage is how far the store reaches. It holds every pull request in the scope last
// updated at or after From, each as it stood at Synced or later, and so every one merged or
// closed since From.
type Coverage struct {
	From   time.Time `json:"from"`
	Synced time.Time `json:"synced"`
}

// key names a pull request across reads, in lower case, because GitHub matches an owner and
// a name in any case.
func key(slug string, number int) string { return fmt.Sprintf("%s#%d", strings.ToLower(slug), number) }

// FileFor is the store's file in dir for a scope: a name digested from its repos and owners,
// so two scopes never share rows, and an edit to the registry starts a file of its own.
func FileFor(dir string, scope pullrequests.Scope) string {
	repos, owners := scope.Names()
	raw, _ := json.Marshal(map[string][]string{"repos": repos, "owners": owners})
	digest := sha256.Sum256(raw)
	return filepath.Join(dir, hex.EncodeToString(digest[:])[:16]+".db")
}

// Open opens the store at path, making its directory and tables where they are missing. An
// empty path keeps the store in memory, for a run with nowhere to cache it.
func Open(path string) (*Store, error) {
	dsn := ":memory:"
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("making the closed pull requests' cache: %w", err)
		}
		dsn = path + connectionOptions
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening the closed pull requests' cache %s: %w", path, err)
	}
	if path == "" {
		// Each connection to :memory: is a database of its own.
		db.SetMaxOpenConns(1)
	}
	s := &Store{db: db, path: path}
	if err := s.shape(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("the closed pull requests' cache %s: %w", path, err)
	}
	return s, nil
}

// shape makes the tables where the file holds none, or another version's.
func (s *Store) shape() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == schemaVersion {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{"DROP TABLE IF EXISTS pull_requests", "DROP TABLE IF EXISTS coverage", schema, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close closes the file.
func (s *Store) Close() error { return s.db.Close() }

// Path is the store's file, and empty for one kept in memory.
func (s *Store) Path() string { return s.path }

// Coverage is how far the store reaches, and false where nothing has been read into it.
func (s *Store) Coverage() (Coverage, bool, error) {
	var from, synced string
	err := s.db.QueryRow("SELECT covered_from, synced_at FROM coverage").Scan(&from, &synced)
	if errors.Is(err, sql.ErrNoRows) {
		return Coverage{}, false, nil
	}
	if err != nil {
		return Coverage{}, false, fmt.Errorf("reading what the closed pull requests' cache covers: %w", err)
	}
	var c Coverage
	if c.From, err = time.Parse(stamp, from); err != nil {
		return Coverage{}, false, err
	}
	if c.Synced, err = time.Parse(stamp, synced); err != nil {
		return Coverage{}, false, err
	}
	return c, true, nil
}

// Closed is every pull request the store holds merged or closed at or after since, the
// newest first.
func (s *Store) Closed(since time.Time) ([]pullrequests.HistoryRecord, error) {
	rows, err := s.db.Query(`SELECT slug, number, title, url, author, bot, state, created_at, updated_at, merged_at, closed_at, review_round
		FROM pull_requests WHERE closed_at >= ? ORDER BY closed_at DESC`, since.UTC().Format(stamp))
	if err != nil {
		return nil, fmt.Errorf("reading the closed pull requests' cache: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []pullrequests.HistoryRecord
	for rows.Next() {
		var r pullrequests.HistoryRecord
		var bot sql.NullBool
		var created, updated string
		var merged, closed sql.NullString
		if err := rows.Scan(&r.Slug, &r.Number, &r.Title, &r.URL, &r.Author, &bot, &r.State, &created, &updated, &merged, &closed, &r.ReviewRound); err != nil {
			return nil, err
		}
		if bot.Valid {
			r.Bot = &bot.Bool
		}
		if r.CreatedAt, err = parseStamp(r, created); err == nil {
			r.UpdatedAt, err = parseStamp(r, updated)
		}
		if err == nil {
			r.MergedAt, err = parseNullStamp(r, merged)
		}
		if err == nil {
			r.ClosedAt, err = parseNullStamp(r, closed)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func parseStamp(r pullrequests.HistoryRecord, text string) (time.Time, error) {
	at, err := time.Parse(stamp, text)
	if err != nil {
		return at, fmt.Errorf("the closed pull requests' cache holds %s#%d with a time %q: %w", r.Slug, r.Number, text, err)
	}
	return at, nil
}

func parseNullStamp(r pullrequests.HistoryRecord, text sql.NullString) (*time.Time, error) {
	if !text.Valid {
		return nil, nil
	}
	at, err := parseStamp(r, text.String)
	return &at, err
}

// Keep writes each record over any row it had that was updated no later, and the coverage,
// as one transaction, so the coverage never claims a read whose rows did not land. A board
// and a prs summary can sync one file at once, so the coverage only widens: the earliest
// From and the latest Synced stand. Each read asks for every update since a From older than
// any sync's start, so the later sync read every change since the earlier one began.
func (s *Store) Keep(records []pullrequests.HistoryRecord, coverage Coverage) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("writing the closed pull requests' cache: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	upsert, err := tx.Prepare(`INSERT INTO pull_requests
		(slug_key, number, slug, title, url, author, bot, state, created_at, updated_at, merged_at, closed_at, review_round)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (slug_key, number) DO UPDATE SET
		  slug = excluded.slug, title = excluded.title, url = excluded.url, author = excluded.author,
		  bot = excluded.bot, state = excluded.state, created_at = excluded.created_at,
		  updated_at = excluded.updated_at, merged_at = excluded.merged_at, closed_at = excluded.closed_at,
		  review_round = excluded.review_round
		WHERE excluded.updated_at >= pull_requests.updated_at`)
	if err != nil {
		return fmt.Errorf("writing the closed pull requests' cache: %w", err)
	}
	defer func() { _ = upsert.Close() }()
	for _, r := range records {
		var bot any
		if r.Bot != nil {
			bot = *r.Bot
		}
		if _, err := upsert.Exec(strings.ToLower(r.Slug), r.Number, r.Slug, r.Title, r.URL, r.Author, bot, r.State,
			r.CreatedAt.UTC().Format(stamp), r.UpdatedAt.UTC().Format(stamp), nullStamp(r.MergedAt), nullStamp(r.ClosedAt), r.ReviewRound); err != nil {
			return fmt.Errorf("writing %s#%d to the closed pull requests' cache: %w", r.Slug, r.Number, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO coverage (only, covered_from, synced_at) VALUES (1, ?, ?)
		ON CONFLICT (only) DO UPDATE SET
		  covered_from = MIN(covered_from, excluded.covered_from), synced_at = MAX(synced_at, excluded.synced_at)`,
		coverage.From.UTC().Format(stamp), coverage.Synced.UTC().Format(stamp)); err != nil {
		return fmt.Errorf("writing what the closed pull requests' cache covers: %w", err)
	}
	return tx.Commit()
}

func nullStamp(at *time.Time) any {
	if at == nil {
		return nil
	}
	return at.UTC().Format(stamp)
}
