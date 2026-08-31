package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Backup creates a transactionally consistent SQLite snapshot without opening
// the source through Store.Open. This is important before upgrades because a
// backup must capture the pre-migration database.
func Backup(ctx context.Context, sourcePath, destinationPath string) (err error) {
	if strings.TrimSpace(sourcePath) == "" || strings.TrimSpace(destinationPath) == "" {
		return errors.New("source and destination SQLite paths are required")
	}
	sourceAbsolute, err := filepath.Abs(sourcePath)
	if err != nil {
		return fmt.Errorf("resolve source SQLite path: %w", err)
	}
	destinationAbsolute, err := filepath.Abs(destinationPath)
	if err != nil {
		return fmt.Errorf("resolve backup SQLite path: %w", err)
	}
	if filepath.Clean(sourceAbsolute) == filepath.Clean(destinationAbsolute) {
		return errors.New("backup destination must differ from the source database")
	}
	if _, err := os.Stat(sourceAbsolute); err != nil {
		return fmt.Errorf("inspect source SQLite database: %w", err)
	}
	if _, err := os.Stat(destinationAbsolute); err == nil {
		return errors.New("backup destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect backup destination: %w", err)
	}
	if parent := filepath.Dir(destinationAbsolute); parent != "." {
		if info, err := os.Stat(parent); err != nil {
			return fmt.Errorf("inspect backup directory: %w", err)
		} else if !info.IsDir() {
			return errors.New("backup parent is not a directory")
		}
	}

	db, err := sql.Open("sqlite", connectionDSN(sourceAbsolute))
	if err != nil {
		return fmt.Errorf("open source SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	defer func() {
		if err != nil {
			_ = os.Remove(destinationAbsolute)
		}
	}()
	if _, err = db.ExecContext(ctx, `VACUUM INTO ?`, destinationAbsolute); err != nil {
		return fmt.Errorf("create SQLite backup: %w", err)
	}
	if err = verifyBackup(ctx, destinationAbsolute); err != nil {
		return err
	}
	return nil
}

func verifyBackup(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", connectionDSN(path))
	if err != nil {
		return fmt.Errorf("open SQLite backup for verification: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&result); err != nil {
		return fmt.Errorf("verify SQLite backup: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("verify SQLite backup: quick_check returned %q", result)
	}
	return nil
}

// Restore verifies and atomically installs a backup. The caller must ensure no
// RedDotRelay process is using destinationPath.
func Restore(ctx context.Context, backupPath, destinationPath string) (err error) {
	backupAbsolute, err := filepath.Abs(backupPath)
	if err != nil {
		return fmt.Errorf("resolve SQLite backup path: %w", err)
	}
	destinationAbsolute, err := filepath.Abs(destinationPath)
	if err != nil {
		return fmt.Errorf("resolve destination SQLite path: %w", err)
	}
	if filepath.Clean(backupAbsolute) == filepath.Clean(destinationAbsolute) {
		return errors.New("restore backup must differ from the destination database")
	}
	if err := verifyBackup(ctx, backupAbsolute); err != nil {
		return err
	}

	directory := filepath.Dir(destinationAbsolute)
	temporary, err := os.CreateTemp(directory, ".reddotrelay-restore-*.db")
	if err != nil {
		return fmt.Errorf("create staged restore: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	source, err := os.Open(backupAbsolute)
	if err != nil {
		_ = temporary.Close()
		return fmt.Errorf("open SQLite backup: %w", err)
	}
	if _, err = temporary.ReadFrom(source); err != nil {
		_ = source.Close()
		_ = temporary.Close()
		return fmt.Errorf("stage SQLite restore: %w", err)
	}
	if err = source.Close(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("close SQLite backup: %w", err)
	}
	if err = temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync staged SQLite restore: %w", err)
	}
	if err = temporary.Close(); err != nil {
		return fmt.Errorf("close staged SQLite restore: %w", err)
	}
	if err = verifyBackup(ctx, temporaryPath); err != nil {
		return err
	}

	rollbackPath := destinationAbsolute + ".restore-rollback"
	if _, err := os.Stat(rollbackPath); err == nil {
		return errors.New("restore rollback file already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect restore rollback path: %w", err)
	}
	destinationExists := false
	if _, err := os.Stat(destinationAbsolute); err == nil {
		destinationExists = true
		if err := os.Rename(destinationAbsolute, rollbackPath); err != nil {
			return fmt.Errorf("preserve current SQLite database: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect destination SQLite database: %w", err)
	}
	defer func() {
		if err != nil && destinationExists {
			_ = os.Rename(rollbackPath, destinationAbsolute)
		}
	}()
	for _, suffix := range []string{"-wal", "-shm"} {
		if removeErr := os.Remove(destinationAbsolute + suffix); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("remove stale SQLite%s sidecar: %w", suffix, removeErr)
		}
	}
	if err = os.Rename(temporaryPath, destinationAbsolute); err != nil {
		return fmt.Errorf("install SQLite restore: %w", err)
	}
	if destinationExists {
		if err = os.Remove(rollbackPath); err != nil {
			return fmt.Errorf("remove restore rollback file: %w", err)
		}
	}
	return nil
}
