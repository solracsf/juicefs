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

package fs

import (
	"syscall"
	"testing"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/vfs"
)

// A handle opened without write access cannot write, so a snapshot file cannot
// be rewritten through a read-only open, the only open it allows.
func TestSnapshotReadOnlyHandle(t *testing.T) {
	fs := createTestFS(t)
	ctx := meta.NewContext(1, 1000, []uint32{1000})
	if e := fs.Mkdir(ctx, "/src", 0777, 0); e != 0 {
		t.Fatal(e)
	}
	f, e := fs.Create(ctx, "/src/f", 0644, 0)
	if e != 0 {
		t.Fatal(e)
	}
	if _, e := f.Write(ctx, []byte("original")); e != 0 {
		t.Fatal(e)
	}
	if e := f.Close(ctx); e != 0 {
		t.Fatal(e)
	}
	fi, e := fs.Stat(ctx, "/src")
	if e != 0 {
		t.Fatal(e)
	}
	if _, st := fs.m.CreateSnapshot(meta.Background(), fi.Inode(), "s", true, nil, nil); st != 0 {
		t.Fatal(st)
	}
	snap := "/" + meta.SnapshotName + "/s/f"
	if _, e := fs.Open(ctx, snap, vfs.MODE_MASK_W); e != syscall.EPERM {
		t.Fatalf("open a snapshot file for writing: %v", e)
	}
	r, e := fs.Open(ctx, snap, vfs.MODE_MASK_R)
	if e != 0 {
		t.Fatal(e)
	}
	if n, e := r.Pwrite(ctx, []byte("TAMPERED"), 0); e != syscall.EBADF || n != 0 {
		t.Errorf("pwrite through a read-only handle: (%d, %v)", n, e)
	}
	if n, e := r.Write(ctx, []byte("TAMPERED")); e != syscall.EBADF || n != 0 {
		t.Errorf("write through a read-only handle: (%d, %v)", n, e)
	}
	if e := r.Flush(ctx); e != 0 {
		t.Errorf("flush: %v", e)
	}
	_ = r.Close(ctx)
	r, e = fs.Open(ctx, snap, vfs.MODE_MASK_R)
	if e != 0 {
		t.Fatal(e)
	}
	buf := make([]byte, 8)
	if _, err := r.Pread(ctx, buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "original" {
		t.Errorf("snapshot file changed through a read-only handle: %q", buf)
	}
	_ = r.Close(ctx)

	// handles with write access keep working
	w, e := fs.Open(ctx, "/src/f", vfs.MODE_MASK_R|vfs.MODE_MASK_W)
	if e != 0 {
		t.Fatal(e)
	}
	if n, e := w.Pwrite(ctx, []byte("changed!"), 0); e != 0 || n != 8 {
		t.Errorf("pwrite through a read-write handle: (%d, %v)", n, e)
	}
	if e := w.Close(ctx); e != 0 {
		t.Fatal(e)
	}
	w, e = fs.Open(ctx, "/src/f", vfs.MODE_MASK_W)
	if e != 0 {
		t.Fatal(e)
	}
	if n, e := w.Write(ctx, []byte("CHANGED!")); e != 0 || n != 8 {
		t.Errorf("write through a write-only handle: (%d, %v)", n, e)
	}
	if e := w.Close(ctx); e != 0 {
		t.Fatal(e)
	}
	r, e = fs.Open(ctx, "/src/f", vfs.MODE_MASK_R)
	if e != 0 {
		t.Fatal(e)
	}
	if _, err := r.Pread(ctx, buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "CHANGED!" {
		t.Errorf("live file after writes: %q", buf)
	}
}
