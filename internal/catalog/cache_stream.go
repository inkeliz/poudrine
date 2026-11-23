package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/inkeliz/poudrine/internal/packageinfo"
)

const maxUpstreamDownloads = 16

// One append-only temporary file supplies independent readers. Every byte is
// retained on disk for late, slow and ranged readers; payloads are not retained
// in Go heap buffers. Only verified bytes can become a persistent cache hit.
type archiveDownload struct {
	file      *os.File
	size      int64
	key       string
	ctx       context.Context
	cancel    context.CancelFunc
	ready     chan struct{}
	readyOnce sync.Once
	done      chan struct{}
	mu        sync.Mutex
	changed   chan struct{}
	available int64
	err       error
	complete  bool
	// The following lifecycle fields are protected by Cache.mu.
	users    int
	started  bool
	retired  bool
	finished bool
}

// ArchiveReader has its own position and cancellation. Closing one reader
// cannot cancel other readers; the upstream stops when the last reader leaves.
type ArchiveReader struct {
	file     *os.File
	download *archiveDownload
	cache    *Cache
	ctx      context.Context
	cancel   context.CancelFunc
	offset   int64
	once     sync.Once
}

func (c *Cache) Stream(ctx context.Context, p packageinfo.Package) (*ArchiveReader, error) {
	if _, err := packageHasher(p.Sum); err != nil {
		return nil, err
	}
	if p.Size < 0 || p.Size >= 1<<62 {
		return nil, errUntrustedPackage
	}
	key := PackageCacheKey(p.Sum)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		f, err := c.hit(key, p)
		c.mu.Unlock()
		if f != nil || err != nil {
			if err != nil {
				return nil, err
			}
			return &ArchiveReader{file: f, ctx: ctx}, nil
		}
		// singleflight coalesces creation, returning the live stream rather than
		// waiting for its payload. The map retains it while readers still use it.
		value, err, _ := c.flights.Do(key, func() (any, error) {
			c.mu.Lock()
			existing := c.downloads[key]
			_, cached := c.files[key]
			c.mu.Unlock()
			if existing != nil {
				return existing, nil
			}
			if cached {
				return nil, nil
			} // Retry the cache hit with an independent fd.
			file, err := os.CreateTemp(c.dir, ".download-")
			if err != nil {
				return nil, err
			}
			shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
			d := &archiveDownload{file: file, key: key, size: p.Size, ctx: shared, cancel: cancel,
				ready: make(chan struct{}), done: make(chan struct{}), changed: make(chan struct{})}
			c.mu.Lock()
			c.downloads[key] = d
			c.mu.Unlock()
			return d, nil
		})
		if err != nil {
			return nil, err
		}
		if value == nil {
			continue
		}
		d, ok := value.(*archiveDownload)
		if !ok {
			return nil, errors.New("invalid shared download result")
		}
		c.mu.Lock()
		if d.retired {
			c.mu.Unlock()
			continue
		}
		if d.size != p.Size {
			c.mu.Unlock()
			return nil, errUntrustedPackage
		}
		d.users++
		start := !d.started
		d.started = true
		c.mu.Unlock()
		readerCtx, cancel := context.WithCancel(ctx)
		r := &ArchiveReader{download: d, cache: c, ctx: readerCtx, cancel: cancel}
		if start {
			go c.runDownload(d, p)
		}
		select {
		case <-ctx.Done():
			r.Close()
			return nil, ctx.Err()
		case <-d.ready:
		}
		d.mu.Lock()
		err = d.err
		d.mu.Unlock()
		if err != nil {
			r.Close()
			return nil, err
		}
		return r, nil
	}
}

func (c *Cache) runDownload(d *archiveDownload, p packageinfo.Package) {
	err := func() error {
		select {
		case c.fill <- struct{}{}:
		case <-d.ctx.Done():
			return d.ctx.Err()
		}
		defer func() { <-c.fill }()
		req, err := http.NewRequestWithContext(d.ctx, "GET", c.upstream.JoinPath(p.Path).String(), nil)
		if err != nil {
			return err
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("upstream package: HTTP %d", resp.StatusCode)
		}
		if resp.ContentLength >= 0 && resp.ContentLength != p.Size {
			return errUntrustedPackage
		}
		d.readyOnce.Do(func() { close(d.ready) })
		if err := VerifyPackage(resp.Body, d, p); err != nil {
			return err
		}
		return c.storeDownload(d.ctx, p, d.key, d.file)
	}()
	d.mu.Lock()
	d.err, d.complete = err, true
	c.mu.Lock()
	d.finished = true
	cleanup := d.users == 0
	c.mu.Unlock()
	close(d.done)
	close(d.changed)
	d.mu.Unlock()
	d.readyOnce.Do(func() { close(d.ready) })
	d.cancel()
	if cleanup {
		c.cleanupDownload(d)
	}
}

func (d *archiveDownload) Write(p []byte) (int, error) {
	n, err := d.file.Write(p)
	d.mu.Lock()
	d.available += int64(n)
	close(d.changed)
	d.changed = make(chan struct{})
	d.mu.Unlock()
	return n, err
}

func (d *archiveDownload) readAt(ctx context.Context, p []byte, offset int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		d.mu.Lock()
		if d.complete && d.err != nil {
			err := d.err
			d.mu.Unlock()
			return 0, err
		}
		available := min(d.available, d.size)
		if offset < available {
			p = p[:min(int64(len(p)), available-offset)]
			d.mu.Unlock()
			return d.file.ReadAt(p, offset)
		}
		if d.complete {
			d.mu.Unlock()
			return 0, io.EOF
		}
		changed := d.changed
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-changed:
		}
	}
}

func (r *ArchiveReader) Read(p []byte) (int, error) {
	if r.file != nil {
		return r.file.Read(p)
	}
	n, err := r.download.readAt(r.ctx, p, r.offset)
	r.offset += int64(n)
	return n, err
}

func (r *ArchiveReader) Seek(offset int64, whence int) (int64, error) {
	if r.file != nil {
		return r.file.Seek(offset, whence)
	}
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = r.offset
	case io.SeekEnd:
		base = r.download.size
	default:
		return 0, errors.New("invalid seek origin")
	}
	if offset < -base || offset > int64(^uint64(0)>>1)-base {
		return 0, errors.New("invalid seek offset")
	}
	r.offset = base + offset
	return r.offset, nil
}

// Verify waits for the entire upstream archive, including size/hash checks and
// cache publication. Call before completing an HTTP body, including a range.
func (r *ArchiveReader) Verify() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if r.file != nil {
		return nil
	}
	select {
	case <-r.ctx.Done():
		return r.ctx.Err()
	case <-r.download.done:
	}
	r.download.mu.Lock()
	defer r.download.mu.Unlock()
	return r.download.err
}

func (r *ArchiveReader) Close() error {
	var err error
	r.once.Do(func() {
		if r.file != nil {
			err = r.file.Close()
			return
		}
		r.cancel()
		c, d := r.cache, r.download
		c.mu.Lock()
		d.users--
		if d.users == 0 {
			d.retired = true
			delete(c.downloads, d.key)
			d.cancel()
		}
		cleanup := d.users == 0 && d.finished
		c.mu.Unlock()
		if cleanup {
			c.cleanupDownload(d)
		}
	})
	return err
}

func (c *Cache) cleanupDownload(d *archiveDownload) {
	os.Remove(d.file.Name())
	d.file.Close()
}
