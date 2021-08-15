// Copyright 2020, 2021 Tamás Gulácsi.
//
// SPDX-License-Identifier: Apache-2.0

package filecache

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/dgraph-io/ristretto"
	"github.com/google/renameio"
)

type Hash [HashSize]byte

const (
	DefaultMaxCount = 10_000
	DefaultMaxSize  = 1 << 30
	HashSize        = sha512.Size224
)

// New returns a new file cache, which stores as most maxCount number and maxSize bytes
// of files.
func New(root string, maxCount, maxSize int64) (*FileCache, error) {
	if maxCount <= 0 {
		maxCount = DefaultMaxCount
	}
	if maxSize <= 0 {
		maxSize = DefaultMaxSize
	}
	pc := &FileCache{
		root:    root,
		tempDir: renameio.TempDir(root),
	}
	var err error
	// The cache stores the file name -> file path map.
	if pc.cache, err = ristretto.NewCache(&ristretto.Config{
		NumCounters: maxCount * 10,
		MaxCost:     maxSize,
		BufferItems: 64,
		OnEvict: func(item *ristretto.Item) {
			if item == nil || item.Key == 0 || item.Value == nil {
				return
			}
			pc.onCacheEvict(item.Value.(string))
		},
	}); err != nil {
		return nil, err
	}

	// List existing files and populate cache back.
	return pc, filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink == 0 {
			return nil
		}
		b, err := base64.URLEncoding.DecodeString(entry.Name())
		if err != nil {
			return nil
		}
		origFn := string(b)
		fi, err := os.Stat(path)
		if err != nil {
			return nil
		}
		if len(fi.Name()) != HashSize*2 {
			return nil
		}
		var hsh Hash
		if _, err = hex.Decode(hsh[:], []byte(fi.Name())); err != nil {
			return nil
		}
		_ = pc.Log("msg", "set cache from db", "key", origFn, "value", fi.Name())
		pc.cache.Set(origFn, hsh, fi.Size())
		return nil
	})
}

type FileCache struct {
	root, tempDir string
	cache         *ristretto.Cache
	Logger
}
type Logger interface {
	Log(...interface{}) error
}

func (pc *FileCache) Log(keyvals ...interface{}) error {
	if pc.Logger != nil {
		if th, ok := pc.Logger.(interface{ Helper() }); ok {
			th.Helper()
		}
		return pc.Logger.Log(keyvals...)
	}
	return nil
}

func (pc *FileCache) Close() error {
	pc.cache.Close()
	return nil
}
func (pc *FileCache) onCacheEvict(v string) {
	_ = pc.Log("msg", "evict", "value", v)
	_ = os.Remove(v)
	root, bn := filepath.Split(v)
	// Delete the symlinks that point to this
	des, _ := os.ReadDir(root)
	for _, de := range des {
		if de.Type()&os.ModeSymlink == 0 {
			continue
		}
		fn := filepath.Join(root, de.Name())
		if fi, err := os.Stat(fn); err != nil {
			_ = os.Remove(fn)
		} else if bn == fi.Name() || fi.Name() == de.Name() {
			_ = os.Remove(fi.Name())
		}
	}
}

func (pc *FileCache) fileName(hsh Hash) string {
	var a [6 + 2*HashSize]byte
	hex.Encode(a[:2], hsh[0:1])
	a[2] = byte(filepath.Separator)
	hex.Encode(a[3:5], hsh[1:2])
	a[5] = byte(filepath.Separator)
	hex.Encode(a[6:], hsh[:])
	return filepath.Join(pc.root, string(a[:]))
}

func (pc *FileCache) Get(ctx context.Context, nodeID string) (io.ReadCloser, error) {
	v, ok := pc.cache.Get(nodeID)
	if !ok {
		return nil, ErrNotFound
	}
	return os.Open(v.(string))
}

var ErrNotFound = errors.New("not found")

func (pc *FileCache) Put(ctx context.Context, nodeID string, data io.Reader) error {
	tfh, err := os.CreateTemp(pc.tempDir, "")
	if err != nil {
		return err
	}
	defer func() {
		_ = tfh.Close()
		_ = os.Remove(tfh.Name())
	}()
	hsh := sha512.New512_224()
	n, err := io.Copy(io.MultiWriter(tfh, hsh), data)
	if err != nil {
		return err
	}
	var a Hash
	hsh.Sum(a[:0])
	v := pc.fileName(a)
	dn, bn := filepath.Split(v)
	_ = os.MkdirAll(dn, 0750)
	if err = os.Rename(tfh.Name(), v); err != nil {
		return fmt.Errorf("rename %q to %q: %w", tfh.Name(), v, err)
	}
	_ = os.Symlink(bn, filepath.Join(dn, base64.URLEncoding.EncodeToString([]byte(nodeID))))
	pc.cache.Set(nodeID, v, n)
	return nil
}
