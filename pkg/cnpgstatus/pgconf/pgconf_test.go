// Copyright 2026 The pgToolBox Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pgconf

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
)

// The expected values are what PostgreSQL 17 answers to SHOW (or
// pg_settings.setting) for the same assignment.
func TestParseAsPostgreSQLDoes(t *testing.T) {
	cases := []struct {
		value string
		unit  BaseUnit
		want  int64
		bad   bool
	}{
		{"128MB", Blocks, 16384, false},
		{"128 MB", Blocks, 16384, false},
		{"  128MB  ", Blocks, 16384, false},
		{"1GB", KB, 1048576, false},
		{"1.5GB", KB, 1572864, false},
		{"30.1GB", KB, 31561728, false}, // 30822 MB: rounded to the next smaller unit first
		{"16384", Blocks, 16384, false}, // bare number: base unit
		{"4096", KB, 4096, false},
		{"0x10", KB, 16, false}, // strtol base 0
		{"010", KB, 8, false},   // octal
		{"1e3", KB, 1000, false},
		{"2.5", Unitless, 2, false}, // rint: round half to even
		{"3.5", Unitless, 4, false},
		{"100", Unitless, 100, false},
		{"-1", Blocks, -1, false},
		{"1TB", KB, 1073741824, false},
		{"100B", KB, 0, false}, // 100 B rounds to 0 kB (PostgreSQL then range-checks)
		{"8192B", Blocks, 1, false},
		{"1mb", KB, 0, true},  // units are case-sensitive
		{"1 kb", KB, 0, true}, // kB, not kb
		{"1KB", KB, 0, true},
		{"1MB x", KB, 0, true},
		{"MB", KB, 0, true},
		{"", KB, 0, true},
		{"abc", KB, 0, true},
		{"100MB", Unitless, 0, true},
		{"2TB", Blocks, 268435456, false},
		{"4TB", KB, 0, true}, // 4294967296 kB overflows int32
		{"nan", KB, 0, true},
		{"inf", KB, 0, true},
	}
	for _, c := range cases {
		got, err := Parse(c.value, c.unit)
		if c.bad {
			if err == nil || !errors.Is(err, ErrSyntax) {
				t.Errorf("Parse(%q) = %d, %v; want a refusal", c.value, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("Parse(%q, %d) = %d, %v; want %d", c.value, c.unit, got, err, c.want)
		}
	}
}

func TestReadMemoryDefaultsAndDerivations(t *testing.T) {
	m := ReadMemory(nil, 0)
	if !m.SharedBuffers.Default || m.SharedBuffers.Value != 128<<20 {
		t.Errorf("shared_buffers default = %+v", m.SharedBuffers)
	}
	// 128 MB / 32 = 4 MB.
	if !m.WALBuffers.Auto || m.WALBuffers.Value != 4<<20 {
		t.Errorf("wal_buffers auto = %+v", m.WALBuffers)
	}
	if !m.AutovacuumWorkMem.Auto || m.AutovacuumWorkMem.Value != 64<<20 {
		t.Errorf("autovacuum_work_mem = %+v", m.AutovacuumWorkMem)
	}
	if m.HugePages.Value != HugePagesTry || !m.HugePages.Default {
		t.Errorf("huge_pages default = %+v", m.HugePages)
	}
	// 128 + 4 + 100×4 + 3×64 = 724 MB.
	if got := m.WorstCase(0); got != 724<<20 {
		t.Errorf("WorstCase = %d, want %d", got, 724<<20)
	}

	m = ReadMemory(map[string]string{
		"Shared_Buffers": "8GB", "wal_buffers": "-1", "work_mem": "64MB", "max_connections": "200",
		"maintenance_work_mem": "2GB", "autovacuum_max_workers": "5", "huge_pages": "ON",
	}, 64<<20)
	if m.SharedBuffers.Value != 8<<30 || m.SharedBuffers.Default {
		t.Errorf("case-insensitive name not read: %+v", m.SharedBuffers)
	}
	// 8 GB / 32 = 256 MB, capped at the 64 MB segment.
	if m.WALBuffers.Value != 64<<20 {
		t.Errorf("wal_buffers cap = %d", m.WALBuffers.Value)
	}
	if m.HugePages.Value != HugePagesOn {
		t.Errorf("huge_pages = %+v", m.HugePages)
	}
	// PostgreSQL 16 caps the autovacuum store at 1 GB; 17 does not.
	if m.AutovacuumPerWorker(16) != 1<<30 || m.AutovacuumPerWorker(17) != 2<<30 || m.AutovacuumPerWorker(0) != 2<<30 {
		t.Errorf("autovacuum cap: 16=%d 17=%d", m.AutovacuumPerWorker(16), m.AutovacuumPerWorker(17))
	}

	m = ReadMemory(map[string]string{"shared_buffers": "64kB"}, 0)
	if m.WALBuffers.Value != 64<<10 {
		t.Errorf("wal_buffers floor = %d", m.WALBuffers.Value)
	}

	m = ReadMemory(map[string]string{"work_mem": "4mb", "huge_pages": "sometimes", "max_connections": "-5", "wal_buffers": "2MB"}, 0)
	if len(m.Unparsable) != 3 {
		t.Fatalf("unparsable = %+v", m.Unparsable)
	}
	for i, name := range []string{"huge_pages", "max_connections", "work_mem"} {
		if m.Unparsable[i].Name != name {
			t.Errorf("unparsable[%d] = %+v, want %s", i, m.Unparsable[i], name)
		}
	}
	if m.WALBuffers.Auto || m.WALBuffers.Value != 2<<20 {
		t.Errorf("explicit wal_buffers = %+v", m.WALBuffers)
	}
}

// FuzzParse holds the invariants: a refusal wraps ErrSyntax, an
// acceptance fits a 32-bit integer, and a canonical rendering of an
// accepted value reads back to the same number.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{"128MB", "1.5GB", "0x10", "010", "-1", "1e3", " 4 kB ", "30.1GB", "9999999999", "1mb"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		for _, unit := range []BaseUnit{Unitless, KB, Blocks, MB} {
			n, err := Parse(value, unit)
			if err != nil {
				if !errors.Is(err, ErrSyntax) {
					t.Fatalf("Parse(%q) refused without ErrSyntax: %v", value, err)
				}
				continue
			}
			if n > math.MaxInt32 || n < math.MinInt32 {
				t.Fatalf("Parse(%q) = %d, outside int32", value, n)
			}
			back, err := Parse(strconv.FormatInt(n, 10), unit)
			if err != nil || back != n {
				t.Fatalf("Parse(%q) = %d does not read back: %d, %v", value, n, back, err)
			}
		}
	})
}

func TestEquivalent(t *testing.T) {
	cases := []struct {
		name, a, b string
		want       bool
	}{
		{"shared_buffers", "1GB", "1024MB", true},
		{"shared_buffers", "131072", "1GB", true}, // 8 kB pages
		{"Work_Mem", "4MB", "4096", true},
		{"max_wal_size", "1GB", "1024", true}, // MB base unit
		{"work_mem", "4MB", "8MB", false},
		{"work_mem", "4mb", "4MB", false},                   // unparsable on one side
		{"log_min_duration_statement", "1s", "1000", false}, // not a memory parameter
		{"application_name", "x", "x", true},
	}
	for _, c := range cases {
		if got := Equivalent(c.name, c.a, c.b); got != c.want {
			t.Errorf("Equivalent(%s, %q, %q) = %v, want %v", c.name, c.a, c.b, got, c.want)
		}
	}
}

// TestReadMemoryRefusesWhatPostgreSQLRefuses: a value outside
// guc_tables.c's range would stop the server, so it is unparsable, never
// a number the estimate multiplies (300000 connections × 2 TB of
// work_mem overflowed int64).
func TestReadMemoryRefusesWhatPostgreSQLRefuses(t *testing.T) {
	m := ReadMemory(map[string]string{
		"max_connections": "300000", "work_mem": "2000000MB", "autovacuum_max_workers": "262144",
		"shared_buffers": "8kB", "maintenance_work_mem": "512kB", "huge_pages": " on",
	}, 0)
	got := map[string]bool{}
	for _, u := range m.Unparsable {
		got[u.Name] = true
		if !errors.Is(errorFor(u), ErrSyntax) {
			t.Errorf("%s: reason %q", u.Name, u.Reason)
		}
	}
	for _, name := range []string{"max_connections", "autovacuum_max_workers", "shared_buffers", "maintenance_work_mem", "huge_pages"} {
		if !got[name] {
			t.Errorf("%s accepted: %+v", name, m.Unparsable)
		}
	}
	// 2000000MB is 2048000000 kB, within MAX_KILOBYTES.
	if got["work_mem"] {
		t.Errorf("work_mem 2000000MB refused")
	}
	m = ReadMemory(map[string]string{"max_connections": "262143", "autovacuum_max_workers": "262143", "work_mem": "2147483647kB", "maintenance_work_mem": "2147483647kB"}, 0)
	if len(m.Unparsable) != 0 {
		t.Fatalf("maxima refused: %+v", m.Unparsable)
	}
	if w := m.WorstCase(17); w <= 0 || w == math.MaxInt64 {
		t.Errorf("WorstCase at the maxima = %d", w)
	}
	if w := (Memory{MaxConnections: Setting{Value: math.MaxInt64}, WorkMem: Setting{Value: 2}}).WorstCase(17); w != math.MaxInt64 {
		t.Errorf("WorstCase did not saturate: %d", w)
	}
}

func errorFor(u Unparsable) error {
	if strings.HasPrefix(u.Reason, ErrSyntax.Error()) {
		return ErrSyntax
	}
	return errors.New(u.Reason)
}

// TestAutoWALBuffersCountsPages: XLOGChooseNumBuffers divides pages, not
// bytes. 8200 kB is 1025 pages; 1025 / 32 = 32 pages = 256 kB, where a
// division of bytes would give 262400.
func TestAutoWALBuffersCountsPages(t *testing.T) {
	m := ReadMemory(map[string]string{"shared_buffers": "8200kB"}, 0)
	if m.WALBuffers.Value != 32*BlockSize {
		t.Errorf("wal_buffers auto = %d, want %d", m.WALBuffers.Value, 32*BlockSize)
	}
}
