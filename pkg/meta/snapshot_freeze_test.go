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
	"path"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests here cover the freeze of snapshot inodes across every engine family.
// They run on a private Redis (port 6391), sqlite3, memkv and badger.

const snapshotFreezeRedis = "127.0.0.1:6391/1"

func snapshotFreezeMetas(t *testing.T, conf *Config, format *Format) map[string]Meta {
	if conf == nil {
		conf = testConfig()
	}
	if format == nil {
		format = testFormat()
	}
	ms := map[string]Meta{}
	s, err := newSQLMeta("sqlite3", path.Join(t.TempDir(), "freeze.db"), conf)
	if err != nil {
		t.Fatal(err)
	}
	ms["sqlite3"] = s
	k, err := newKVMeta("memkv", "freeze-"+strings.ReplaceAll(t.Name(), "/", "-"), conf)
	if err != nil {
		t.Fatal(err)
	}
	ms["memkv"] = k
	b, err := newKVMeta("badger", t.TempDir(), conf)
	if err != nil {
		t.Fatal(err)
	}
	ms["badger"] = b
	r, err := newRedisMeta("redis", snapshotFreezeRedis, conf)
	if err != nil {
		t.Fatal(err)
	}
	ms["redis"] = r
	for name, m := range ms {
		if err := m.Reset(); err != nil {
			t.Fatalf("%s reset: %s", name, err)
		}
		if err := m.Init(format, true); err != nil {
			t.Fatalf("%s init: %s", name, err)
		}
		if _, err := m.Load(true); err != nil {
			t.Fatalf("%s load: %s", name, err)
		}
		if err := m.NewSession(true); err != nil {
			t.Fatalf("%s session: %s", name, err)
		}
	}
	return ms
}

// snapshotFreezeTree builds /src/sub/f with data, snapshots /src as s1 and
// returns the snapshot root, its copy of sub and its copy of f.
func snapshotFreezeTree(t *testing.T, m Meta) (Ino, Ino, Ino) {
	ctx := Background()
	var src, sub, file Ino
	if st := m.Mkdir(ctx, RootInode, "src", 0777, 0, 0, &src, &Attr{}); st != 0 {
		t.Fatalf("mkdir src: %s", st)
	}
	if st := m.Mkdir(ctx, src, "sub", 0777, 0, 0, &sub, &Attr{}); st != 0 {
		t.Fatalf("mkdir sub: %s", st)
	}
	if st := m.Create(ctx, sub, "f", 0666, 0, 0, &file, &Attr{}); st != 0 {
		t.Fatalf("create f: %s", st)
	}
	if st := m.Write(ctx, file, 0, 0, Slice{Id: 300001, Size: 100, Len: 100}, time.Now()); st != 0 {
		t.Fatalf("write f: %s", st)
	}
	root, st := m.CreateSnapshot(ctx, src, "s1", true, nil, nil)
	if st != 0 {
		t.Fatalf("snapshot: %s", st)
	}
	var ssub, sfile Ino
	if st := m.Lookup(ctx, root, "sub", &ssub, &Attr{}, false); st != 0 {
		t.Fatalf("lookup sub: %s", st)
	}
	if st := m.Lookup(ctx, ssub, "f", &sfile, &Attr{}, false); st != 0 {
		t.Fatalf("lookup f: %s", st)
	}
	return root, ssub, sfile
}

// rawAttr reads an inode straight from the engine, past any open file cache.
func rawAttr(t *testing.T, m Meta, ino Ino) Attr {
	var attr Attr
	if st := m.getBase().en.doGetAttr(Background(), ino, &attr); st != 0 {
		t.Fatalf("getattr %d: %s", ino, st)
	}
	return attr
}

// Write is refused on a snapshot file and on any immutable file, whether or not
// the caller went through Open.
func TestSnapshotWriteRefused(t *testing.T) {
	for name, m := range snapshotFreezeMetas(t, nil, nil) {
		ctx := Background()
		_, _, sfile := snapshotFreezeTree(t, m)
		before := rawAttr(t, m, sfile)
		if st := m.Write(ctx, sfile, 0, 0, Slice{Id: 300002, Size: 4096, Len: 4096}, time.Now()); st != syscall.EPERM {
			t.Errorf("%s: write to a snapshot file should be EPERM, got %v", name, st)
		}
		if after := rawAttr(t, m, sfile); after != before {
			t.Errorf("%s: a refused write changed the snapshot file: %+v -> %+v", name, before, after)
		}

		var live Ino
		if st := m.Create(ctx, RootInode, "live", 0666, 0, 0, &live, &Attr{}); st != 0 {
			t.Fatalf("%s: create: %s", name, st)
		}
		if st := m.SetAttr(ctx, live, SetAttrFlag, 0, &Attr{Flags: FlagImmutable}); st != 0 {
			t.Fatalf("%s: chattr +i: %s", name, st)
		}
		if st := m.Write(ctx, live, 0, 0, Slice{Id: 300003, Size: 100, Len: 100}, time.Now()); st != syscall.EPERM {
			t.Errorf("%s: write to an immutable file should be EPERM, got %v", name, st)
		}
		if st := m.SetAttr(ctx, live, SetAttrFlag, 0, &Attr{Flags: FlagAppend}); st != 0 {
			t.Fatalf("%s: chattr -i +a: %s", name, st)
		}
		if st := m.Write(ctx, live, 0, 0, Slice{Id: 300004, Size: 100, Len: 100}, time.Now()); st != 0 {
			t.Errorf("%s: write to an append-only file: %v", name, st)
		}
	}
}

// With the open cache on, a second open must still honor the freeze and the
// permission bits, both of which the first open already checked.
func TestSnapshotOpenCache(t *testing.T) {
	conf := testConfig()
	conf.OpenCache = time.Hour
	for name, m := range snapshotFreezeMetas(t, conf, nil) {
		_, _, sfile := snapshotFreezeTree(t, m)
		user := NewContext(1, 1000, []uint32{1000})
		var attr Attr
		if st := m.Open(user, sfile, syscall.O_RDONLY, &attr); st != 0 {
			t.Fatalf("%s: open snapshot file read-only: %s", name, st)
		}
		for _, ctx := range []Context{user, Background()} {
			if st := m.Open(ctx, sfile, syscall.O_RDWR, &attr); st != syscall.EPERM {
				t.Errorf("%s: cached open of a snapshot file for writing by uid %d should be EPERM, got %v", name, ctx.Uid(), st)
			}
		}
		// a refused open holds no reference
		if !m.getBase().of.Close(sfile) {
			t.Errorf("%s: refused opens left references on the snapshot file", name)
		}

		var private Ino
		if st := m.Create(user, RootInode, "private", 0600, 0, 0, &private, &Attr{}); st != 0 {
			t.Fatalf("%s: create: %s", name, st)
		}
		if st := m.Open(user, private, syscall.O_RDWR, &attr); st != 0 {
			t.Fatalf("%s: open own file: %s", name, st)
		}
		other := NewContext(2, 1001, []uint32{1001})
		if st := m.Open(other, private, syscall.O_RDONLY, &attr); st != syscall.EACCES {
			t.Errorf("%s: cached open of a 0600 file by another user should be EACCES, got %v", name, st)
		}
		if st := m.Open(user, private, syscall.O_RDWR, &attr); st != 0 {
			t.Errorf("%s: second open of own file: %v", name, st)
		}
	}
}

// Only CreateSnapshot may build frozen trees: Clone refuses the snapshot mode.
func TestSnapshotCloneMode(t *testing.T) {
	for name, m := range snapshotFreezeMetas(t, nil, nil) {
		ctx := Background()
		var dir Ino
		if st := m.Mkdir(ctx, RootInode, "d", 0777, 0, 0, &dir, &Attr{}); st != 0 {
			t.Fatalf("%s: mkdir: %s", name, st)
		}
		if st := m.Create(ctx, dir, "f", 0644, 0, 0, new(Ino), &Attr{}); st != 0 {
			t.Fatalf("%s: create: %s", name, st)
		}
		user := NewContext(2, 1000, []uint32{1000})
		for _, c := range []Context{user, ctx} {
			st := m.Clone(c, RootInode, dir, RootInode, "frozen", CLONE_MODE_SNAPSHOT, 022, 1, new(uint64), new(uint64))
			if st != syscall.EINVAL {
				t.Errorf("%s: clone with the snapshot mode by uid %d should be EINVAL, got %v", name, c.Uid(), st)
			}
			if st := m.Lookup(ctx, RootInode, "frozen", new(Ino), &Attr{}, false); st != syscall.ENOENT {
				t.Errorf("%s: a refused clone left an entry behind: %v", name, st)
			}
		}
		if st := m.Clone(ctx, RootInode, dir, RootInode, "copy", CLONE_MODE_PRESERVE_ATTR, 022, 1, new(uint64), new(uint64)); st != 0 {
			t.Errorf("%s: plain clone: %v", name, st)
		}
		// snapshots are still frozen through the internal path
		_, ssub, sfile := snapshotFreezeTree(t, m)
		for _, ino := range []Ino{ssub, sfile} {
			if got := rawAttr(t, m, ino).Flags; got&(FlagSnapshot|FlagImmutable) != FlagSnapshot|FlagImmutable {
				t.Errorf("%s: snapshot inode %d is not frozen: flags %d", name, ino, got)
			}
		}
	}
}
