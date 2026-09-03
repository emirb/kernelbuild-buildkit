package main

import (
	"archive/tar"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// The serial untar loop was the measured cost of a seed hydrate: ~4 syscalls
// per entry across ~100k object files, one file at a time (23.8 s for a
// 401 MB seed on 4 vCPU, of which S3 transfer was ~3 s). Small regular files
// are therefore written by a pool of workers, sharded by path hash so a
// repeated name keeps its in-stream order. Everything order-sensitive —
// directories, symlinks, hardlinks, and large files (streamed, not buffered)
// — stays inline in the reader loop behind a flush barrier.

// untarInlineLimit: regular files above this stream inline instead of being
// buffered for a worker (a vmlinux or built-in.a would otherwise sit whole in
// memory). Var, not const, so tests can lower it.
var untarInlineLimit int64 = 8 << 20

type writeJob struct {
	name  string
	mode  fs.FileMode
	mtime time.Time
	data  []byte
}

type untarPool struct {
	root  *os.Root
	chans []chan writeJob
	// pending counts dispatched-but-unfinished jobs; flush() waits it out.
	// Add happens only on the reader goroutine and Wait only after its Adds,
	// so the WaitGroup reuse rules hold.
	pending sync.WaitGroup
	workers sync.WaitGroup
	bufs    sync.Pool

	mu  sync.Mutex
	err error
}

func newUntarPool(root *os.Root) *untarPool {
	n := max(min(runtime.GOMAXPROCS(0), 8), 1)
	p := &untarPool{root: root, chans: make([]chan writeJob, n)}
	p.bufs.New = func() any { return make([]byte, 32<<10) }
	for i := range p.chans {
		ch := make(chan writeJob, 4)
		p.chans[i] = ch
		p.workers.Go(func() {
			for job := range ch {
				if p.failed() == nil {
					if err := writeFileBytes(p.root, job.name, job.mode, job.mtime, job.data); err != nil {
						p.fail(fmt.Errorf("%s: %w", job.name, err))
					}
				}
				p.bufs.Put(job.data[:cap(job.data)]) //nolint:staticcheck // slice, not pointer: 24B header per put is fine here
				p.pending.Done()
			}
		})
	}
	return p
}

func (p *untarPool) fail(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
}

func (p *untarPool) failed() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// dispatch reads one regular-file entry's content (the tar reader is serial;
// only the WRITE is deferred) and hands it to the worker owning the path.
func (p *untarPool) dispatch(name string, hdr *tar.Header, tr io.Reader) error {
	if err := p.failed(); err != nil {
		return err
	}
	buf := p.bufs.Get().([]byte)
	if int64(cap(buf)) < hdr.Size {
		buf = make([]byte, hdr.Size)
	}
	buf = buf[:hdr.Size]
	if _, err := io.ReadFull(tr, buf); err != nil {
		p.bufs.Put(buf[:cap(buf)]) //nolint:staticcheck // see above
		return fmt.Errorf("%s: read: %w", name, err)
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	p.pending.Add(1)
	shard := h.Sum32() % uint32(len(p.chans)) //nolint:gosec // len(p.chans) is at most 8
	p.chans[shard] <- writeJob{name: name, mode: hdr.FileInfo().Mode().Perm(), mtime: hdr.ModTime, data: buf}
	return nil
}

// flush drains every queued write — the barrier before anything
// order-sensitive (symlinks, hardlinks, large inline files, EOF).
func (p *untarPool) flush() error {
	p.pending.Wait()
	return p.failed()
}

// close flushes, stops the workers, and returns the first error.
func (p *untarPool) close() error {
	p.pending.Wait()
	for _, ch := range p.chans {
		close(ch)
	}
	p.workers.Wait()
	return p.failed()
}

// writeFileBytes is writeEntry for an in-memory body: same
// remove-then-create-exclusively symlink defense, same mode/mtime restore.
func writeFileBytes(root *os.Root, name string, mode fs.FileMode, mtime time.Time, data []byte) error {
	if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := root.Chmod(name, mode); err != nil {
		return err
	}
	return root.Chtimes(name, mtime, mtime)
}
