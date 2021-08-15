// Copyright 2020, 2021 Tamás Gulácsi.
//
// SPDX-License-Identifier: Apache-2.0

package filecache_test

import (
	"context"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/UNO-SOFT/filecache"
)

func TestCache(t *testing.T) {
	dn, err := ioutil.TempDir("", "filecache-")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("KEEP") != "1" {
		defer os.RemoveAll(dn)
	}
	pc, err := filecache.New(dn, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	pc.Logger = testLogger{TB: t}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	putGet := func(k, v string) {
		var rc io.ReadCloser
		for i := 0; i < 100; i++ {
			if err := pc.Put(ctx, k, strings.NewReader(v)); err != nil {
				t.Errorf("Put %q: %+v", k, err)
				return
			}
			var err error
			if rc, err = pc.Get(ctx, k); err == nil {
				break
			}
			t.Logf("Get(%d). %q: %+v", i, k, err)
		}
		if rc == nil {
			return
		}
		b, err := ioutil.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Errorf("Read %q: %+v", k, err)
			return
		}
		if string(b) != v {
			t.Errorf("%q: got %q, wanted %q", k, string(b), v)
		}
	}

	for k, v := range map[string]string{
		"a": "árvíztűrő tükörfúrógép",
		"b": "nil",
	} {
		putGet(k, v)
	}

	var a [4]byte
	var b []byte
	for i := 0; i < 1000; i++ {
		_, _ = rand.Read(a[:])
		b = append(b, a[:]...)
		putGet("a"+strconv.Itoa(i), string(b))
	}
}

type testLogger struct{ testing.TB }

func (tl testLogger) Log(keyvals ...interface{}) error {
	tl.TB.Helper()
	tl.TB.Log(keyvals...)
	return nil
}
