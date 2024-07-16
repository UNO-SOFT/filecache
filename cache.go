// Copyright 2021, 2022 Tamás Gulácsi. All rights reserved.
// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package filecache

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rogpeppe/go-internal/cache"
	"github.com/rogpeppe/go-internal/lockedfile"
)

// An ActionID is a cache action key, the hash of a complete description of a
// repeatable computation (command line, environment variables,
// input file contents, executable contents).
type ActionID = cache.ActionID

// An OutputID is a cache output key, the hash of an output of a computation.
type OutputID = cache.OutputID

func NewActionID(p []byte) ActionID { return ActionID(SumID(p)) }

// A Cache is a package cache, backed by a file system directory tree.
type Cache struct {
	lastTrim time.Time
	c        *cache.Cache
	now      func() time.Time

	dir                     string
	trimSize                int64
	trimInterval, trimLimit time.Duration

	mu sync.Mutex
}

// Open opens and returns the cache in the given directory.
//
// It is safe for multiple processes on a single machine to use the
// same cache directory in a local file system simultaneously.
// They will coordinate using operating system file locks and may
// duplicate effort but will not corrupt the cache.
//
// However, it is NOT safe for multiple processes on different machines
// to share a cache directory (for example, if the directory were stored
// in a network file system). File locking is notoriously unreliable in
// network file systems and may not suffice to protect the cache.
func Open(dir string) (*Cache, error) {
	c, err := cache.Open(dir)
	if err != nil {
		return nil, err
	}
	return &Cache{c: c, now: time.Now, dir: dir, trimInterval: 5 * time.Minute, trimLimit: 24 * time.Hour}, nil
}

// Put stores the given output in the cache as the output for the action ID.
// It may read file twice. The content of file must not change between the two passes.
func (C *Cache) Put(id ActionID, file io.ReadSeeker) (OutputID, int64, error) {
	C.mu.Lock()
	defer C.mu.Unlock()
	C.trim()
	return C.c.Put(id, file)
}

// Get looks up the action ID in the cache,
// returning the corresponding output ID and file size, if any.
// Note that finding an output ID does not guarantee that the
// saved file for that output ID is still available.
func (C *Cache) Get(id ActionID) (cache.Entry, error) {
	C.mu.Lock()
	defer C.mu.Unlock()
	return C.c.Get(id)
}

// GetBytes looks up the action ID in the cache and returns
// the corresponding output bytes.
// GetBytes should only be used for data that can be expected to fit in memory.
func (C *Cache) GetBytes(id ActionID) ([]byte, cache.Entry, error) {
	C.mu.Lock()
	defer C.mu.Unlock()
	return C.c.GetBytes(id)
}

// GetFile looks up the action ID in the cache and returns
// the name of the corresponding data file.
func (C *Cache) GetFile(id ActionID) (file string, entry cache.Entry, err error) {
	C.mu.Lock()
	defer C.mu.Unlock()
	return C.c.GetFile(id)
}

// SetTrimInterval set the time intervals between Trims (on Put).
func (C *Cache) SetTrimInterval(d time.Duration) {
	C.mu.Lock()
	C.trimInterval = d
	C.mu.Unlock()
}

// SetTrimLimit set the max age of entries.
func (C *Cache) SetTrimLimit(d time.Duration) {
	C.mu.Lock()
	C.trimInterval = d
	C.mu.Unlock()
}

// SetTrimSize set the trim file size (checked on Put).
func (C *Cache) SetTrimSize(n int64) {
	C.mu.Lock()
	C.trimSize = n
	C.mu.Unlock()
}

// Trim removes old cache entries that are likely not to be reused.
func (C *Cache) Trim() error {
	C.mu.Lock()
	defer C.mu.Unlock()
	return C.trim()
}

func (C *Cache) trim() error {
	now := C.now()
	if !C.lastTrim.IsZero() && now.Sub(C.lastTrim) < C.trimInterval {
		return nil
	}

	// We maintain in dir/trim.txt the time of the last completed cache trim.
	// If the cache has been trimmed recently enough, do nothing.
	// This is the common case.
	// If the trim file is corrupt, detected if the file can't be parsed, or the
	// trim time is too far in the future, attempt the trim anyway. It's possible that
	// the cache was full when the corruption happened. Attempting a trim on
	// an empty cache is cheap, so there wouldn't be a big performance hit in that case.
	log.Println("trimInterval:", C.trimInterval)
	if C.trimInterval > 0 {
		if data, err := lockedfile.Read(filepath.Join(C.dir, "trim.txt")); err == nil {
			if t, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
				C.lastTrim = time.Unix(t, 0)
				if d := now.Sub(C.lastTrim); d < C.trimInterval {
					return nil
				}
			}
		}
	}

	// Trim each of the 256 subdirectories.
	// We subtract an additional mtimeInterval
	// to account for the imprecision of our "last used" mtimes.
	cutoffTime := now.Add(-C.trimLimit)
	cutoffSize := C.trimSize
	sizeCutoffTime := now.Add(-C.trimInterval)
	for i := 0; i < 256; i++ {
		subdir := filepath.Join(C.dir, fmt.Sprintf("%02x", i))
		C.trimSubdir(subdir, cutoffTime, cutoffSize, sizeCutoffTime)
	}
	C.lastTrim = now

	// Ignore errors from here: if we don't write the complete timestamp, the
	// cache will appear older than it is, and we'll trim it again next time.
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d", now.Unix())
	if err := lockedfile.Write(filepath.Join(C.dir, "trim.txt"), &b, 0666); err != nil {
		return err
	}

	return nil
}

// trimSubdir trims a single cache subdirectory.
func (C *Cache) trimSubdir(subdir string, cutoffTime time.Time, cutoffSize int64, sizeCutoffTime time.Time) {
	// Read all directory entries from subdir before removing
	// any files, in case removing files invalidates the file offset
	// in the directory scan. Also, ignore error from f.Readdirnames,
	// because we don't care about reporting the error and we still
	// want to process any entries found before the error.
	f, err := os.Open(subdir)
	if err != nil {
		return
	}
	names, _ := f.Readdirnames(-1)
	f.Close()

	for _, name := range names {
		// Remove only cache entries (xxxx-a and xxxx-d).
		if !strings.HasSuffix(name, "-a") && !strings.HasSuffix(name, "-d") {
			continue
		}
		entry := filepath.Join(subdir, name)
		info, err := os.Stat(entry)
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoffTime) ||
			(cutoffSize > 0 && info.Size() > cutoffSize && info.ModTime().Before(sizeCutoffTime)) {
			os.Remove(entry)
		}
	}
}
