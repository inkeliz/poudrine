package poudrine

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The poller serializes scans. Only this Poudriere run's successful builds
// can add archives. Completed captures survive the native repository rename.
type liveArchiveStore struct {
	dir, repoDir string
	started      time.Time
	packages     map[string]livePackage
	failures     map[string]liveCaptureFailure
}

type liveCaptureFailure struct {
	message string
	retry   time.Time
}

func (s *liveArchiveStore) scan(ctx context.Context, cfg BuilderConfig, started time.Time) (map[string]livePackage, error) {
	if !s.started.Equal(started) {
		s.started = started
		s.packages = make(map[string]livePackage)
		s.failures = make(map[string]liveCaptureFailure)
	}
	if started.IsZero() {
		return nil, nil
	}
	err := s.scanRun(ctx, cfg, started.UTC().Format(buildNameFormat))
	if errors.Is(err, os.ErrNotExist) {
		err = nil // The worker may be preparing the queue or committing it.
	}
	return maps.Clone(s.packages), err
}

func (s *liveArchiveStore) scanRun(ctx context.Context, cfg BuilderConfig, name string) error {
	logs, err := openBulkLogs(cfg)
	if err != nil {
		return err
	}
	defer logs.Close()
	run, err := openLogPath(logs, cfg.Jail+"-"+cfg.Ports+"/"+name, true)
	if err != nil {
		return err
	}
	defer run.Close()

	repo, err := os.OpenFile(s.repoDir, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer repo.Close()
	stage, err := openLogPath(repo, ".building", true)
	if err != nil {
		return err
	}
	defer stage.Close()
	// Poudriere writes .buildname before launching builders. Check it via the
	// same directory descriptor used for All, even across an atomic commit.
	marker, err := openLogPath(stage, ".buildname", false)
	if err != nil {
		return err
	}
	defer marker.Close()
	st, err := marker.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("unexpected Poudriere .buildname file")
	}
	id, err := io.ReadAll(io.LimitReader(marker, 128))
	if err != nil || strings.TrimSpace(string(id)) != name {
		return err
	}
	all, err := openLogPath(stage, "All", true)
	if err != nil {
		return err
	}
	defer all.Close()

	err = scanBuildResults(ctx, run, "built", func(_ string, fields []string) bool {
		origin, id := fields[0], fields[1]
		name, version := splitBuildPackage(id)
		if name == "" || s.packages[name].manifest.pkg.Version == version {
			return true
		}
		failure := s.failures[id]
		if time.Now().Before(failure.retry) {
			return true
		}
		p, err := s.capture(ctx, all, id, origin)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			// A failed copy (e.g. temporary low disk space) can be retried.
			// Other builders in the same batch can still publish normally.
			if !errors.Is(err, os.ErrNotExist) && err.Error() != failure.message {
				log.Printf("stage built package %s: %v", id, err)
			}
			s.failures[id] = liveCaptureFailure{message: err.Error(), retry: time.Now().Add(time.Minute)}
			return true
		}
		delete(s.failures, id)
		s.packages[name] = p
		return true
	})
	if err == nil {
		err = ctx.Err()
	}
	return err
}

func (s *liveArchiveStore) capture(ctx context.Context, all *os.File, id, origin string) (livePackage, error) {
	source, err := openLogPath(all, id+".pkg", false)
	if err != nil {
		return livePackage{}, err
	}
	defer source.Close()
	before, err := source.Stat()
	if err != nil {
		return livePackage{}, err
	}
	if !before.Mode().IsRegular() || before.Size() <= 0 {
		return livePackage{}, errors.New("unexpected staged package file")
	}
	fetched, err := fetchedByPoudriere(s.repoDir, id+".pkg", before)
	if err != nil {
		return livePackage{}, err
	}
	if fetched {
		return livePackage{}, errors.New("staged archive belongs to Poudriere's download cache")
	}

	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return livePackage{}, err
	}
	tmp, err := os.CreateTemp(s.dir, ".capture-")
	if err != nil {
		return livePackage{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	stop := context.AfterFunc(ctx, func() { source.Close() })
	defer stop()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(source, before.Size()))
	if err != nil {
		return livePackage{}, err
	}
	if err := ctx.Err(); err != nil {
		return livePackage{}, err
	}
	after, err := source.Stat()
	if err != nil {
		return livePackage{}, err
	}
	if n != before.Size() || !sameArchive(before, after) {
		return livePackage{}, errors.New("staged package changed while copying")
	}
	sum := hex.EncodeToString(h.Sum(nil))
	m, err := readLiveManifest(tmp, sum)
	if err != nil {
		return livePackage{}, err
	}
	// The result list can omit the default flavor, including when it names
	// a subpackage. Honor every flavor and subpackage it explicitly names.
	unflavored := m.pkg.Origin
	if _, sub, ok := strings.Cut(m.pkg.BuildOrigin, "~"); ok {
		unflavored += "~" + sub
	}
	if m.pkg.Name+"-"+m.pkg.Version != id || (origin != m.pkg.BuildOrigin && origin != unflavored) {
		return livePackage{}, fmt.Errorf("compact manifest does not match successful build %s %s", origin, id)
	}

	dest := filepath.Join(s.dir, sum+".pkg")
	// Link only validated bytes into the addressable namespace. A prior run
	// may already own this path; verify those bytes before reusing the inode.
	if err := os.Link(tmp.Name(), dest); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return livePackage{}, err
		}
		old, err := os.Lstat(dest)
		if err != nil {
			return livePackage{}, err
		}
		file := &compiledArchive{path: dest, info: old}
		if old.Mode().IsRegular() && file.verify(m.pkg) == nil {
			return livePackage{manifest: m, file: file}, nil
		}
		if err := os.Rename(tmp.Name(), dest); err != nil {
			return livePackage{}, err
		}
	}
	info, err := tmp.Stat()
	if err != nil {
		return livePackage{}, err
	}
	return livePackage{manifest: m, file: &compiledArchive{path: dest, info: info, verified: true}}, nil
}

// Read the compact header from the same descriptor whose bytes the caller
// hashed. Keep pkg's complete manifest, including dependencies and future
// fields, and supply the fields a repository adds to an archive's manifest.
func readLiveManifest(f *os.File, sum string) (manifest, error) {
	info, err := f.Stat()
	if err != nil {
		return manifest{}, err
	}
	if len(sum) != 64 || !isHex(sum) || sum != strings.ToLower(sum) || !info.Mode().IsRegular() {
		return manifest{}, errors.New("invalid live package archive")
	}
	const limit = 4 << 20
	src, err := decompressArchive(io.NewSectionReader(f, 0, limit+512))
	if err != nil {
		return manifest{}, err
	}
	defer src.Close()
	t := tar.NewReader(io.LimitReader(src, limit+512))
	h, err := t.Next()
	if err != nil {
		return manifest{}, err
	}
	if strings.TrimPrefix(h.Name, "./") != "+COMPACT_MANIFEST" || h.Typeflag != tar.TypeReg || h.Size <= 0 || h.Size > limit {
		return manifest{}, errors.New("missing or invalid +COMPACT_MANIFEST header")
	}
	raw, err := io.ReadAll(t)
	if err != nil {
		return manifest{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return manifest{}, err
	}
	if fields == nil {
		return manifest{}, errors.New("empty compact manifest")
	}
	path := liveURLPrefix + sum + ".pkg"
	fields["path"], _ = json.Marshal(path)
	fields["repopath"] = fields["path"]
	fields["sum"], _ = json.Marshal(sum)
	fields["pkgsize"] = json.RawMessage(strconv.FormatInt(info.Size(), 10))
	raw, err = json.Marshal(fields)
	if err != nil {
		return manifest{}, err
	}
	parsed, err := parseCatalog(raw, "packagesite.yaml", true)
	if err != nil {
		return manifest{}, err
	}
	for _, m := range parsed.byName {
		p := m.pkg
		if p.Path == path && p.Sum == sum && p.Size == info.Size() && p.ABI != "" && buildOrigin.MatchString(p.BuildOrigin) && validIssuePackage(p.Name+"-"+p.Version) {
			return m, nil
		}
	}
	return manifest{}, errors.New("invalid package identity in compact manifest")
}
