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

// dropInode removes the attributes of an inode behind the client's back,
// leaving whatever named it dangling -- the way a wrongly-swept leaked inode
// or other metadata corruption would.
func dropInode(t *testing.T, m Meta, ino Ino) {
	var err error
	switch c := m.(type) {
	case *redisMeta:
		err = c.rdb.Del(Background(), c.inodeKey(ino)).Err()
	case *dbMeta:
		_, err = c.db.Delete(&node{Inode: ino})
	case *kvMeta:
		err = c.deleteKeys(c.inodeKey(ino))
	default:
		t.Fatalf("unknown engine %T", m)
	}
	if err != nil {
		t.Fatalf("drop inode %d: %s", ino, err)
	}
}

// TestSnapshotLeakedSweepVsBuild checks that the Redis sweep of leaked inodes
// leaves a snapshot under construction alone, even though its inodes carry the
// old ctime of what they copy and are not yet named under .snapshots.
func TestSnapshotLeakedSweepVsBuild(t *testing.T) {
	forSnapshotClients(t, func(t *testing.T, m Meta) {
		rm, ok := m.(*redisMeta)
		if !ok {
			t.Skip("the sweep under test is specific to Redis")
		}
		ctx := Background()
		dir := snapshotMkdir(t, m, RootInode, "d")
		var inodes []Ino
		for i := 0; i < 10; i++ {
			sub := snapshotMkdir(t, m, dir, fmt.Sprintf("sub%d", i))
			inodes = append(inodes, sub)
			for j := 0; j < 20; j++ {
				inodes = append(inodes, snapshotCreate(t, m, sub, fmt.Sprintf("f%d", j)))
			}
		}
		// age the source tree well past the sweep's one-hour cutoff, as a tree
		// worth snapshotting usually is
		old := time.Now().Add(-48 * time.Hour).Unix()
		for _, ino := range inodes {
			var a Attr
			if st := rm.doGetAttr(ctx, ino, &a); st != 0 {
				t.Fatalf("getattr: %s", st)
			}
			a.Ctime = old
			if err := rm.rdb.Set(ctx, rm.inodeKey(ino), rm.marshal(&a), 0).Err(); err != nil {
				t.Fatal(err)
			}
		}
		for round := 0; round < 5; round++ {
			name := fmt.Sprintf("s%d", round)
			stop := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						rm.cleanupLeakedInodes(true)
					}
				}
			}()
			root, st := m.CreateSnapshot(ctx, dir, name, false, nil, nil)
			close(stop)
			wg.Wait()
			if st != 0 {
				t.Fatalf("round %d: snapshot: %s", round, st)
			}
			var broken int
			var walk func(Ino)
			walk = func(ino Ino) {
				var es []*Entry
				if st := rm.doReaddir(ctx, ino, 0, &es, -1); st != 0 {
					t.Fatalf("readdir %d: %s", ino, st)
				}
				for _, e := range es {
					var a Attr
					if st := rm.doGetAttr(ctx, e.Inode, &a); st != 0 {
						broken++
						continue
					}
					if a.Typ == TypeDirectory {
						walk(e.Inode)
					}
				}
			}
			walk(root)
			if broken > 0 {
				t.Fatalf("round %d: the concurrent sweep deleted %d inodes of snapshot %s while it was built", round, broken, name)
			}
		}
	})
}

// TestSnapshotDanglingEntry checks that removing a tree with an entry whose
// inode is already gone -- the state a wrongly-swept leaked inode leaves
// behind -- fails instead of retrying forever.
func TestSnapshotDanglingEntry(t *testing.T) {
	forSnapshotClients(t, func(t *testing.T, m Meta) {
		ctx := Background()
		dir := snapshotMkdir(t, m, RootInode, "d")
		sub := snapshotMkdir(t, m, dir, "sub")
		snapshotCreate(t, m, sub, "f")
		root, st := m.CreateSnapshot(ctx, dir, "s", false, nil, nil)
		if st != 0 {
			t.Fatalf("snapshot: %s", st)
		}
		var ssub Ino
		if st := m.Lookup(ctx, root, "sub", &ssub, &Attr{}, false); st != 0 {
			t.Fatalf("lookup: %s", st)
		}
		dropInode(t, m, ssub)
		done := make(chan syscall.Errno, 1)
		go func() { done <- m.DeleteSnapshot(ctx, "s", nil) }()
		select {
		case st := <-done:
			if st == 0 {
				t.Fatalf("deleting a snapshot with a dangling entry succeeded")
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("deleting a snapshot with a dangling entry did not return")
		}
	})
}
