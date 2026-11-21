package poudrine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// A host administrator may use a root-managed symlink for worker storage.
// Validate its parents and its target in filesystem traversal order. Changes
// from the web broker use only real directories and regular files.
func inspectRootPath(path string, directory, allowLinks bool) (string, bool, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", false, fmt.Errorf("expected a clean absolute path: %s", path)
	}
	current := "/"
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	links := 0
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			current = filepath.Dir(current)
			continue
		}
		parent := current
		current = filepath.Join(parent, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			// A missing component below a writable parent can be claimed
			// before the worker creates it, even in a sticky directory.
			if err := protectedDirectory(parent); err != nil {
				return "", false, err
			}
			if slices.Contains(parts, "..") {
				return "", false, fmt.Errorf("missing directory in symlink target: %s", current)
			}
			return filepath.Join(append([]string{current}, parts...)...), false, nil
		}
		if err != nil {
			return "", false, err
		}
		if info.Sys().(*syscall.Stat_t).Uid != 0 {
			return "", false, fmt.Errorf("%s must be root-owned", current)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if !allowLinks {
				return "", false, fmt.Errorf("expected a real path at %s", current)
			}
			target, err := os.Readlink(current)
			if err != nil {
				return "", false, err
			}
			links++
			if links > 40 {
				return "", false, fmt.Errorf("%s: %w", path, syscall.ELOOP)
			}
			current = parent
			if filepath.IsAbs(target) {
				current = "/"
			}
			parts = append(strings.Split(target, "/"), parts...)
			continue
		}
		// A root-owned sticky ancestor such as /tmp is safe to traverse into
		// an existing root-owned entry. It cannot be a new object's parent.
		stickyAncestor := info.IsDir() && info.Mode()&os.ModeSticky != 0 && len(parts) > 0
		if info.Mode().Perm()&0022 != 0 && !stickyAncestor {
			return "", false, fmt.Errorf("%s must not be group- or world-writable", current)
		}
		if len(parts) > 0 && !info.IsDir() {
			return "", false, fmt.Errorf("expected a real directory at %s", current)
		}
	}
	info, err := os.Stat(current)
	if err != nil {
		return "", false, err
	}
	if directory {
		if !info.IsDir() {
			return "", false, fmt.Errorf("expected a directory at %s", current)
		}
		if err := protectedDirectory(current); err != nil {
			return "", false, err
		}
	} else {
		if !info.Mode().IsRegular() {
			return "", false, fmt.Errorf("expected a regular file at %s", current)
		}
		if err := protectedDirectory(filepath.Dir(current)); err != nil {
			return "", false, err
		}
	}
	return current, true, nil
}

func protectedDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0022 != 0 || info.Sys().(*syscall.Stat_t).Uid != 0 {
		return fmt.Errorf("%s must be a root-owned directory without group or world write access", path)
	}
	return nil
}

func (o options) checkWorkerStorage(mode string) error {
	var state string
	if mode != "worker" {
		parent, _, err := inspectRootPath(filepath.Dir(o.state), true, true)
		if err != nil {
			return fmt.Errorf("state: %w", err)
		}
		state = filepath.Join(parent, filepath.Base(o.state))
	}
	var paths []settingPath
	for _, suffix := range []string{"", ".lock", ".nonces", ".admin.pub", ".setup-token"} {
		paths = append(paths, settingPath{"configuration file", o.configPath + suffix, false})
	}
	for _, p := range o.builder.workerPaths() {
		// Poudriere manages the published repository (including symlinks)
		// below basefs. The worker writes directly to basefs and its metadata.
		if p.path == o.builder.RepositoryDir() && p.field == "basefs" {
			continue
		}
		resolved, _, err := inspectRootPath(p.path, p.directory, true)
		if err != nil {
			return fmt.Errorf("%s: %w", p.field, err)
		}
		if state != "" && within(resolved, state) {
			return fmt.Errorf("%s: worker storage resolves into the private state directory", p.field)
		}
		if within(resolved, publicStatsDir) {
			return fmt.Errorf("%s: worker storage resolves into the public stats directory", p.field)
		}
		paths = append(paths, settingPath{p.field, resolved, p.directory})
	}
	return validatePathLayout(paths)
}
