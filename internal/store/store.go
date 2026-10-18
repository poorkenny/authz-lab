// Package store is the primary permission store, backed by SQLite.
//
// Every write runs in one transaction that updates the tuple, appends to the
// changelog (whose autoincrement id is the revision) and bumps the document's
// revision token. Each store is a private in-memory database.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// Tuple is a relationship: Object#Relation@Subject, e.g. doc:42#viewer@user:alice.
type Tuple struct {
	Object   string `json:"object"`
	Relation string `json:"relation"`
	Subject  string `json:"subject"`
}

func (t Tuple) String() string { return t.Object + "#" + t.Relation + "@" + t.Subject }

func (t Tuple) Validate() error {
	if !strings.HasPrefix(t.Object, "doc:") || !strings.HasPrefix(t.Subject, "user:") {
		return fmt.Errorf("tuple %s: object must be doc:<id> and subject user:<id>", t)
	}
	if t.Relation != "owner" && t.Relation != "viewer" {
		return fmt.Errorf("tuple %s: relation must be owner or viewer", t)
	}
	if len(t.String()) > 200 {
		return errors.New("tuple too long")
	}
	return nil
}

type Op string

const (
	OpTouch  Op = "touch"
	OpDelete Op = "delete"
)

// Change is one committed write.
type Change struct {
	Rev   int64 `json:"rev"`
	Op    Op    `json:"op"`
	Tuple Tuple `json:"tuple"`
}

const schema = `
CREATE TABLE tuples (
  object   TEXT NOT NULL,
  relation TEXT NOT NULL,
  subject  TEXT NOT NULL,
  PRIMARY KEY (object, relation, subject)
);
CREATE TABLE changelog (
  rev      INTEGER PRIMARY KEY AUTOINCREMENT,
  op       TEXT NOT NULL,
  object   TEXT NOT NULL,
  relation TEXT NOT NULL,
  subject  TEXT NOT NULL
);
CREATE TABLE doc_revisions (
  object TEXT PRIMARY KEY,
  rev    INTEGER NOT NULL
);`

type Store struct {
	db  *sql.DB
	rev int64
}

// Open creates a fresh in-memory SQLite store.
func Open() (*Store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	// Each connection to ":memory:" is its own database, so keep exactly one.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Rev is the latest committed revision.
func (s *Store) Rev() int64 { return s.rev }

// Write commits one change and returns it with its revision.
func (s *Store) Write(op Op, t Tuple) (Change, error) {
	if err := t.Validate(); err != nil {
		return Change{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Change{}, err
	}
	defer tx.Rollback()

	switch op {
	case OpTouch:
		if _, err := tx.Exec(`INSERT OR IGNORE INTO tuples (object, relation, subject) VALUES (?, ?, ?)`, t.Object, t.Relation, t.Subject); err != nil {
			return Change{}, err
		}
	case OpDelete:
		res, err := tx.Exec(`DELETE FROM tuples WHERE object = ? AND relation = ? AND subject = ?`, t.Object, t.Relation, t.Subject)
		if err != nil {
			return Change{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return Change{}, fmt.Errorf("delete %s: tuple does not exist", t)
		}
	default:
		return Change{}, fmt.Errorf("unknown op %q", op)
	}

	res, err := tx.Exec(`INSERT INTO changelog (op, object, relation, subject) VALUES (?, ?, ?, ?)`, op, t.Object, t.Relation, t.Subject)
	if err != nil {
		return Change{}, err
	}
	rev, err := res.LastInsertId()
	if err != nil {
		return Change{}, err
	}
	if _, err := tx.Exec(`INSERT INTO doc_revisions (object, rev) VALUES (?, ?) ON CONFLICT(object) DO UPDATE SET rev = excluded.rev`, t.Object, rev); err != nil {
		return Change{}, err
	}
	if err := tx.Commit(); err != nil {
		return Change{}, err
	}
	s.rev = rev
	return Change{Rev: rev, Op: op, Tuple: t}, nil
}

// CanView reports whether subject is an owner or viewer of object.
func (s *Store) CanView(subject, object string) (bool, error) {
	var ok bool
	err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM tuples WHERE object = ? AND relation IN ('owner', 'viewer') AND subject = ?)`, object, subject).Scan(&ok)
	return ok, err
}

// DocRevs returns every document's revision token (the revision of its last ACL change).
func (s *Store) DocRevs() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT object, rev FROM doc_revisions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var obj string
		var rev int64
		if err := rows.Scan(&obj, &rev); err != nil {
			return nil, err
		}
		out[obj] = rev
	}
	return out, rows.Err()
}

// DocRev returns one document's revision token (0 if it has never changed).
func (s *Store) DocRev(object string) (int64, error) {
	var rev int64
	err := s.db.QueryRow(`SELECT rev FROM doc_revisions WHERE object = ?`, object).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return rev, err
}

// Tuples lists every tuple, sorted.
func (s *Store) Tuples() ([]Tuple, error) {
	rows, err := s.db.Query(`SELECT object, relation, subject FROM tuples ORDER BY object, relation, subject`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tuple
	for rows.Next() {
		var t Tuple
		if err := rows.Scan(&t.Object, &t.Relation, &t.Subject); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
