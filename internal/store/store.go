// Package store keeps tasks in SQLite (modernc.org/sqlite, no cgo).
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	_ "modernc.org/sqlite"

	"fleetlab/internal/task"
)

// DB implements task.Store. Each task is one row; the whole record is a JSON
// column so adding fields needs no migration.
type DB struct {
	db  *sql.DB
	log *slog.Logger
}

// Open opens (or creates) the database. log may be nil.
func Open(path string, log *slog.Logger) (*DB, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	// WAL survives a hard kill mid-write; busy_timeout covers a second reader.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; keeps pragmas on the single connection
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		id    TEXT PRIMARY KEY,
		seq   INTEGER NOT NULL,
		state TEXT NOT NULL,
		data  TEXT NOT NULL
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &DB{db: db, log: log}, nil
}

func (s *DB) Close() error { return s.db.Close() }

// Save inserts or replaces the task.
func (s *DB) Save(t task.Task) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO tasks (id, seq, state, data) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET state = excluded.state, data = excluded.data`,
		t.ID, t.Seq, t.State, string(data))
	return err
}

// Load returns every task in creation order. A row that cannot be decoded is
// logged and skipped: one bad row must not keep the server from starting.
func (s *DB) Load() ([]task.Task, error) {
	rows, err := s.db.Query(`SELECT id, data FROM tasks ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []task.Task
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		var t task.Task
		if err := json.Unmarshal([]byte(data), &t); err != nil || t.ID != id || len(t.Waypoints) == 0 {
			s.log.Error("skipping unreadable task row", "id", id, "err", err)
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// MaxSeq is the highest seq ever stored, including rows Load skipped, so new
// task ids never reuse one of them.
func (s *DB) MaxSeq() (uint64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(seq) FROM tasks`).Scan(&n)
	return uint64(n.Int64), err
}
