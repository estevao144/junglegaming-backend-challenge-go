package migrations

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.sql
var files embed.FS

// Apply executes pending migrations on up and reverses all versions on down,
// preserving the original CLI contract. Each invocation is atomic. The advisory lock
// protects schema changes only; it is never used for wallet processing.
func Apply(ctx context.Context, pool *pgxpool.Pool, direction string) error {
	if direction != "up" && direction != "down" {
		return fmt.Errorf("migration direction must be up or down")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(731031)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	versions := []string{"0001_financial", "0002_outbox_delivery"}
	for step := range versions {
		index := step
		if direction == "down" {
			index = len(versions) - 1 - step
		}
		if err := applyVersion(ctx, tx, index+1, versions[index], direction); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func applyVersion(ctx context.Context, tx pgx.Tx, version int, name, direction string) error {
	up, err := files.ReadFile(name + ".up.sql")
	if err != nil {
		return err
	}
	hash := sha256.Sum256(up)
	checksum := hex.EncodeToString(hash[:])
	var stored string
	err = tx.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version = $1`, version).Scan(&stored)
	applied := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if applied && stored != checksum {
		return fmt.Errorf("migration %d checksum changed", version)
	}
	if (direction == "up" && applied) || (direction == "down" && !applied) {
		return nil
	}
	sql, err := files.ReadFile(name + "." + direction + ".sql")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		return fmt.Errorf("migration %d %s: %w", version, direction, err)
	}
	if direction == "up" {
		_, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version, checksum) VALUES ($1, $2)`, version, checksum)
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version)
	}
	if err != nil {
		return err
	}
	return nil
}
