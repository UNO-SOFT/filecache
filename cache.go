// Copyright 2021, 2022 Tamás Gulácsi. All rights reserved.
// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package filecache

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
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

const (
	DefaultMaxSize      = 0
	DefaultTrimInterval = 5 * time.Minute
	DefaultTrimLimit    = 24 * time.Hour
	DefaultTrimSize     = 100 << 20
)

// A Cache is a package cache, backed by a file system directory tree.
type Cache struct {
	lastTrim time.Time
	c        *cache.Cache
	now      func() time.Time

	dir                     string
	trimSize, maxSize       int64
	trimInterval, trimLimit time.Duration
	logger                  *slog.Logger

	mu sync.Mutex
}
type cacheOption func(*Cache)

func WithMaxSize(n int64) cacheOption {
	return func(C *Cache) {
		if n < 0 {
			n = DefaultMaxSize
		}
		C.maxSize = n
	}
}
func WithNow(f func() time.Time) cacheOption {
	return func(C *Cache) {
		if f != nil {
			C.now = f
		}
	}
}
func WithTrimSize(n int64) cacheOption {
	return func(C *Cache) {
		if n < 0 {
			n = DefaultTrimSize
		}
		C.trimSize = n
	}
}
func WithTrimInterval(d time.Duration) cacheOption {
	return func(C *Cache) {
		if d < 0 {
			d = DefaultTrimInterval
		}
		C.trimInterval = d
	}
}
func WithTrimLimit(d time.Duration) cacheOption {
	return func(C *Cache) {
		if d < 0 {
			d = DefaultTrimLimit
		}
		C.trimLimit = d
	}
}

// WithLogger sets the logger to the given Logger - iff it is not nil.
func WithLogger(lgr *slog.Logger) cacheOption {
	return func(C *Cache) {
		if lgr != nil {
			C.logger = lgr
		}
	}
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
func Open(dir string, options ...cacheOption) (*Cache, error) {
	c, err := cache.Open(dir)
	if err != nil {
		return nil, err
	}
	C := &Cache{c: c, now: time.Now, dir: dir,
		maxSize:      DefaultMaxSize,
		trimInterval: DefaultTrimInterval,
		trimLimit:    DefaultTrimLimit,
		trimSize:     DefaultTrimSize,
		logger:       slog.Default().With("lib", "filecache"),
	}
	for _, o := range options {
		o(C)
	}
	return C, nil
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

// Trim removes old cache entries that are likely not to be reused.
func (C *Cache) Trim() error {
	C.mu.Lock()
	defer C.mu.Unlock()
	return C.trim()
}

func (C *Cache) trim() error {
	now := C.now()
	if !C.lastTrim.IsZero() && now.Sub(C.lastTrim) < C.trimInterval {
		C.logger.Debug("skip trim", slog.Time("lastTrim", C.lastTrim), slog.String("trimInterval", C.trimInterval.String()))
		return nil
	}

	trimFn := filepath.Join(C.dir, "trim.txt")
	// We maintain in dir/trim.txt the time of the last completed cache trim.
	// If the cache has been trimmed recently enough, do nothing.
	// This is the common case.
	// If the trim file is corrupt, detected if the file can't be parsed, or the
	// trim time is too far in the future, attempt the trim anyway. It's possible that
	// the cache was full when the corruption happened. Attempting a trim on
	// an empty cache is cheap, so there wouldn't be a big performance hit in that case.
	if C.trimInterval > 0 {
		if data, err := lockedfile.Read(trimFn); err == nil {
			if t, err := strconv.ParseInt(string(bytes.TrimSpace(data)), 10, 64); err == nil {
				C.lastTrim = time.Unix(t, 0)
				if now.Sub(C.lastTrim) < C.trimInterval {
					C.logger.Debug("skip trim", slog.Time("lastTrim", C.lastTrim), slog.String("trimInterval", C.trimInterval.String()))
					return nil
				}
			}
		}
	}

	// Trim each of the 256 subdirectories.
	cutoffTime := now.Add(-C.trimLimit)
	cutoffSize := C.trimSize
	sizeCutoffTime := now.Add(-C.trimInterval)
	var size int64
	for i := 0; i < 256; i++ {
		subdir := filepath.Join(C.dir, fmt.Sprintf("%02x", i))
		size += C.trimSubdir(subdir, cutoffTime, cutoffSize, sizeCutoffTime)
	}
	C.logger.Warn("trim", "size", size, "maxSize", C.maxSize)
	if C.maxSize > 0 && size > C.maxSize {
		C.logger.Warn("truncate cache", "maxSize", C.maxSize, "size", size)
		for i := 0; i < 256; i++ {
			subdir := filepath.Join(C.dir, fmt.Sprintf("%02x", i))
			dis, _ := os.ReadDir(subdir)
			for _, di := range dis {
				// Remove only cache entries (xxxx-a and xxxx-d).
				if fi, err := di.Info(); err == nil {
					size -= fi.Size()
					if size <= C.maxSize/2 {
						break
					}
				}
				if name := di.Name(); len(name) > 2 {
					switch name[len(name)-2:] {
					case "-a", "-d":
						os.Remove(filepath.Join(subdir, di.Name()))
					}
				}
			}
		}
	}
	C.lastTrim = now

	// Ignore errors from here: if we don't write the complete timestamp, the
	// cache will appear older than it is, and we'll trim it again next time.
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d", now.Unix())
	if err := lockedfile.Write(trimFn, &b, 0666); err != nil {
		C.logger.Error("write", slog.String("file", trimFn), slog.Any("error", err))
		return err
	}

	return nil
}

// trimSubdir trims a single cache subdirectory.
func (C *Cache) trimSubdir(subdir string, cutoffTime time.Time, cutoffSize int64, sizeCutoffTime time.Time) int64 {
	// Read all directory entries from subdir before removing
	// any files, in case removing files invalidates the file offset
	// in the directory scan. Also, ignore error from f.Readdirnames,
	// because we don't care about reporting the error and we still
	// want to process any entries found before the error.
	f, err := os.Open(subdir)
	if err != nil {
		if !os.IsNotExist(err) {
			C.logger.Warn("Open", "subdir", subdir)
		}
		return 0
	}
	defer f.Close()
	var size int64
	for {
		dis, _ := f.ReadDir(128)
		if len(dis) == 0 {
			break
		}

		for _, di := range dis {
			// C.logger.Info("list", "entry", di.Name())
			name := di.Name()
			// Remove only cache entries (xxxx-a and xxxx-d).
			if !strings.HasSuffix(name, "-a") && !strings.HasSuffix(name, "-d") {
				continue
			}
			entry := filepath.Join(subdir, name)
			info, err := os.Stat(entry)
			if err != nil {
				C.logger.Warn("stat", "entry", entry, "error", err)
				continue
			}
			if info.ModTime().Before(cutoffTime) ||
				(cutoffSize > 0 && info.Size() > cutoffSize && info.ModTime().Before(sizeCutoffTime)) {
				// C.logger.Info("remove", "entry", entry)
				os.Remove(entry)
			} else {
				// C.logger.Info("keep", "entry", entry, "size", size, "info", info.Size())
				size += info.Size()
			}
		}
	}
	return size
}
