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
	"testing"
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

// newCompatMeta opens an empty volume on the engine, formatted with the test
// format, and closes it with the test.
func newCompatMeta(t *testing.T, uri string) Meta {
	t.Helper()
	m := NewClient(uri, testConfig())
	if err := m.Reset(); err != nil {
		t.Fatalf("reset %s: %s", uri, err)
	}
	t.Cleanup(func() {
		_ = m.Reset()
		_ = m.Shutdown()
	})
	if err := m.Init(testFormat(), false); err != nil {
		t.Fatalf("init %s: %s", uri, err)
	}
	return m
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
