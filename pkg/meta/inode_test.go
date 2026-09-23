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
}
