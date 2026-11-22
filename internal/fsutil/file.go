package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

func WriteAtomicFile(name string, data []byte, mode fs.FileMode) error {
	return publishFile(name, data, mode, os.Rename)
}

// CreateFileOnce publishes the complete file only if the destination does not exist.
// Concurrent creators and retries preserve the first published value.
func CreateFileOnce(name string, data []byte, mode fs.FileMode) error {
	return publishFile(name, data, mode, func(tmp, dst string) error {
		err := os.Link(tmp, dst)
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	})
}

func publishFile(name string, data []byte, mode fs.FileMode, publish func(string, string) error) error {
	f, err := os.CreateTemp(filepath.Dir(name), ".poudrine-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return publish(f.Name(), name)
}
