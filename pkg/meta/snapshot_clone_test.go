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
	"context"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// cloneTestEngines holds one engine of each family; the redis one needs a
// server on 127.0.0.1:6392.
var cloneTestEngines = []string{"sqlite3", "memkv", "badger", "redis"}

// newCloneTestMeta opens a fresh volume on the engine. A wrap is installed
// around the engine before the session starts, so no goroutine of the session
// sees the engine change.
func newCloneTestMeta(t *testing.T, kind string, wrap func(engine) engine) Meta {
	t.Helper()
	var m Meta
	var err error
	switch kind {
	case "redis":
		m, err = newRedisMeta("redis", "127.0.0.1:6392/1", testConfig())
	case "sqlite3":
		m, err = newSQLMeta("sqlite3", path.Join(t.TempDir(), "clone.db"), testConfig())
	case "memkv":
		m, err = newKVMeta("memkv", "clone-"+t.Name(), testConfig())
	case "badger":
		m, err = newKVMeta("badger", t.TempDir(), testConfig())
	default:
		t.Fatalf("unknown engine %s", kind)
	}
	if err != nil {
		t.Fatalf("create meta: %s", err)
	}
	if wrap != nil {
		base := m.getBase()
		base.en = wrap(base.en)
	}
	if err := m.Reset(); err != nil {
		t.Fatalf("reset: %s", err)
	}
	if err := m.Init(testFormat(), true); err != nil {
		t.Fatalf("init: %s", err)
	}
	if _, err := m.Load(true); err != nil {
		t.Fatalf("load: %s", err)
	}
	if err := m.NewSession(true); err != nil {
		t.Fatalf("session: %s", err)
	}
	t.Cleanup(func() { _ = m.CloseSession() })
	return m
}

// sliceRefCount returns the stored reference count of a slice: the number of
// references beyond the one of its writer.
func sliceRefCount(t *testing.T, m Meta, id uint64, size uint32) int64 {
	t.Helper()
	switch mm := m.(type) {
	case *redisMeta:
		v, err := mm.rdb.HGet(context.Background(), mm.sliceRefs(), mm.sliceKey(id, size)).Int64()
		if err != nil && err != redis.Nil {
			t.Fatal(err)
		}
		return v
	case *dbMeta:
		r := sliceRef{Id: id}
		if _, err := mm.db.Get(&r); err != nil {
			t.Fatal(err)
		}
		return int64(r.Refs)
	case *kvMeta:
		v, err := mm.get(mm.sliceKey(id, size))
		if err != nil {
			t.Fatal(err)
		}
		return parseCounter(v)
	}
	t.Fatalf("unknown meta %T", m)
	return 0
}

func mustClone(t *testing.T, m Meta, srcParent, src, dstParent Ino, name string) {
	t.Helper()
	var count, total uint64
	if st := m.Clone(Background(), srcParent, src, dstParent, name, 0, 022, 1, &count, &total); st != 0 {
		t.Fatalf("clone %d as %s: %s", src, name, st)
	}
}

func mustLookup(t *testing.T, m Meta, parent Ino, name string) (Ino, *Attr) {
	t.Helper()
	var ino Ino
	var attr Attr
	if st := m.Lookup(Background(), parent, name, &ino, &attr, false); st != 0 {
		t.Fatalf("lookup %s in %d: %s", name, parent, st)
	}
	return ino, &attr
}

// compactOnRead runs fn the first time the chunk at key is read, standing in
// for a compaction that lands between the read of a chunk and its copy.
type compactOnRead struct {
	key string
	mu  sync.Mutex
	fn  func()
}

func (h *compactOnRead) arm(fn func()) {
	h.mu.Lock()
	h.fn = fn
	h.mu.Unlock()
}

func (h *compactOnRead) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *compactOnRead) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		h.fire([]redis.Cmder{cmd})
		return err
	}
}

func (h *compactOnRead) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		h.fire(cmds)
		return err
	}
}

func (h *compactOnRead) fire(cmds []redis.Cmder) {
	for _, c := range cmds {
		args := c.Args()
		if len(args) >= 2 && strings.EqualFold(c.Name(), "lrange") && args[1] == h.key {
			h.mu.Lock()
			fn := h.fn
			h.fn = nil
			h.mu.Unlock()
			if fn != nil {
				fn()
			}
			return
		}
	}
}

// A compaction that lands while a file is copied must not free the slices the
// copy then holds: the copy has to start over on the compacted chunk.
func TestCloneCompactRace(t *testing.T) {
	m := newCloneTestMeta(t, "redis", nil)
	rm := m.(*redisMeta)
	ctx := Background()
	var mu sync.Mutex
	deleted := make(map[uint64]bool)
	m.OnMsg(DeleteSlice, func(args ...any) error {
		mu.Lock()
		deleted[args[0].(uint64)] = true
		mu.Unlock()
		return nil
	})
	var dir, f Ino
	if st := m.Mkdir(ctx, RootInode, "cr", 0755, 022, 0, &dir, &Attr{}); st != 0 {
		t.Fatalf("mkdir: %s", st)
	}
	if st := m.Create(ctx, dir, "f", 0644, 022, 0, &f, &Attr{}); st != 0 {
		t.Fatalf("create: %s", st)
	}
	hook := &compactOnRead{key: rm.chunkKey(f, 0)}
	rm.rdb.(*redis.Client).AddHook(hook)

	// write three slices and arm a compaction of them
	prepare := func() []uint64 {
		var ids []uint64
		for i := range 3 {
			var id uint64
			if st := m.NewSlice(ctx, &id); st != 0 {
				t.Fatalf("new slice: %s", st)
			}
			ids = append(ids, id)
			if st := m.Write(ctx, f, 0, uint32(i*1000), Slice{Id: id, Size: 1000, Len: 1000}, time.Now()); st != 0 {
				t.Fatalf("write: %s", st)
			}
		}
		hook.arm(func() {
			vals, err := rm.rdb.LRange(context.Background(), hook.key, 0, -1).Result()
			if err != nil {
				t.Errorf("lrange: %s", err)
				return
			}
			var nid uint64
			if st := m.NewSlice(ctx, &nid); st != 0 {
				t.Errorf("new slice: %s", st)
				return
			}
			if st := rm.doCompactChunk(f, 0, []byte(strings.Join(vals, "")), readSlices(vals), 0, 0, nid, 3000, nil); st != 0 {
				t.Errorf("compact: %s", st)
			}
		})
		return ids
	}
	check := func(copy Ino, ids []uint64, what string) {
		var ss []Slice
		if st := m.Read(ctx, copy, 0, &ss); st != 0 {
			t.Fatalf("read %s: %s", what, st)
		}
		// the compaction frees the slices it replaced; wait for that to land
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			mu.Lock()
			n := 0
			for _, id := range ids {
				if deleted[id] {
					n++
				}
			}
			mu.Unlock()
			if n == len(ids) {
				break
			}
		}
		mu.Lock()
		defer mu.Unlock()
		for _, s := range ss {
			if deleted[s.Id] {
				t.Errorf("%s holds slice %d, which the compaction deleted (refs now %d)", what, s.Id, sliceRefCount(t, m, s.Id, s.Size))
			}
		}
	}

	ids := prepare()
	mustClone(t, m, dir, f, dir, "g")
	g, _ := mustLookup(t, m, dir, "g")
	check(g, ids, "the copied file")

	ids = prepare()
	mustClone(t, m, RootInode, dir, RootInode, "cr2")
	dir2, _ := mustLookup(t, m, RootInode, "cr2")
	f2, _ := mustLookup(t, m, dir2, "f")
	check(f2, ids, "the file in the copied directory")
}

// A slice that a file holds twice is referenced twice more by a copy of the
// file, whether it is copied on its own or as part of a directory.
func TestCloneDupSliceRefs(t *testing.T) {
	for _, kind := range cloneTestEngines {
		t.Run(kind, func(t *testing.T) {
			m := newCloneTestMeta(t, kind, nil)
			ctx := Background()
			var dir, out, f Ino
			if st := m.Mkdir(ctx, RootInode, "d", 0755, 022, 0, &dir, &Attr{}); st != 0 {
				t.Fatalf("mkdir: %s", st)
			}
			if st := m.Mkdir(ctx, RootInode, "out", 0755, 022, 0, &out, &Attr{}); st != 0 {
				t.Fatalf("mkdir: %s", st)
			}
			if st := m.Create(ctx, dir, "f", 0644, 022, 0, &f, &Attr{}); st != 0 {
				t.Fatalf("create: %s", st)
			}
			var id uint64
			if st := m.NewSlice(ctx, &id); st != 0 {
				t.Fatalf("new slice: %s", st)
			}
			if st := m.Write(ctx, f, 0, 0, Slice{Id: id, Size: 1000, Len: 1000}, time.Now()); st != 0 {
				t.Fatalf("write: %s", st)
			}
			var copied uint64
			if st := m.CopyFileRange(ctx, f, 0, f, ChunkSize, 1000, 0, &copied, nil); st != 0 {
				t.Fatalf("copy file range: %s", st)
			}
			before := sliceRefCount(t, m, id, 1000)

			mustClone(t, m, dir, f, out, "g")
			if got := sliceRefCount(t, m, id, 1000); got-before != 2 {
				t.Fatalf("slice %d appears twice in the copied file but its refs moved %d -> %d", id, before, got)
			}
			mustClone(t, m, RootInode, dir, RootInode, "d2")
			if got := sliceRefCount(t, m, id, 1000); got-before != 4 {
				t.Fatalf("slice %d appears twice in the copied directory but its refs moved %d -> %d", id, before+2, got)
			}
		})
	}
}
