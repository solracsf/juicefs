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

import "testing"

func TestInoRanges(t *testing.T) {
	cases := []struct {
		name     string
		ino      Ino
		normal   bool
		trash    bool
		snapshot bool
	}{
		{"root", RootInode, true, false, false},
		{"regular", 1 << 20, true, false, false},
		{"last normal", TrashInode - 1, true, false, false},
		{"trash root", TrashInode, false, true, false},
		{"sub trash", TrashInode + 1, false, true, false},
		{"last trash", SnapshotInode - 1, false, true, false},
		{"snapshot root", SnapshotInode, false, false, true},
		{"snapshot child", SnapshotInode + 1, false, false, true},
	}
	for _, c := range cases {
		if got := c.ino.IsNormal(); got != c.normal {
			t.Fatalf("%s (%d): IsNormal is %v, want %v", c.name, c.ino, got, c.normal)
		}
		if got := c.ino.IsTrash(); got != c.trash {
			t.Fatalf("%s (%d): IsTrash is %v, want %v", c.name, c.ino, got, c.trash)
		}
		if got := c.ino.IsSnapshot(); got != c.snapshot {
			t.Fatalf("%s (%d): IsSnapshot is %v, want %v", c.name, c.ino, got, c.snapshot)
		}
	}

	// the ranges must not overlap, and nothing may be two things at once
	for _, c := range cases {
		n := 0
		for _, in := range []bool{c.ino.IsNormal(), c.ino.IsTrash(), c.ino.IsSnapshot()} {
			if in {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s (%d) belongs to %d ranges, want exactly 1", c.name, c.ino, n)
		}
	}

	// vfs internal nodes sit below TrashInode, so narrowing IsTrash must not have
	// pulled them into the trash range
	if TrashInode <= 0x7FFFFFFF00000000 {
		t.Fatalf("TrashInode %d must stay above vfs.minInternalNode", TrashInode)
	}
	if SnapshotInode <= TrashInode {
		t.Fatalf("SnapshotInode %d must be above TrashInode %d", SnapshotInode, TrashInode)
	}
}

// FlagSnapshot must not collide with an existing flag, and Attr.Flags is a uint8
// so the whole set has to fit in 8 bits.
func TestFlagBits(t *testing.T) {
	flags := map[string]int{
		"FlagImmutable":      FlagImmutable,
		"FlagAppend":         FlagAppend,
		"FlagWindowsHidden":  FlagWindowsHidden,
		"FlagWindowsSystem":  FlagWindowsSystem,
		"FlagWindowsArchive": FlagWindowsArchive,
		"FlagSkipTrash":      FlagSkipTrash,
		"FlagSnapshot":       FlagSnapshot,
	}
	seen := make(map[int]string, len(flags))
	for name, bit := range flags {
		if bit > 0xFF {
			t.Fatalf("%s (%d) does not fit in Attr.Flags", name, bit)
		}
		if other, ok := seen[bit]; ok {
			t.Fatalf("%s and %s share bit %d", name, other, bit)
		}
		seen[bit] = name
	}
}
