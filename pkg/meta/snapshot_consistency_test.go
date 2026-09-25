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
	"fmt"
	"os"
	"path"
	"sync"
	"syscall"
	"testing"
	"time"
)

// snapshotTestRedis is the Redis server the snapshot consistency tests use, a
// database of their own; JFS_SNAPSHOT_REDIS overrides it.
const snapshotTestRedis = "127.0.0.1:6393/1"

// snapshotTestClients returns a constructor of a fresh client per engine
// family, or nil for one not available here.
func snapshotTestClients(t *testing.T) map[string]func() Meta {
	return map[string]func() Meta{
		"sqlite3": func() Meta {
			m, err := newSQLMeta("sqlite3", path.Join(t.TempDir(), "snapshot.db"), testConfig())
			if err != nil {
				t.Fatal(err)
			}
			return m
		},
		"memkv": func() Meta {
			m, err := newKVMeta("memkv", "snapshot-"+t.Name(), testConfig())
			if err != nil {
				t.Fatal(err)
			}
			return m
		},
		"badger": func() Meta {
			m, err := newKVMeta("badger", t.TempDir(), testConfig())
			if err != nil {
				t.Fatal(err)
			}
			return m
		},
		"redis": func() Meta {
			addr := os.Getenv("JFS_SNAPSHOT_REDIS")
			if addr == "" {
				addr = snapshotTestRedis
			}
			m, err := newRedisMeta("redis", addr, testConfig())
			if err != nil {
				t.Logf("redis %s unavailable: %s", addr, err)
				return nil
			}
			if err = m.Reset(); err != nil {
				t.Logf("redis %s unavailable: %s", addr, err)
				return nil
			}
			return m
		},
	}
}

// forSnapshotClients runs f once per engine family, on a freshly formatted
// volume with a session and no trash.
func forSnapshotClients(t *testing.T, f func(t *testing.T, m Meta)) {
	for name, mk := range snapshotTestClients(t) {
		t.Run(name, func(t *testing.T) {
			m := mk()
			if m == nil {
				t.Skip("engine not available")
			}
			if err := m.Reset(); err != nil {
				t.Fatal(err)
			}
			format := testFormat()
			format.TrashDays = 0
			if err := m.Init(format, true); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Load(true); err != nil {
				t.Fatal(err)
			}
			if err := m.NewSession(true); err != nil {
				t.Fatal(err)
			}
			m.OnMsg(DeleteSlice, func(args ...any) error { return nil })
			m.OnMsg(CompactChunk, func(args ...any) error { return nil })
			defer func() { _ = m.CloseSession() }()
			f(t, m)
		})
	}
}

func snapshotMkdir(t *testing.T, m Meta, parent Ino, name string) Ino {
	var ino Ino
	if st := m.Mkdir(Background(), parent, name, 0755, 022, 0, &ino, &Attr{}); st != 0 {
		t.Fatalf("mkdir %s: %s", name, st)
	}
	return ino
}

func snapshotCreate(t *testing.T, m Meta, parent Ino, name string) Ino {
	var ino Ino
	if st := m.Create(Background(), parent, name, 0644, 022, 0, &ino, &Attr{}); st != 0 {
		t.Fatalf("create %s: %s", name, st)
	}
	return ino
}

// skipMtimeEngine drives a snapshot build through this interleaving on the
// tree src/{sub/z}: right after the copy lists src, a file n is created under
// src and z gets an extended attribute referring to it, so the copy of z (made
// once the wait on sub below releases) carries the attribute, but n itself,
// already listed past by then, never gets copied. Once the comparison reads
// src's attributes, n is removed again. With SkipDirMtime long enough to cover
// both changes, src's mtime and ctime never move, so a naive comparison would
// call the copy consistent even though the source never looked like that: z
// referring to n while n does not exist.
type skipMtimeEngine struct {
	engine
	m        Meta
	src, sub Ino
	z        Ino
	created  chan struct{}
	create   sync.Once
	unlink   sync.Once
}

func (e *skipMtimeEngine) doCloneEntry(ctx Context, srcIno Ino, parent Ino, name string, ino Ino, attr *Attr, cmode uint8, cumask uint16, top bool) syscall.Errno {
	if srcIno == e.sub {
		<-e.created
	}
	return e.engine.doCloneEntry(ctx, srcIno, parent, name, ino, attr, cmode, cumask, top)
}

func (e *skipMtimeEngine) newDirHandler(inode Ino, plus bool, entries []*Entry) DirHandler {
	h := e.engine.newDirHandler(inode, plus, entries)
	if inode != e.src {
		return h
	}
	return &afterListHandler{DirHandler: h, after: func() {
		e.create.Do(func() {
			var ino Ino
			if st := e.m.Mknod(Background(), e.src, "n", TypeFile, 0644, 022, 0, "", &ino, &Attr{}); st != 0 {
				panic(st)
			}
			if st := e.m.SetXattr(Background(), e.z, "user.refers", []byte("n"), 0); st != 0 {
				panic(st)
			}
			close(e.created)
		})
	}}
}

func (e *skipMtimeEngine) doGetAttr(ctx Context, inode Ino, attr *Attr) syscall.Errno {
	if inode == e.src {
		select {
		case <-e.created:
			e.unlink.Do(func() {
				if st := e.m.Unlink(Background(), e.src, "n"); st != 0 {
					panic(st)
				}
			})
		default:
		}
	}
	return e.engine.doGetAttr(ctx, inode, attr)
}

type afterListHandler struct {
	DirHandler
	after func()
}

func (h *afterListHandler) List(ctx Context, offset int) ([]*Entry, syscall.Errno) {
	es, st := h.DirHandler.List(ctx, offset)
	h.after()
	return es, st
}

// TestSnapshotSkipDirMtime checks that a consistent snapshot is never
// published in a state the source never had, when SkipDirMtime lets an entry
// change leave the parent's mtime and ctime alone: the snapshot either fails
// with EBUSY (retried by the caller) or matches the tree.
func TestSnapshotSkipDirMtime(t *testing.T) {
	forSnapshotClients(t, func(t *testing.T, m Meta) {
		ctx := Background()
		base := m.getBase()
		for _, skip := range []time.Duration{100 * time.Millisecond, time.Hour} {
			base.conf.SkipDirMtime = skip
			name := fmt.Sprintf("skip%d", skip)
			src := snapshotMkdir(t, m, RootInode, name)
			sub := snapshotMkdir(t, m, src, "sub")
			z := snapshotCreate(t, m, sub, "z")
			orig := base.en
			base.en = &skipMtimeEngine{engine: orig, m: m, src: src, sub: sub, z: z, created: make(chan struct{})}
			root, st := m.CreateSnapshot(ctx, src, name, false, nil, nil)
			base.en = orig
			if st == syscall.EBUSY {
				continue
			}
			if st != 0 {
				t.Fatalf("snapshot with SkipDirMtime %s: %s", skip, st)
			}
			var ss, sz, sn Ino
			if st := m.Lookup(ctx, root, "sub", &ss, &Attr{}, false); st != 0 {
				t.Fatalf("lookup sub: %s", st)
			}
			if st := m.Lookup(ctx, ss, "z", &sz, &Attr{}, false); st != 0 {
				t.Fatalf("lookup z: %s", st)
			}
			var v []byte
			hasX := m.GetXattr(ctx, sz, "user.refers", &v) == 0
			hasN := m.Lookup(ctx, root, "n", &sn, &Attr{}, false) == 0
			if hasX && !hasN {
				t.Fatalf("SkipDirMtime %s: the snapshot has z referring to n but no n, a state the source never had", skip)
			}
		}

		// a directory last changed long before SkipDirMtime cannot be hiding an
		// unrecorded change, so building from it goes through on the first try
		base.conf.SkipDirMtime = time.Hour
		src := snapshotMkdir(t, m, RootInode, "cold")
		snapshotCreate(t, m, src, "f")
		var attr Attr
		if st := base.en.doGetAttr(ctx, src, &attr); st != 0 {
			t.Fatalf("getattr: %s", st)
		}
		old := time.Now().Add(-72 * time.Hour)
		attr.Mtime, attr.Ctime = old.Unix(), old.Unix()
		if st := base.en.doRepair(ctx, src, &attr, true); st != 0 {
			t.Fatalf("age the directory: %s", st)
		}
		if _, st := m.CreateSnapshot(ctx, src, "cold", false, nil, nil); st != 0 {
			t.Fatalf("snapshot of a directory last changed three days ago: %s", st)
		}
	})
}
