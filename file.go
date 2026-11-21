package poudrine

import (
	"io/fs"
	"os"
	"path/filepath"
)

func writeAtomicFile(name string, data []byte, mode fs.FileMode) error {
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
	return os.Rename(f.Name(), name)
}
