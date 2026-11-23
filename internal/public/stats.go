package public

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/statusproto"
)

// Both the directory and the file stay root-owned. Only the private HTTP
// child inherits the supervisor's writable descriptor.
func OpenManagedStats() (*os.File, error) {
	_, exists, err := config.InspectRootPath(config.PublicStatsDir, true, false)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := os.Mkdir(config.PublicStatsDir, 0755); err != nil {
			return nil, err
		}
		if err := os.Chmod(config.PublicStatsDir, 0755); err != nil {
			return nil, err
		}
	}
	return OpenStatsWriter(config.PublicStatsFile)
}

func OpenStatsWriter(path string) (*os.File, error) {
	flags := os.O_WRONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	file, err := os.OpenFile(path, flags|os.O_CREATE|os.O_EXCL, 0644)
	created := err == nil
	if errors.Is(err, os.ErrExist) {
		file, err = os.OpenFile(path, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st == nil || !info.Mode().IsRegular() || st.Nlink != 1 || int(st.Uid) != os.Geteuid() || (!created && info.Mode().Perm() != 0644) {
			err = fmt.Errorf("expected a singly linked 0644 regular file owned by the publisher at %s", path)
		}
	}
	if err == nil {
		// Two repositories must never publish into the same inode.
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err == nil && created {
		err = file.Chmod(0644)
	}
	if err == nil {
		// A new catalog generation has no verified counts yet.
		err = file.Truncate(0)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func WritePublicStats(file *os.File, s catalog.CatalogStats) error {
	// Keep the inode pinned by the public reader.
	record := (statusproto.Counts{Packages: int64(s.Packages), Ahead: int64(s.Ahead)}).Record()
	n, err := syscall.Pwrite(int(file.Fd()), record, 0)
	if n > 0 && n != len(record) {
		// Do not leave a partial record available after a short device write.
		return errors.Join(io.ErrShortWrite, err, file.Truncate(0))
	}
	return err
}
