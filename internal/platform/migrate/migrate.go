// Package migrate applies versioned SQL migrations embedded in the service binary.
//
// Migrations ship inside the image rather than as a separate artefact, so the
// schema a container expects and the schema it applies can never drift apart. A
// Postgres advisory lock serialises concurrent starts, which matters the moment
// a Deployment scales past one replica or a rolling update overlaps pods.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// advisoryLockID is an arbitrary but stable key. Every Arcadia service uses the
// same one; they hold it against different databases so they never contend.
const advisoryLockID int64 = 8_276_301_945

// Migration is one versioned SQL file.
type Migration struct {
	// Version is the numeric prefix of the filename, e.g. 1 for 0001_init.sql.
	Version int64
	// Name is the descriptive part of the filename.
	Name string
	// SQL is the file's contents.
	SQL string
	// Checksum detects a migration that was edited after being applied.
	Checksum string
}

// Errors reported by this package.
var (
	ErrChecksumMismatch = errors.New("migrate: a previously applied migration was modified")
	ErrNoMigrations     = errors.New("migrate: no migration files found")
	ErrBadFilename      = errors.New("migrate: migration filename must look like 0001_description.sql")
)

// Load reads and validates migrations from an embedded filesystem.
//
// Files must be named <version>_<description>.sql, e.g. 0001_create_wallets.sql.
// Versions must be unique.
func Load(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: read %s: %w", dir, err)
	}

	migrations := make([]Migration, 0, len(entries))
	seen := make(map[int64]string, len(entries))

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, name, err := parseFilename(entry.Name())
		if err != nil {
			return nil, err
		}
		if existing, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrate: version %d is used by both %s and %s", version, existing, entry.Name())
		}
		seen[version] = entry.Name()

		body, err := fs.ReadFile(fsys, path.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(body)

		migrations = append(migrations, Migration{
			Version:  version,
			Name:     name,
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	if len(migrations) == 0 {
		return nil, fmt.Errorf("%w in %s", ErrNoMigrations, dir)
	}
	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})
	return migrations, nil
}

func parseFilename(filename string) (int64, string, error) {
	base := strings.TrimSuffix(filename, ".sql")
	idx := strings.Index(base, "_")
	if idx <= 0 || idx == len(base)-1 {
		return 0, "", fmt.Errorf("%w: got %q", ErrBadFilename, filename)
	}
	version, err := strconv.ParseInt(base[:idx], 10, 64)
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("%w: got %q", ErrBadFilename, filename)
	}
	return version, base[idx+1:], nil
}

// Runner applies migrations against a database.
type Runner struct {
	conn   *pgx.Conn
	logger *slog.Logger
}

// NewRunner returns a Runner bound to a single connection. A dedicated
// connection is required because a session-level advisory lock is only held for
// as long as the session that took it.
func NewRunner(conn *pgx.Conn, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{conn: conn, logger: logger}
}

// Connect opens a dedicated connection and returns a Runner plus its closer.
func Connect(ctx context.Context, dsn string, logger *slog.Logger) (*Runner, func(context.Context) error, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("migrate: connect: %w", err)
	}
	return NewRunner(conn, logger), conn.Close, nil
}

const createSchemaTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version      BIGINT      PRIMARY KEY,
    name         TEXT        NOT NULL,
    checksum     TEXT        NOT NULL,
    applied_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    duration_ms  BIGINT      NOT NULL
)`

// Up applies every pending migration in version order.
//
// Each migration runs in its own transaction together with the bookkeeping row
// that records it, so a failure halfway through a batch leaves the database at a
// consistent, known version rather than in an undefined state.
func (r *Runner) Up(ctx context.Context, migrations []Migration) error {
	if len(migrations) == 0 {
		return ErrNoMigrations
	}

	if err := r.lock(ctx); err != nil {
		return err
	}
	defer r.unlock(ctx)

	if _, err := r.conn.Exec(ctx, createSchemaTable); err != nil {
		return fmt.Errorf("migrate: create schema_migrations: %w", err)
	}

	applied, err := r.appliedChecksums(ctx)
	if err != nil {
		return err
	}

	for _, migration := range migrations {
		if existingChecksum, done := applied[migration.Version]; done {
			if existingChecksum != migration.Checksum {
				return fmt.Errorf("%w: version %d (%s) was applied with checksum %s but the file now hashes to %s",
					ErrChecksumMismatch, migration.Version, migration.Name, existingChecksum, migration.Checksum)
			}
			continue
		}

		start := time.Now()
		if err := r.apply(ctx, migration); err != nil {
			return err
		}
		r.logger.Info("applied migration",
			slog.Int64("version", migration.Version),
			slog.String("name", migration.Name),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		)
	}
	return nil
}

func (r *Runner) apply(ctx context.Context, migration Migration) error {
	tx, err := r.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate: begin for version %d: %w", migration.Version, err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	start := time.Now()
	if _, err := tx.Exec(ctx, migration.SQL); err != nil {
		return fmt.Errorf("migrate: apply version %d (%s): %w", migration.Version, migration.Name, err)
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, duration_ms) VALUES ($1, $2, $3, $4)`,
		migration.Version, migration.Name, migration.Checksum, time.Since(start).Milliseconds(),
	)
	if err != nil {
		return fmt.Errorf("migrate: record version %d: %w", migration.Version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate: commit version %d: %w", migration.Version, err)
	}
	return nil
}

func (r *Runner) appliedChecksums(ctx context.Context) (map[int64]string, error) {
	rows, err := r.conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrate: read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int64]string)
	for rows.Next() {
		var version int64
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("migrate: scan schema_migrations: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: iterate schema_migrations: %w", err)
	}
	return applied, nil
}

func (r *Runner) lock(ctx context.Context) error {
	// pg_advisory_lock blocks until granted; the caller's context deadline is what
	// stops a deadlocked start from hanging forever.
	if _, err := r.conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockID); err != nil {
		return fmt.Errorf("migrate: acquire advisory lock: %w", err)
	}
	return nil
}

func (r *Runner) unlock(ctx context.Context) {
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := r.conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, advisoryLockID); err != nil {
		r.logger.Warn("failed to release migration advisory lock", slog.String("error", err.Error()))
	}
}

// Version returns the highest applied migration version, or 0 for a fresh
// database.
func (r *Runner) Version(ctx context.Context) (int64, error) {
	var version *int64
	err := r.conn.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version)
	if err != nil {
		// A missing table simply means nothing has been applied yet.
		if strings.Contains(err.Error(), "does not exist") {
			return 0, nil
		}
		return 0, fmt.Errorf("migrate: read current version: %w", err)
	}
	if version == nil {
		return 0, nil
	}
	return *version, nil
}
