package storage

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func OpenDB(dbPath string) (*sql.DB, error) {
	if dbPath == "" {
		return nil, errors.New("HAZUKI_DB_PATH is empty")
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}

	// Pass every PRAGMA through the DSN so that each pooled connection is
	// initialised identically.  Previously the PRAGMAs were executed once
	// after Open(); new connections created by the pool never received them.
	//
	// WAL mode is the key fix: without it the default DELETE journal mode
	// lets concurrent write transactions (traffic persister flush every 5 s,
	// config save, db-maintenance prune) collide on the same pooled
	// connection, producing "cannot start a transaction within a transaction".
	//
	// synchronous=NORMAL is safe with WAL and avoids an fsync per commit.
	v := make(url.Values)
	v.Add("_pragma", "journal_mode(WAL)")
	v.Add("_pragma", "synchronous(NORMAL)")
	v.Add("_pragma", "foreign_keys(1)")
	v.Add("_pragma", "busy_timeout(5000)")
	dsn := "file:" + dbPath + "?" + v.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	// Keep the pool small: WAL allows concurrent readers alongside one
	// writer, but opening too many file handles to the same database is
	// pointless for this workload.
	db.SetMaxOpenConns(4)

	return db, nil
}
