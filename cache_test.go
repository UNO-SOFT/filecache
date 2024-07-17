// Copyright 2024 Tamás Gulácsi. All rights reserved.
//
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
// SPDX-License-Identifier: BSD-3-Clause

package filecache

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPutTrim(t *testing.T) {
	dir, err := os.MkdirTemp("", "filecache-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	c, err := Open(dir, WithTrimInterval(15*time.Minute), WithTrimLimit(24*time.Hour), WithTrimSize(1<<20))
	if err != nil {
		t.Fatal(err)
	}

	putGet := func(i int) (ActionID, string, OutputID) {
		id := NewActionID([]byte(fmt.Sprintf("%018d", i)))
		outID, _, err := c.Put(id, strings.NewReader(fmt.Sprintf("abcdefgh-%018d", i)))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%02x", outID)

		fn, _, err := c.GetFile(id)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(fn)
		return id, fn, outID
	}

	_, fn1, _ := putGet(1)

	c.now = func() time.Time { return time.Now().Add(2 * 24 * time.Hour) }
	_, fn2, _ := putGet(2)

	if fi, err := os.Stat(fn1); err == nil {
		t.Fatalf("old file still exists: %q (%v)", fn1, fi.ModTime())
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %q: %+v", fn1, err)
	}

	if _, err := os.Stat(fn2); err != nil {
		t.Fatalf("new file: %+v", err)
	}
}
