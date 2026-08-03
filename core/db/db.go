// Package db is suchi's SQLite bootstrap.
//
// The rule of the house: **one writer, many readers.** SQLite serializes
// writes at the file level; if two connections try to write concurrently
// one of them gets SQLITE_BUSY and we spend the next six months chasing
// ghosts. We prevent that structurally with two connection pools:
//
//   - DB.Write:  MaxOpenConns=1, BEGIN IMMEDIATE, busy_timeout=5000ms.
//     All writes go through this handle. It queues; it does not race.
//   - DB.Read:   ordinary pool, WAL readers concurrent with the writer.
//
// Both pools point at the same file. Both apply the boot pragmas below.
// The write pool additionally applies "PRAGMA foreign_keys=ON" per
// connection because SQLite scopes that pragma per-connection, not
// per-database.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DB is the pair of pools plus the file path. Close closes both.
type DB struct {
	Write *sql.DB
	Read  *sql.DB
	Path  string
}

// Open opens (or creates) the SQLite database at path and returns the
// two-pool handle. The caller is responsible for running migrations
// before serving traffic.
func Open(ctx context.Context, path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve db path: %w", err)
	}

	// Boot pragmas run in the DSN so the very first connection is
	// already correctly configured; nothing observes a pre-pragma window.
	//
	// _pragma=journal_mode(WAL) — many readers + one writer without blocking.
	// _pragma=synchronous(NORMAL) — durable across app crashes, faster than FULL,
	//   loses at most last-committed txn on OS crash. Standard WAL guidance.
	// _pragma=busy_timeout(5000) — 5s wait before SQLITE_BUSY. With the
	//   single-writer pool this is a belt to the suspenders.
	// _pragma=foreign_keys(ON) — the SQLite default is OFF; nothing about
	//   that default is a good idea.
	// _pragma=mmap_size(268435456) — 256MiB memory-mapped read window;
	//   improves read locality without touching the OS page cache accounting.
	// _pragma=temp_store(MEMORY) — temp tables/indexes stay in RAM.
	dsn := "file:" + abs + "?" + url.Values{
		"_pragma": []string{
			"journal_mode(WAL)",
			"synchronous(NORMAL)",
			"busy_timeout(5000)",
			"foreign_keys(ON)",
			"mmap_size(268435456)",
			"temp_store(MEMORY)",
		},
	}.Encode()

	write, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open write pool: %w", err)
	}
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	write.SetConnMaxLifetime(0) // never recycle the sole writer
	if err := ping(ctx, write); err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("write pool ping: %w", err)
	}

	read, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("open read pool: %w", err)
	}
	read.SetMaxOpenConns(0) // unbounded; WAL readers don't block each other
	read.SetMaxIdleConns(4)
	read.SetConnMaxIdleTime(5 * time.Minute)
	if err := ping(ctx, read); err != nil {
		_ = write.Close()
		_ = read.Close()
		return nil, fmt.Errorf("read pool ping: %w", err)
	}

	return &DB{Write: write, Read: read, Path: abs}, nil
}

// Close closes both pools. Errors from either are joined.
func (d *DB) Close() error {
	return errors.Join(d.Write.Close(), d.Read.Close())
}

// WriteTx runs fn inside a BEGIN IMMEDIATE transaction on the write pool.
// BEGIN IMMEDIATE acquires the RESERVED lock up front so we fail fast
// against a concurrent writer instead of discovering the conflict at
// COMMIT time (which is the SQLite footgun the single-writer pool exists
// to avoid — but IMMEDIATE keeps the discipline explicit).
func (d *DB) WriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.Write.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelSerializable,
	})
	if err != nil {
		return err
	}
	// modernc/sqlite honors LevelSerializable as BEGIN, not BEGIN IMMEDIATE.
	// Force it explicitly so our discipline is what we say it is.
	if _, err := tx.ExecContext(ctx, "ROLLBACK; BEGIN IMMEDIATE"); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("begin immediate: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func ping(ctx context.Context, d *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return d.PingContext(ctx)
}
