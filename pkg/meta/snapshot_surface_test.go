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
	"syscall"
	"testing"
)

// forEachSurfaceMeta runs f against a fresh volume on every engine that needs
// no external service: one per metadata family that has one.
func forEachSurfaceMeta(t *testing.T, f func(t *testing.T, m Meta)) {
	engines := []struct {
		name string
		open func(t *testing.T) (Meta, error)
	}{
		{"sqlite3", func(t *testing.T) (Meta, error) {
			return newSQLMeta("sqlite3", path.Join(t.TempDir(), "surface.db"), testConfig())
		}},
		{"memkv", func(t *testing.T) (Meta, error) {
			return newKVMeta("memkv", "jfs-surface-test", testConfig())
		}},
		{"badger", func(t *testing.T) (Meta, error) {
			return newKVMeta("badger", t.TempDir(), testConfig())
		}},
	}
	for _, e := range engines {
		t.Run(e.name, func(t *testing.T) {
			m, err := e.open(t)
			if err != nil {
				t.Fatalf("create meta: %s", err)
			}
			if err := m.Reset(); err != nil {
				t.Fatalf("reset meta: %s", err)
			}
			if err := m.Init(testFormat(), true); err != nil {
				t.Fatalf("init meta: %s", err)
			}
			m.OnMsg(DeleteSlice, func(args ...any) error { return nil })
			f(t, m)
			_ = m.Shutdown()
		})
	}
}

// withSnapshotName runs f with the process-wide snapshot name set to name, as a
// mount with --prefix-internal does, and restores it afterwards.
func withSnapshotName(t *testing.T, name string, f func()) {
	saved := SnapshotName
	SnapshotName = name
	defer func() { SnapshotName = saved }()
	f()
}

// The hidden roots are reserved under both their plain and their .jfs-prefixed
// names, whatever this process calls them: --prefix-internal renames them per
// mount, and an entry made under the other spelling would collide on the mounts
// using it.
func TestSnapshotReservedSpellings(t *testing.T) {
	forEachSurfaceMeta(t, func(t *testing.T, m Meta) {
		ctx := Background()
		var ino, dir, file Ino
		if st := m.Mkdir(ctx, RootInode, "dir", 0755, 0, 0, &dir, nil); st != 0 {
			t.Fatalf("mkdir dir: %s", st)
		}
		if st := m.Create(ctx, RootInode, "file", 0644, 0, 0, &file, nil); st != 0 {
			t.Fatalf("create file: %s", st)
		}
		check := func(name string) {
			t.Helper()
			if st := m.Mkdir(ctx, RootInode, name, 0755, 0, 0, &ino, nil); st != syscall.EPERM {
				t.Fatalf("mkdir /%s (snapshot name %s): %s, want EPERM", name, SnapshotName, st)
			}
			if st := m.Create(ctx, RootInode, name, 0644, 0, 0, &ino, nil); st != syscall.EPERM {
				t.Fatalf("create /%s (snapshot name %s): %s, want EPERM", name, SnapshotName, st)
			}
			if st := m.Symlink(ctx, RootInode, name, "dir", &ino, nil); st != syscall.EPERM {
				t.Fatalf("symlink /%s (snapshot name %s): %s, want EPERM", name, SnapshotName, st)
			}
			if st := m.Link(ctx, file, RootInode, name, nil); st != syscall.EPERM {
				t.Fatalf("link /%s (snapshot name %s): %s, want EPERM", name, SnapshotName, st)
			}
			if st := m.Rename(ctx, RootInode, "dir", RootInode, name, 0, &ino, nil); st != syscall.EPERM {
				t.Fatalf("rename dir to /%s (snapshot name %s): %s, want EPERM", name, SnapshotName, st)
			}
			if st := m.Clone(ctx, RootInode, dir, RootInode, name, 0, 0, 1, nil, nil); st != syscall.EPERM {
				t.Fatalf("clone dir to /%s (snapshot name %s): %s, want EPERM", name, SnapshotName, st)
			}
		}
		for _, name := range []string{".snapshots", ".jfs.snapshots", ".trash", ".jfs.trash"} {
			check(name)
			withSnapshotName(t, ".jfs.snapshots", func() { check(name) })
		}
		// only at the root
		if st := m.Mkdir(ctx, dir, ".jfs.snapshots", 0755, 0, 0, &ino, nil); st != 0 {
			t.Fatalf("mkdir dir/.jfs.snapshots: %s", st)
		}
	})
}

// The snapshot root lives under the root of the volume only, so on a mount of a
// subdirectory its name is an ordinary one: it can be created there, and a real
// directory of that name in the subdirectory can be renamed and removed.
func TestSnapshotReservedAtVolumeRootOnly(t *testing.T) {
	forEachSurfaceMeta(t, func(t *testing.T, m Meta) {
		ctx := Background()
		base := m.getBase()
		var sub, src, ino Ino
		if st := m.Mkdir(ctx, RootInode, "sub", 0755, 0, 0, &sub, nil); st != 0 {
			t.Fatalf("mkdir sub: %s", st)
		}
		if st := m.Mkdir(ctx, RootInode, "src", 0755, 0, 0, &src, nil); st != 0 {
			t.Fatalf("mkdir src: %s", st)
		}
		if _, st := m.CreateSnapshot(ctx, src, "s", false, nil, nil); st != 0 {
			t.Fatalf("create snapshot: %s", st)
		}
		if st := m.Mkdir(ctx, RootInode, SnapshotName, 0755, 0, 0, &ino, nil); st != syscall.EPERM {
			t.Fatalf("mkdir /%s: %s, want EPERM", SnapshotName, st)
		}
		if st := m.Rmdir(ctx, RootInode, SnapshotName); st != syscall.EPERM {
			t.Fatalf("rmdir /%s: %s, want EPERM", SnapshotName, st)
		}

		base.chroot(sub)
		defer base.chroot(RootInode)
		if st := m.Mkdir(ctx, RootInode, SnapshotName, 0755, 0, 0, &ino, nil); st != 0 {
			t.Fatalf("mkdir %s at the root of a subdir mount: %s", SnapshotName, st)
		}
		if st := m.Lookup(ctx, RootInode, SnapshotName, &ino, &Attr{}, false); st != 0 || ino == SnapshotInode {
			t.Fatalf("lookup %s at the root of a subdir mount: %s, inode %d", SnapshotName, st, ino)
		}
		if st := m.Rename(ctx, RootInode, SnapshotName, RootInode, "moved", 0, &ino, nil); st != 0 {
			t.Fatalf("rename %s at the root of a subdir mount: %s", SnapshotName, st)
		}
		if st := m.Rename(ctx, RootInode, "moved", RootInode, SnapshotName, 0, &ino, nil); st != 0 {
			t.Fatalf("rename back to %s at the root of a subdir mount: %s", SnapshotName, st)
		}
		if st := m.Create(ctx, RootInode, "f", 0644, 0, 0, &ino, nil); st != 0 {
			t.Fatalf("create f: %s", st)
		}
		if st := m.Unlink(ctx, RootInode, "f"); st != 0 {
			t.Fatalf("unlink f: %s", st)
		}
		if st := m.Rmdir(ctx, RootInode, SnapshotName); st != 0 {
			t.Fatalf("rmdir %s at the root of a subdir mount: %s", SnapshotName, st)
		}
		// the snapshot itself is untouched
		base.chroot(RootInode)
		if st := m.Lookup(ctx, SnapshotInode, "s", &ino, &Attr{}, false); st != 0 {
			t.Fatalf("the snapshot is gone: %s", st)
		}
	})
}
