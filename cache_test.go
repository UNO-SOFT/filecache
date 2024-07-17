// Copyright 2024 Tamás Gulácsi. All rights reserved.
//
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
// SPDX-License-Identifier: BSD-3-Clause

package filecache_test

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UNO-SOFT/filecache"
	"github.com/tgulacsi/go/iohlp"
)

func TestPutTrim(t *testing.T) {
	dir, err := os.MkdirTemp("", "filecache-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	const maxSize = 1 << 20
	fakeNow := time.Now
	now := func() time.Time { return fakeNow() }
	c, err := filecache.Open(dir,
		filecache.WithTrimInterval(15*time.Minute),
		filecache.WithTrimLimit(24*time.Hour),
		filecache.WithTrimSize(1<<20),
		filecache.WithMaxSize(maxSize),
		filecache.WithNow(now),
	)
	if err != nil {
		t.Fatal(err)
	}

	putGet := func(i, j int) (filecache.ActionID, string, filecache.OutputID) {
		id := filecache.NewActionID([]byte(fmt.Sprintf("%018d", i)))
		start := fmt.Sprintf("abcdefgh-%018d\n", i)
		r := io.ReadSeeker(strings.NewReader(start))
		if j > len(start) {
			fh, err := os.Open("/dev/zero")
			if err != nil {
				t.Skipf("/dev/zero: %+v", err)
			} else {
				defer fh.Close()
				if r, err = iohlp.MakeSectionReader(io.MultiReader(
					r,
					io.LimitReader(fh, int64(j-len(start)))),
					1<<20,
				); err != nil {
					t.Skipf("MakeSectionReader: %+v", err)
				}
			}
		}
		outID, n, err := c.Put(id, r)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Put %02x [%d]", outID, n)

		fn, _, err := c.GetFile(id)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(fn)
		return id, fn, outID
	}

	_, fn1, _ := putGet(1, 0)

	fakeNow = func() time.Time { return time.Now().Add(2 * 24 * time.Hour) }
	_, fn2, _ := putGet(2, 0)

	if fi, err := os.Stat(fn1); err == nil {
		t.Fatalf("old file still exists: %q (%v)", fn1, fi.ModTime())
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %q: %+v", fn1, err)
	}

	if _, err := os.Stat(fn2); err != nil {
		t.Fatalf("new file: %+v", err)
	}

	putGet(3, maxSize+1)
	fakeNow = func() time.Time { return time.Now().Add(5 * 24 * time.Hour) }
	putGet(4, 0)
	// c.Trim()
	names := make([]string, 0, 3)
	if err = filepath.WalkDir(dir, func(path string, di fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if di.Type().IsRegular() {
			if name := di.Name(); strings.HasSuffix(name, "-a") || strings.HasSuffix(name, "-d") {
				names = append(names, name)
				if len(names) == cap(names) {
					return fmt.Errorf("too many files remained: %q", names)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
