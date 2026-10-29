package beesuh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mrchypark/rhiza"
)

// InitStorage opens and closes Rhiza without running application migrations.
// Use the same data directory when subsequently calling Open.
func InitStorage(ctx context.Context, dataDir string) error {
	db, lock, err := openStorage(ctx, dataDir)
	if err != nil {
		return err
	}
	// Keep the outer lock held until Rhiza has finished closing.
	closeErr := db.Close()
	return errors.Join(closeErr, lock.Close())
}

func openStorage(ctx context.Context, dataDir string) (*rhiza.DB, *os.File, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, nil, errors.New("beesuh: data directory is required")
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "runtime.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, nil, errors.Join(fmt.Errorf("beesuh: data directory is in use: %w", err), f.Close())
	}
	db, err := rhiza.Open(ctx, rhiza.Config{NodeID: "beesuh", DataDir: filepath.Join(dataDir, "rhiza"), PeerAddr: "127.0.0.1:0"})
	if err != nil {
		return nil, nil, errors.Join(err, f.Close())
	}
	return db, f, nil
}
