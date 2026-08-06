package githubhook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const pendingLease = 5 * time.Minute

type ReceiptStore struct {
	db *sql.DB
}

type AcquireResult int

const (
	Acquired AcquireResult = iota
	Duplicate
	InProgress
)

func OpenReceiptStore(ctx context.Context, dataDir string) (*ReceiptStore, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dataDir, "github-hook.sqlite")
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_journal_mode=WAL&_busy_timeout=5000"
	database, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open receipt database: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	store := &ReceiptStore{db: database}
	if _, err := database.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS receipts (
			delivery_id TEXT NOT NULL,
			rule_id TEXT NOT NULL,
			state TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(delivery_id, rule_id)
		);
		CREATE INDEX IF NOT EXISTS receipts_updated_at ON receipts(updated_at);
	`); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("create receipt schema: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("restrict receipt database: %w", err)
	}
	return store, nil
}

func (s *ReceiptStore) Close() error {
	return s.db.Close()
}

func (s *ReceiptStore) Acquire(ctx context.Context, deliveryID, ruleID string, now time.Time) (AcquireResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Acquired, err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `INSERT INTO receipts(delivery_id,rule_id,state,updated_at) VALUES(?,?,'pending',?) ON CONFLICT(delivery_id,rule_id) DO NOTHING`, deliveryID, ruleID, stamp)
	if err != nil {
		return Acquired, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Acquired, err
	}
	if rows == 1 {
		if err := tx.Commit(); err != nil {
			return Acquired, err
		}
		return Acquired, nil
	}
	var state, updatedRaw string
	if err := tx.QueryRowContext(ctx, `SELECT state,updated_at FROM receipts WHERE delivery_id=? AND rule_id=?`, deliveryID, ruleID).Scan(&state, &updatedRaw); err != nil {
		return Acquired, err
	}
	if state == "delivered" {
		if err := tx.Commit(); err != nil {
			return Acquired, err
		}
		return Duplicate, nil
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, updatedRaw)
	if err != nil {
		return Acquired, fmt.Errorf("parse pending receipt timestamp: %w", err)
	}
	if now.UTC().Sub(updatedAt) < pendingLease {
		if err := tx.Commit(); err != nil {
			return Acquired, err
		}
		return InProgress, nil
	}
	result, err = tx.ExecContext(ctx, `UPDATE receipts SET updated_at=? WHERE delivery_id=? AND rule_id=? AND state='pending' AND updated_at=?`, stamp, deliveryID, ruleID, updatedRaw)
	if err != nil {
		return Acquired, err
	}
	rows, err = result.RowsAffected()
	if err != nil {
		return Acquired, err
	}
	if err := tx.Commit(); err != nil {
		return Acquired, err
	}
	if rows == 1 {
		return Acquired, nil
	}
	return InProgress, nil
}

func (s *ReceiptStore) MarkDelivered(ctx context.Context, deliveryID, ruleID string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE receipts SET state='delivered',updated_at=? WHERE delivery_id=? AND rule_id=? AND state='pending'`, now.UTC().Format(time.RFC3339Nano), deliveryID, ruleID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("pending receipt ownership was lost")
	}
	return nil
}

func (s *ReceiptStore) Release(ctx context.Context, deliveryID, ruleID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM receipts WHERE delivery_id=? AND rule_id=? AND state='pending'`, deliveryID, ruleID)
	return err
}
