package store

import (
	"context"
	"github.com/google/uuid"
)

// InstanceID identifies the registry independently of its filesystem name.
// It is created only when statistics are enabled, survives restarts and copies,
// and changes when a new registry is created at the same path.
func (s *Store) InstanceID(ctx context.Context) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS registry_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO registry_metadata(key,value) VALUES('instance_id',?)`, uuid.NewString()); err != nil {
		return "", err
	}
	var id string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM registry_metadata WHERE key='instance_id'`).Scan(&id); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}
