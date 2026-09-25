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
	"bytes"
	"encoding/json"
	"io"
	"path"
	"strings"
	"syscall"
	"testing"
	"time"

	"xorm.io/xorm"
)

// compatEngine names one metadata engine the compatibility tests run on, with
// its own store so that they never touch the servers the other suites share.
type compatEngine struct {
	name string
	uri  func(t *testing.T) string
}

func compatEngines() []compatEngine {
	local := func(driver string) func(t *testing.T) string {
		return func(t *testing.T) string { return driver + "://" + path.Join(t.TempDir(), "meta") }
	}
	return []compatEngine{
		{"sqlite3", local("sqlite3")},
		{"memkv", local("memkv")},
		{"badger", local("badger")},
		{"redis", func(*testing.T) string { return "redis://127.0.0.1:6394/1" }},
	}
}

// compatFormat is the test format as `juicefs format` writes it: at metadata
// version 1, the one every client before snapshots formats at and supports.
func compatFormat() *Format {
	f := testFormat()
	f.MetaVersion = 1
	return f
}

// emptyCompatMeta opens an empty, unformatted store on the engine and closes
// it with the test.
func emptyCompatMeta(t *testing.T, uri string) Meta {
	t.Helper()
	m := NewClient(uri, testConfig())
	if err := m.Reset(); err != nil {
		t.Fatalf("reset %s: %s", uri, err)
	}
	t.Cleanup(func() {
		_ = m.Reset()
		_ = m.Shutdown()
	})
	return m
}

// newCompatMeta opens an empty volume on the engine, formatted with
// compatFormat, and closes it with the test.
func newCompatMeta(t *testing.T, uri string) Meta {
	t.Helper()
	m := emptyCompatMeta(t, uri)
	if err := m.Init(compatFormat(), false); err != nil {
		t.Fatalf("init %s: %s", uri, err)
	}
	return m
}

// storedFormat reads the format as the engine holds it, as a JSON object, so
// that a test can see the fields it holds and not only the ones this client
// knows.
func storedFormat(t *testing.T, m Meta) (*Format, map[string]json.RawMessage) {
	t.Helper()
	body, err := m.getBase().en.doLoad()
	if err != nil {
		t.Fatalf("load the stored format: %s", err)
	}
	var f Format
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatalf("parse the stored format: %s", err)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("parse the stored format: %s", err)
	}
	return &f, raw
}

// dumpedSetting returns the format a JSON dump of m records.
func dumpedSetting(t *testing.T, m Meta) *Format {
	t.Helper()
	var buf bytes.Buffer
	if err := m.DumpMeta(&buf, RootInode, 1, true, false, false); err != nil {
		t.Fatalf("dump: %s", err)
	}
	var dm DumpedMeta
	if err := json.Unmarshal(buf.Bytes(), &dm); err != nil {
		t.Fatalf("parse the dump: %s", err)
	}
	return &dm.Setting
}

// A volume is formatted, used and dumped at metadata version 1, as every
// client before snapshots does, until its first snapshot moves it to version
// 2: from then on every client that supports version 1 only refuses it, at
// every command, whatever release it is. The version never goes back.
func TestSnapshotRaisesMetaVersion(t *testing.T) {
	for _, e := range compatEngines() {
		t.Run(e.name, func(t *testing.T) {
			ctx := Background()
			m := newCompatMeta(t, e.uri(t))
			f, raw := storedFormat(t, m)
			if f.MetaVersion != 1 {
				t.Fatalf("a new volume is at meta version %d, want 1", f.MetaVersion)
			}
			if _, ok := raw["MaxSnapshots"]; ok {
				t.Fatalf("a new volume stores a field older clients do not know: %s", raw["MaxSnapshots"])
			}
			var dir Ino
			if st := m.Mkdir(ctx, RootInode, "d", 0755, 0, 0, &dir, nil); st != 0 {
				t.Fatalf("mkdir: %s", st)
			}
			if f := dumpedSetting(t, m); f.MetaVersion != 1 {
				t.Fatalf("a dump of a volume without snapshots is at meta version %d, want 1", f.MetaVersion)
			}
			if f, _ := storedFormat(t, m); f.MetaVersion != 1 {
				t.Fatalf("meta version %d after use, want 1", f.MetaVersion)
			}

			if _, st := m.CreateSnapshot(ctx, dir, "s1", false, nil, nil); st != 0 {
				t.Fatalf("create snapshot: %s", st)
			}
			if f, _ := storedFormat(t, m); f.MetaVersion != 2 {
				t.Fatalf("meta version %d after the first snapshot, want 2", f.MetaVersion)
			}
			if f := dumpedSetting(t, m); f.MetaVersion != 2 {
				t.Fatalf("a dump of a volume with snapshots is at meta version %d, want 2", f.MetaVersion)
			}
			// the versions the clients before snapshots support at most, and this one
			if err := (&Format{MetaVersion: 2}).CheckVersion(); err != nil {
				t.Fatalf("this client refuses meta version 2: %s", err)
			}
			if err := (&Format{MetaVersion: 3}).CheckVersion(); err == nil {
				t.Fatalf("a meta version above the supported ones is accepted")
			}
			if _, err := m.Load(true); err != nil {
				t.Fatalf("load after the first snapshot: %s", err)
			}
			if st := m.DeleteSnapshot(ctx, "s1", nil); st != 0 {
				t.Fatalf("delete snapshot: %s", st)
			}
			if _, st := m.CreateSnapshot(ctx, dir, "s2", false, nil, nil); st != 0 {
				t.Fatalf("create snapshot again: %s", st)
			}
			if f, _ := storedFormat(t, m); f.MetaVersion != 2 {
				t.Fatalf("meta version %d after more snapshots, want 2", f.MetaVersion)
			}
		})
	}
}

// A snapshot is refused while a client that does not support snapshots is
// mounted, which its session tells: a client that supports them records the
// metadata version it supports, so a session without one is from an earlier
// build, whatever its release says, and so is a session without a version.
func TestSnapshotRefusesUnawareSessions(t *testing.T) {
	for _, e := range compatEngines() {
		t.Run(e.name, func(t *testing.T) {
			ctx := Background()
			m := newCompatMeta(t, e.uri(t))
			var dir Ino
			if st := m.Mkdir(ctx, RootInode, "d", 0755, 0, 0, &dir, nil); st != 0 {
				t.Fatalf("mkdir: %s", st)
			}
			b := m.getBase()
			legacySession := func(info string) uint64 {
				sid, err := b.en.incrCounter("nextSession", 1)
				if err != nil {
					t.Fatalf("session id: %s", err)
				}
				b.sid = uint64(sid)
				if err := b.en.doNewSession([]byte(info), false); err != nil {
					t.Fatalf("legacy session: %s", err)
				}
				return uint64(sid)
			}
			for _, info := range []string{
				// a build of the release that gained snapshots, from before it did
				`{"Version":"1.5.0-dev+2026-09-01.9325fe3","HostName":"old","MountPoint":"/jfs","ProcessID":1}`,
				`{"HostName":"unknown"}`,
			} {
				sid := legacySession(info)
				if _, st := m.CreateSnapshot(ctx, dir, "s1", false, nil, nil); st != syscall.EPERM {
					t.Fatalf("snapshot with session %s mounted: %v, want EPERM", info, st)
				}
				if err := b.en.doCleanStaleSession(sid); err != nil {
					t.Fatalf("remove the legacy session: %s", err)
				}
			}
			// an expired session is a client that is gone: one that left on seeing
			// the raised metadata version leaves its session behind
			heartbeat := b.conf.Heartbeat
			b.conf.Heartbeat = time.Millisecond
			sid := legacySession(`{"Version":"1.4.0","HostName":"gone"}`)
			b.conf.Heartbeat = heartbeat
			time.Sleep(1100 * time.Millisecond) // sessions expire at second granularity
			if _, st := m.CreateSnapshot(ctx, dir, "s1", false, nil, nil); st != 0 {
				t.Fatalf("snapshot with an expired legacy session: %s", st)
			}
			if st := m.DeleteSnapshot(ctx, "s1", nil); st != 0 {
				t.Fatalf("delete snapshot: %s", st)
			}
			if err := b.en.doCleanStaleSession(sid); err != nil {
				t.Fatalf("remove the expired session: %s", err)
			}
			// a session of this client is no obstacle
			b.sid = 0
			if err := m.NewSession(true); err != nil {
				t.Fatalf("new session: %s", err)
			}
			if _, st := m.CreateSnapshot(ctx, dir, "s1", false, nil, nil); st != 0 {
				t.Fatalf("snapshot with this client mounted: %s", st)
			}
			if err := m.CloseSession(); err != nil {
				t.Fatalf("close session: %s", err)
			}
		})
	}
}

// otherClient is a second client of the volume m holds, standing for another
// process. The stores a single process owns cannot be opened twice, so there
// it is m itself: what the tests exercise is a format read earlier, and Load
// returns a copy.
func otherClient(t *testing.T, e compatEngine, uri string, m Meta) Meta {
	t.Helper()
	if e.name == "memkv" || e.name == "badger" {
		return m
	}
	other := NewClient(uri, testConfig())
	t.Cleanup(func() { _ = other.Shutdown() })
	if _, err := other.Load(true); err != nil {
		t.Fatalf("load %s: %s", uri, err)
	}
	return other
}

// A config that read the format before the first snapshot, or before another
// config raised the minimum client version, writes the whole format back: the
// write must keep the versions the store has meanwhile reached.
func TestFormatWriteKeepsVersions(t *testing.T) {
	for _, e := range compatEngines() {
		t.Run(e.name, func(t *testing.T) {
			uri := e.uri(t)
			m1 := newCompatMeta(t, uri)
			stale, err := m1.Load(false) // what `juicefs config` reads first
			if err != nil {
				t.Fatalf("load: %s", err)
			}
			m2 := otherClient(t, e, uri, m1)
			raised := *stale
			raised.MinClientVersion = "1.4.0"
			if err := m2.Init(&raised, false); err != nil {
				t.Fatalf("raise the minimum client version: %s", err)
			}
			var dir Ino
			if st := m2.Mkdir(Background(), RootInode, "d", 0755, 0, 0, &dir, nil); st != 0 {
				t.Fatalf("mkdir: %s", st)
			}
			if _, st := m2.CreateSnapshot(Background(), dir, "s1", false, nil, nil); st != 0 {
				t.Fatalf("create snapshot: %s", st)
			}
			before, err := m2.Load(false)
			if err != nil {
				t.Fatalf("load: %s", err)
			}

			stale.Capacity = 1 << 30 // the change config makes
			if err := m1.Init(stale, false); err != nil {
				t.Fatalf("config from a stale format: %s", err)
			}
			after, err := m2.Load(false)
			if err != nil {
				t.Fatalf("load: %s", err)
			}
			if after.Capacity != 1<<30 {
				t.Fatalf("capacity %d after config, want %d", after.Capacity, 1<<30)
			}
			if after.MetaVersion < before.MetaVersion {
				t.Fatalf("meta version lowered from %d to %d by a stale config", before.MetaVersion, after.MetaVersion)
			}
			if below, err := belowVersion(after.MinClientVersion, before.MinClientVersion); err != nil || below {
				t.Fatalf("min client version lowered from %q to %q by a stale config (%v)", before.MinClientVersion, after.MinClientVersion, err)
			}
		})
	}
}

// setCounter writes a counter as a load by another client may have left it.
func setCounter(t *testing.T, m Meta, name string, value int64) {
	t.Helper()
	var err error
	switch m := m.(type) {
	case *redisMeta:
		err = m.rdb.Set(Background(), m.counterKey(name), value, 0).Err()
	case *dbMeta:
		err = m.txn(func(s *xorm.Session) error {
			c := counter{Name: name}
			ok, err := s.ForUpdate().Get(&c)
			if err != nil {
				return err
			}
			c.Value = value
			if ok {
				_, err = s.Cols("value").Update(&c, &counter{Name: name})
			} else {
				err = mustInsert(s, &c)
			}
			return err
		})
	case *kvMeta:
		err = m.setValue(m.counterKey(name), packCounter(value))
	default:
		t.Fatalf("unknown engine %T", m)
	}
	if err != nil {
		t.Fatalf("set counter %s: %s", name, err)
	}
}

// A load by a client unaware of the snapshot range counts the snapshot inodes
// as trash and leaves the trash counter past its range. The next hourly trash
// directory must not be written in the snapshot range, where it would share
// an inode with a snapshot: the counter starts over from the highest trash
// directory there is, in the transaction that creates the new one.
func TestTrashCounterPastRange(t *testing.T) {
	for _, e := range compatEngines() {
		t.Run(e.name, func(t *testing.T) {
			ctx := Background()
			m := newCompatMeta(t, e.uri(t))
			f := compatFormat()
			f.TrashDays = 1
			if err := m.Init(f, false); err != nil {
				t.Fatalf("enable trash: %s", err)
			}
			b := m.getBase()
			// an hourly directory from earlier, at the first trash inode
			var earlier Ino
			attr := Attr{Typ: TypeDirectory, Nlink: 2, Length: 4 << 10, Parent: TrashInode, Full: true}
			if st := b.en.doMknod(ctx, TrashInode, "2000-01-01-00", TypeDirectory, 0555, 0, "", &earlier, &attr); st != 0 {
				t.Fatalf("earlier trash directory: %s", st)
			}
			if earlier != TrashInode+1 {
				t.Fatalf("earlier trash directory got inode %d, want %d", earlier, TrashInode+1)
			}
			past := int64(SnapshotInode-TrashInode) + 5
			setCounter(t, m, "nextTrash", past)

			var file Ino
			if st := m.Create(ctx, RootInode, "f", 0644, 0, 0, &file, nil); st != 0 {
				t.Fatalf("create: %s", st)
			}
			if st := m.Unlink(ctx, RootInode, "f"); st != 0 {
				t.Fatalf("unlink with the trash counter past its range: %s", st)
			}
			if trash := b.subTrash.inode; trash != TrashInode+2 {
				t.Fatalf("the hourly trash directory got inode %d, want %d", trash, TrashInode+2)
			}
			if st := b.en.doGetAttr(ctx, TrashInode+Ino(past)+1, nil); st != syscall.ENOENT {
				t.Fatalf("inode %d in the snapshot range: %v, want ENOENT", TrashInode+Ino(past)+1, st)
			}
			if v, err := b.en.getCounter("nextTrash"); err != nil || v != 2 {
				t.Fatalf("trash counter %d %v after the repair, want 2", v, err)
			}
			// the counter goes on from there
			var later Ino
			if st := b.en.doMknod(ctx, TrashInode, "2000-01-01-01", TypeDirectory, 0555, 0, "", &later, &attr); st != 0 || later != TrashInode+3 {
				t.Fatalf("later trash directory: %v, inode %d, want %d", st, later, TrashInode+3)
			}
		})
	}
}

// A load by a client unaware of snapshots leaves the snapshot counter behind
// the snapshots it loaded: a new snapshot skips the inodes that are taken
// instead of overwriting the snapshot that holds them.
func TestSnapshotSkipsTakenInodes(t *testing.T) {
	for _, e := range compatEngines() {
		t.Run(e.name, func(t *testing.T) {
			ctx := Background()
			m := newCompatMeta(t, e.uri(t))
			var dir, file Ino
			if st := m.Mkdir(ctx, RootInode, "d", 0755, 0, 0, &dir, nil); st != 0 {
				t.Fatalf("mkdir: %s", st)
			}
			if st := m.Create(ctx, dir, "f", 0644, 0, 0, &file, nil); st != 0 {
				t.Fatalf("create: %s", st)
			}
			first, st := m.CreateSnapshot(ctx, dir, "s1", false, nil, nil)
			if st != 0 {
				t.Fatalf("create snapshot: %s", st)
			}
			setCounter(t, m, "nextSnapshot", 0)
			second, st := m.CreateSnapshot(ctx, dir, "s2", false, nil, nil)
			if st != 0 {
				t.Fatalf("create snapshot with the counter behind: %s", st)
			}
			if second == first {
				t.Fatalf("the second snapshot took the inode of the first, %d", first)
			}
			var got Ino
			var attr Attr
			if st := m.Lookup(ctx, SnapshotInode, "s1", &got, &attr, false); st != 0 || got != first {
				t.Fatalf("first snapshot after the second: %v, inode %d, want %d", st, got, first)
			}
			if st := m.Lookup(ctx, first, "f", &got, &attr, false); st != 0 || attr.Flags&FlagSnapshot == 0 {
				t.Fatalf("file in the first snapshot: %v, flags %d", st, attr.Flags)
			}
			if snaps, st := m.ListSnapshots(ctx); st != 0 || len(snaps) != 2 {
				t.Fatalf("snapshots: %v, %d, want 2", st, len(snaps))
			}
		})
	}
}

// A dump of a snapshot, or of the hidden root that holds them, is refused: the
// volume loaded from it would keep the frozen entries, and so could never be
// changed or emptied. A dump of a live directory is unaffected.
func TestDumpRefusesSnapshotSubdir(t *testing.T) {
	for _, e := range compatEngines() {
		t.Run(e.name, func(t *testing.T) {
			ctx := Background()
			m := newCompatMeta(t, e.uri(t))
			var dir, file Ino
			if st := m.Mkdir(ctx, RootInode, "d", 0755, 0, 0, &dir, nil); st != 0 {
				t.Fatalf("mkdir: %s", st)
			}
			if st := m.Create(ctx, dir, "f", 0644, 0, 0, &file, nil); st != 0 {
				t.Fatalf("create: %s", st)
			}
			if _, st := m.CreateSnapshot(ctx, dir, "s1", false, nil, nil); st != 0 {
				t.Fatalf("create snapshot: %s", st)
			}
			b := m.getBase()
			for _, subdir := range []string{SnapshotName, SnapshotName + "/s1"} {
				b.chroot(RootInode)
				if st := m.Chroot(ctx, subdir); st != 0 {
					t.Fatalf("chroot %s: %s", subdir, st)
				}
				var buf bytes.Buffer
				err := m.DumpMeta(&buf, RootInode, 1, true, false, false)
				if err == nil || !strings.Contains(err.Error(), "cannot dump a snapshot") {
					t.Fatalf("dump of %s: %v, want a refusal", subdir, err)
				}
				if buf.Len() != 0 {
					t.Fatalf("dump of %s wrote %d bytes before refusing", subdir, buf.Len())
				}
			}
			b.chroot(RootInode)
			if st := m.Chroot(ctx, "d"); st != 0 {
				t.Fatalf("chroot d: %s", st)
			}
			var buf bytes.Buffer
			if err := m.DumpMeta(&buf, RootInode, 1, true, false, false); err != nil {
				t.Fatalf("dump of a live directory: %s", err)
			}
			var dm DumpedMeta
			if err := json.Unmarshal(buf.Bytes(), &dm); err != nil {
				t.Fatalf("parse the dump: %s", err)
			}
			if dm.FSTree == nil || dm.FSTree.Entries["f"] == nil || dm.FSTree.Entries["f"].Attr.Flags&FlagSnapshot != 0 {
				t.Fatalf("dump of a live directory: %+v", dm.FSTree)
			}
		})
	}
}

// sortedDump re-serializes a JSON dump with its keys sorted, as a tool that
// edits it may, which puts FSTree before Setting.
func sortedDump(t *testing.T, dump []byte, edit func(map[string]json.RawMessage)) []byte {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(dump, &keys); err != nil {
		t.Fatalf("parse the dump: %s", err)
	}
	if edit != nil {
		edit(keys)
	}
	sorted, err := json.Marshal(keys)
	if err != nil {
		t.Fatalf("serialize the dump: %s", err)
	}
	if bytes.Index(sorted, []byte(`"FSTree"`)) > bytes.Index(sorted, []byte(`"Setting"`)) {
		t.Fatalf("the sorted dump still has Setting before FSTree")
	}
	return sorted
}

// A JSON dump whose keys were reordered so that its entries come before its
// settings loads from a plain file, checked before anything is written; from
// a stream it is refused, since it cannot be checked first.
func TestLoadReorderedDump(t *testing.T) {
	for _, e := range compatEngines() {
		t.Run(e.name, func(t *testing.T) {
			ctx := Background()
			src := newCompatMeta(t, e.uri(t))
			var dir, file Ino
			if st := src.Mkdir(ctx, RootInode, "d", 0755, 0, 0, &dir, nil); st != 0 {
				t.Fatalf("mkdir: %s", st)
			}
			if st := src.Create(ctx, dir, "f", 0644, 0, 0, &file, nil); st != 0 {
				t.Fatalf("create: %s", st)
			}
			if st := src.Write(ctx, file, 0, 0, Slice{Id: 300001, Size: 100, Len: 100}, time.Now()); st != 0 {
				t.Fatalf("write: %s", st)
			}
			if _, st := src.CreateSnapshot(ctx, dir, "s1", false, nil, nil); st != 0 {
				t.Fatalf("create snapshot: %s", st)
			}
			var buf bytes.Buffer
			if err := src.DumpMeta(&buf, RootInode, 1, true, false, false); err != nil {
				t.Fatalf("dump: %s", err)
			}
			sorted := sortedDump(t, buf.Bytes(), nil)

			// from a file: loaded whole
			dst := emptyCompatMeta(t, e.uri(t))
			if err := dst.LoadMeta(bytes.NewReader(sorted)); err != nil {
				t.Fatalf("load the reordered dump: %s", err)
			}
			if f, err := dst.Load(true); err != nil || f.MetaVersion != 2 {
				t.Fatalf("format after the load: %+v %v", f, err)
			}
			var got Ino
			var attr Attr
			if st := dst.Lookup(ctx, RootInode, "d", &got, &attr, false); st != 0 {
				t.Fatalf("lookup d: %s", st)
			}
			if st := dst.Lookup(ctx, got, "f", &got, &attr, false); st != 0 || attr.Length != 100 {
				t.Fatalf("lookup f: %v, length %d", st, attr.Length)
			}
			if st := dst.Lookup(ctx, SnapshotInode, "s1", &got, &attr, false); st != 0 || attr.Flags&FlagSnapshot == 0 {
				t.Fatalf("lookup the snapshot: %v, flags %d", st, attr.Flags)
			}

			// from a stream: refused, and nothing written
			dst = emptyCompatMeta(t, e.uri(t))
			err := dst.LoadMeta(io.MultiReader(bytes.NewReader(sorted)))
			if err == nil || !strings.Contains(err.Error(), "plain JSON file") {
				t.Fatalf("load the reordered dump from a stream: %v, want a refusal", err)
			}
			if st := dst.getBase().en.doGetAttr(ctx, RootInode, &Attr{}); st == 0 {
				t.Fatalf("a refused load wrote the root")
			}

			// from a file, for a newer client: refused, and nothing written
			newer := sortedDump(t, buf.Bytes(), func(keys map[string]json.RawMessage) {
				var f Format
				if err := json.Unmarshal(keys["Setting"], &f); err != nil {
					t.Fatalf("parse Setting: %s", err)
				}
				f.MetaVersion = MaxVersion + 1
				keys["Setting"], _ = json.Marshal(&f)
			})
			dst = emptyCompatMeta(t, e.uri(t))
			err = dst.LoadMeta(bytes.NewReader(newer))
			if err == nil || !strings.Contains(err.Error(), "please upgrade the client") {
				t.Fatalf("load a reordered dump for newer clients: %v, want a refusal", err)
			}
			if st := dst.getBase().en.doGetAttr(ctx, RootInode, &Attr{}); st == 0 {
				t.Fatalf("a refused load wrote the root")
			}
		})
	}
}
