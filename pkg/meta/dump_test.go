/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package meta

import (
	"io"
	"path"
	"runtime"
	"strings"
	"testing"
	"time"
)

func settleGoroutines() {
	runtime.GC()
	time.Sleep(300 * time.Millisecond)
	runtime.GC()
}

func testDumpMetaNoGoroutineLeakOnFailure(t *testing.T, setup func(t *testing.T) Meta, breakMeta func(t *testing.T, m Meta)) {
	t.Helper()
	m := setup(t)
	t.Cleanup(func() { _ = m.Shutdown() })
	breakMeta(t, m)

	settleGoroutines()
	base := runtime.NumGoroutine()

	const cycles = 50
	for i := 0; i < cycles; i++ {
		if err := m.DumpMeta(io.Discard, RootInode, 1, false, true, true); err == nil {
			t.Fatal("expected DumpMeta to fail")
		}
	}

	settleGoroutines()
	if leaked := runtime.NumGoroutine() - base; leaked > cycles/10 {
		t.Fatalf("leaked %d goroutines after %d failed DumpMeta calls", leaked, cycles)
	}
}

func TestSQLDumpMetaNoGoroutineLeakOnFailure(t *testing.T) {
	testDumpMetaNoGoroutineLeakOnFailure(t,
		func(t *testing.T) Meta {
			t.Helper()
			metaClient, err := newSQLMeta("sqlite3", path.Join(t.TempDir(), "dump-leak.db"), testConfig())
			if err != nil {
				t.Fatalf("create meta: %s", err)
			}
			if err := metaClient.Reset(); err != nil {
				t.Fatalf("reset meta: %s", err)
			}
			if err := metaClient.Init(testFormat(), true); err != nil {
				t.Fatalf("init meta: %s", err)
			}
			return metaClient
		},
		func(t *testing.T, m Meta) {
			t.Helper()
			dm := m.(*dbMeta)
			if _, err := dm.db.Exec("DROP TABLE " + dm.tablePrefix + "node"); err != nil {
				t.Fatalf("drop node table: %s", err)
			}
		},
	)
}

// The truncated stream fails inside loadEntries after the load workers and
// the progress bar are created, so both must be cleaned up on the error path.
func testLoadMetaNoGoroutineLeakOnFailure(t *testing.T, setup func(t *testing.T) Meta) {
	t.Helper()
	m := setup(t)
	t.Cleanup(func() { _ = m.Shutdown() })

	settleGoroutines()
	base := runtime.NumGoroutine()

	const cycles = 50
	for i := 0; i < cycles; i++ {
		if err := m.LoadMeta(strings.NewReader(`{"FSTree":`)); err == nil {
			t.Fatal("expected LoadMeta to fail")
		}
	}

	settleGoroutines()
	if leaked := runtime.NumGoroutine() - base; leaked > cycles/10 {
		t.Fatalf("leaked %d goroutines after %d failed LoadMeta calls", leaked, cycles)
	}
}

func TestSQLLoadMetaNoGoroutineLeakOnFailure(t *testing.T) {
	testLoadMetaNoGoroutineLeakOnFailure(t, func(t *testing.T) Meta {
		t.Helper()
		metaClient, err := newSQLMeta("sqlite3", path.Join(t.TempDir(), "load-leak.db"), testConfig())
		if err != nil {
			t.Fatalf("create meta: %s", err)
		}
		if err := metaClient.Reset(); err != nil {
			t.Fatalf("reset meta: %s", err)
		}
		return metaClient
	})
}

func TestMemKVLoadMetaNoGoroutineLeakOnFailure(t *testing.T) {
	testLoadMetaNoGoroutineLeakOnFailure(t, func(t *testing.T) Meta {
		t.Helper()
		m, err := newKVMeta("memkv", "jfs-load-leak", testConfig())
		if err != nil {
			t.Fatalf("create meta: %s", err)
		}
		if err := m.Reset(); err != nil {
			t.Fatalf("reset meta: %s", err)
		}
		return m
	})
}

type failFastDumpScan struct {
	tkvClient
}

func (c *failFastDumpScan) scan(prefix []byte, handler func(key, value []byte) bool) error {
	// DumpMeta scans deleted files with a non-nil prefix before creating progress,
	// then fast dump scans the whole keyspace with prefix=nil after progress is ready.
	if prefix != nil {
		return c.tkvClient.scan(prefix, handler)
	}
	return io.ErrClosedPipe
}

func TestMemKVDumpMetaNoGoroutineLeakOnFailure(t *testing.T) {
	testDumpMetaNoGoroutineLeakOnFailure(t,
		func(t *testing.T) Meta {
			t.Helper()
			m, err := newKVMeta("memkv", "jfs-dump-leak", testConfig())
			if err != nil {
				t.Fatalf("create meta: %s", err)
			}
			if err := m.Reset(); err != nil {
				t.Fatalf("reset meta: %s", err)
			}
			if err := m.Init(testFormat(), true); err != nil {
				t.Fatalf("init meta: %s", err)
			}
			return m
		},
		func(t *testing.T, m Meta) {
			t.Helper()
			km := m.(*kvMeta)
			km.client = &failFastDumpScan{tkvClient: km.client}
		},
	)
}
