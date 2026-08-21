package database

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func OpenPermanent(path string) (*sql.DB, error) {
	return open(path, true)
}

func OpenRuntime(path string) (*sql.DB, error) {
	return open(path, false)
}

func open(path string, wal bool) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("secure database directory: %w", err)
	}
	q := url.Values{"_foreign_keys": {"on"}, "_busy_timeout": {"5000"}}
	if wal {
		q.Set("_journal_mode", "WAL")
	}
	databaseURL := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	db, err := sql.Open("sqlite3", databaseURL.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("secure database: %w", err)
	}
	return db, nil
}
