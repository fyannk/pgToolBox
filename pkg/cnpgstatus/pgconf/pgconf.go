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

// Package pgconf reads PostgreSQL configuration values the way the
// server does, so the console can reason about a Cluster's declared
// parameters (spec.postgresql.parameters) without guessing at units.
//
// The rules are those of src/backend/utils/misc/guc.c, parse_int and
// convert_to_base_unit (unchanged from PostgreSQL 12 to 18):
//
//   - the number is read like C strtol with base 0, so "0x10" is hex and
//     "010" octal; a '.', 'e' or 'E' after it re-reads it like strtod,
//     so "1.5GB" and "1e3" are accepted;
//   - whitespace may separate the number from the unit and follow it;
//   - memory units are B, kB, MB, GB and TB, case-sensitive, with a
//     multiplier of 1024;
//   - a value without a unit is in the parameter's base unit: kB for
//     work_mem, 8 kB blocks for shared_buffers, wal_buffers and
//     effective_cache_size;
//   - a fractional value with a unit is rounded to a multiple of the
//     next smaller unit, then the result is rounded to a whole number of
//     base units and must fit a 32-bit signed integer.
//
// Range checks (a work_mem below 64kB, say) are the server's and are
// not repeated here: a value this package accepts may still be refused
// at start-up for being out of range.
package pgconf

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// BaseUnit is the unit a parameter's bare number is counted in.
type BaseUnit int

// The base units of the parameters this package reads.
const (
	// Unitless is a plain integer (max_connections); a unit is refused.
	Unitless BaseUnit = iota
	// KB is kilobytes (work_mem, maintenance_work_mem).
	KB
	// Blocks is 8 kB pages (shared_buffers, wal_buffers,
	// effective_cache_size). BLCKSZ is 8192 in every build CloudNativePG
	// ships.
	Blocks
	// MB is megabytes (max_wal_size, min_wal_size, wal_keep_size).
	MB
)

// BlockSize is BLCKSZ, the size of one page.
const BlockSize = 8192

// bytesPer is the size of one base unit in bytes; 1 for Unitless.
func (u BaseUnit) bytesPer() int64 {
	switch u {
	case KB:
		return 1024
	case Blocks:
		return BlockSize
	case MB:
		return 1 << 20
	default:
		return 1
	}
}

// memoryUnits is guc.c's memory_unit_conversion_table, in its order
// (largest first), in bytes.
var memoryUnits = []struct {
	unit  string
	bytes float64
}{
	{"TB", 1 << 40},
	{"GB", 1 << 30},
	{"MB", 1 << 20},
	{"kB", 1 << 10},
	{"B", 1},
}

// ErrSyntax is wrapped by every refusal.
var ErrSyntax = errors.New("not a value PostgreSQL accepts")

func refuse(value, why string) error {
	return fmt.Errorf("%w: %q %s", ErrSyntax, value, why)
}

// isSpace is C isspace in the C locale.
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

// strtol reads a C long with base 0 from the start of s (after leading
// whitespace) and returns it with the index after the last digit; end
// is 0 when no digit was read. overflow reports ERANGE.
func strtol(s string) (val float64, end int, overflow bool) {
	i := 0
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	base := 10
	switch {
	case i+1 < len(s) && s[i] == '0' && (s[i+1] == 'x' || s[i+1] == 'X') && i+2 < len(s) && isDigit(s[i+2], 16):
		base, i = 16, i+2
	case i < len(s) && s[i] == '0':
		base = 8
	}
	start := i
	var n float64
	for i < len(s) && isDigit(s[i], base) {
		n = n*float64(base) + float64(digit(s[i]))
		i++
	}
	if i == start {
		return 0, 0, false
	}
	if neg {
		n = -n
	}
	return n, i, n > math.MaxInt64 || n < math.MinInt64
}

func isDigit(c byte, base int) bool {
	d := digit(c)
	return d >= 0 && d < base
}

func digit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	default:
		return 99
	}
}

// strtod reads the longest decimal floating-point prefix of s (after
// leading whitespace). Hexadecimal floats, "inf" and "nan" are not
// read: PostgreSQL would accept "inf" only to refuse it as out of range,
// and NaN is refused outright, so both are refusals here too.
func strtod(s string) (val float64, end int, ok bool) {
	i := 0
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	start := i
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			digits++
		}
	}
	if digits == 0 {
		return 0, 0, false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && s[j] >= '0' && s[j] <= '9' {
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			i = j
		}
	}
	v, err := strconv.ParseFloat(s[start:i], 64)
	if err != nil {
		return 0, 0, false
	}
	return v, i, true
}

// Parse reads value as PostgreSQL reads an integer parameter whose bare
// number is in unit, and returns it in that base unit.
func Parse(value string, unit BaseUnit) (int64, error) {
	val, end, overflow := strtol(value)
	if end < len(value) && (value[end] == '.' || value[end] == 'e' || value[end] == 'E') || overflow {
		var ok bool
		if val, end, ok = strtod(value); !ok {
			return 0, refuse(value, "is not a number")
		}
	}
	if end == 0 {
		return 0, refuse(value, "is not a number")
	}
	rest := value[end:]
	for rest != "" && isSpace(rest[0]) {
		rest = rest[1:]
	}
	if rest != "" {
		if unit == Unitless {
			return 0, refuse(value, "carries a unit, and this parameter takes none")
		}
		converted, err := toBase(val, rest, unit)
		if err != nil {
			return 0, refuse(value, err.Error())
		}
		val = converted
	}
	val = math.RoundToEven(val) // C rint in the default rounding mode
	if val > math.MaxInt32 || val < math.MinInt32 {
		return 0, refuse(value, "is outside the range of a PostgreSQL integer parameter")
	}
	return int64(val), nil
}

// toBase is convert_to_base_unit for the memory units.
func toBase(val float64, unitText string, unit BaseUnit) (float64, error) {
	i := 0
	for i < len(unitText) && !isSpace(unitText[i]) {
		i++
	}
	name, tail := unitText[:i], strings.TrimLeft(unitText[i:], " \t\n\v\f\r")
	if tail != "" {
		return 0, errors.New("has text after its unit")
	}
	base := float64(unit.bytesPer())
	for k, u := range memoryUnits {
		if u.unit != name {
			continue
		}
		cvalue := val * u.bytes / base
		if k+1 < len(memoryUnits) {
			next := memoryUnits[k+1].bytes / base
			cvalue = math.RoundToEven(cvalue/next) * next
		}
		return cvalue, nil
	}
	return 0, errors.New(`has an invalid unit: valid units are "B", "kB", "MB", "GB" and "TB" (case-sensitive)`)
}

// Bytes reads a memory value and returns it in bytes.
func Bytes(value string, unit BaseUnit) (int64, error) {
	n, err := Parse(value, unit)
	if err != nil {
		return 0, err
	}
	return n * unit.bytesPer(), nil
}

// Setting is one parameter as the console read it.
type Setting struct {
	// Name is the parameter, lower-case.
	Name string
	// Raw is the declared value; empty when the parameter is not set.
	Raw string
	// Default is true when the parameter is not set and Value is
	// PostgreSQL's documented default.
	Default bool
	// Value is in bytes for a memory parameter, a count otherwise.
	Value int64
	// Auto is true for wal_buffers -1 (or unset): Value is derived from
	// shared_buffers.
	Auto bool
}

// Unparsable is a declared parameter the console could not read.
type Unparsable struct {
	Name, Value, Reason string
}

// Memory is the part of a configuration that decides how much memory
// PostgreSQL may take.
type Memory struct {
	SharedBuffers        Setting
	WALBuffers           Setting
	WorkMem              Setting
	MaintenanceWorkMem   Setting
	AutovacuumWorkMem    Setting
	AutovacuumMaxWorkers Setting
	MaxConnections       Setting
	EffectiveCacheSize   Setting
	// HugePages is on, off or try, normalised from PostgreSQL's
	// accepted spellings; empty when unparsable.
	HugePages Setting
	// Unparsable lists the declared values PostgreSQL would refuse, by
	// name. A Setting whose value is unparsable keeps Raw and has Value
	// 0; WorstCase is then not meaningful.
	Unparsable []Unparsable
}

// Parameters is the set of names ReadMemory reads; everything else in a
// Cluster's parameters is irrelevant to the memory budget.
var Parameters = []string{
	"autovacuum_max_workers", "autovacuum_work_mem", "effective_cache_size", "huge_pages",
	"maintenance_work_mem", "max_connections", "shared_buffers", "wal_buffers", "work_mem",
}

// The documented defaults (PostgreSQL 13–18, "Server Configuration").
// shared_buffers' compiled-in default is smaller, but initdb writes
// 128MB unless the kernel cannot provide it, and CloudNativePG keeps
// initdb's file.
const (
	defaultSharedBuffers       = 128 << 20
	defaultWorkMem             = 4 << 20
	defaultMaintenanceWorkMem  = 64 << 20
	defaultEffectiveCacheSize  = 4 << 30
	defaultMaxConnections      = 100
	defaultAutovacuumWorkers   = 3
	defaultWALSegmentBytes     = 16 << 20
	walBuffersAutoMin          = 64 << 10
	autovacuumCapBeforeVersion = 17
	autovacuumCapBytes         = 1 << 30
)

// ReadMemory reads the memory-relevant parameters of a configuration.
// Names are case-insensitive, as PostgreSQL's are; when two spellings of
// one name are declared, the later in sorted order wins, as the later
// line of postgresql.conf does (CloudNativePG writes them sorted).
// walSegmentBytes is the WAL segment size (0 for the 16 MB default),
// which bounds wal_buffers' automatic size.
func ReadMemory(params map[string]string, walSegmentBytes int64) Memory {
	lower := map[string]string{}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		lower[strings.ToLower(k)] = params[k]
	}
	if walSegmentBytes <= 0 {
		walSegmentBytes = defaultWALSegmentBytes
	}
	var m Memory
	read := func(name string, unit BaseUnit, def int64) Setting {
		s := Setting{Name: name}
		raw, ok := lower[name]
		if !ok {
			s.Default, s.Value = true, def
			return s
		}
		s.Raw = raw
		n, err := Parse(raw, unit)
		if err == nil {
			err = inRange(name, raw, n)
		}
		if err != nil {
			m.Unparsable = append(m.Unparsable, Unparsable{Name: name, Value: raw, Reason: err.Error()})
			return s
		}
		s.Value = n * unitBytes(unit)
		return s
	}
	m.SharedBuffers = read("shared_buffers", Blocks, defaultSharedBuffers)
	m.WorkMem = read("work_mem", KB, defaultWorkMem)
	m.MaintenanceWorkMem = read("maintenance_work_mem", KB, defaultMaintenanceWorkMem)
	m.EffectiveCacheSize = read("effective_cache_size", Blocks, defaultEffectiveCacheSize)
	m.MaxConnections = read("max_connections", Unitless, defaultMaxConnections)
	m.AutovacuumMaxWorkers = read("autovacuum_max_workers", Unitless, defaultAutovacuumWorkers)

	// autovacuum_work_mem and wal_buffers take -1 for "derive it"; the
	// -1 is counted in base units, so "-1" is the only spelling that
	// means it (a "-1kB" is -1 too after conversion, and PostgreSQL
	// treats it the same).
	m.AutovacuumWorkMem = read("autovacuum_work_mem", KB, -1024)
	if m.AutovacuumWorkMem.Value == -1024 {
		m.AutovacuumWorkMem.Auto = true
		m.AutovacuumWorkMem.Value = m.MaintenanceWorkMem.Value
	}
	m.WALBuffers = read("wal_buffers", Blocks, -BlockSize)
	if m.WALBuffers.Value == -BlockSize {
		m.WALBuffers.Auto = true
		// XLOGChooseNumBuffers counts pages: NBuffers / 32 (integer
		// division), at least 8 pages (64 kB), at most one WAL segment.
		pages := (m.SharedBuffers.Value / BlockSize) / 32
		pages = max(pages, walBuffersAutoMin/BlockSize)
		pages = min(pages, walSegmentBytes/BlockSize)
		m.WALBuffers.Value = pages * BlockSize
	}

	m.HugePages = Setting{Name: "huge_pages"}
	if raw, ok := lower["huge_pages"]; ok {
		m.HugePages.Raw = raw
		// config_enum_lookup_by_name: case-insensitive, never trimmed.
		switch strings.ToLower(raw) {
		case "on", "true", "yes", "1":
			m.HugePages.Value = HugePagesOn
		case "off", "false", "no", "0":
			m.HugePages.Value = HugePagesOff
		case "try":
			m.HugePages.Value = HugePagesTry
		default:
			m.Unparsable = append(m.Unparsable, Unparsable{
				Name: "huge_pages", Value: raw,
				Reason: fmt.Sprintf("%v: %q is not one of on, off, try", ErrSyntax, raw),
			})
			m.HugePages.Value = HugePagesUnknown
		}
	} else {
		m.HugePages.Default, m.HugePages.Value = true, HugePagesTry
	}
	sort.Slice(m.Unparsable, func(i, j int) bool { return m.Unparsable[i].Name < m.Unparsable[j].Name })
	return m
}

// The huge_pages values, as Setting.Value.
const (
	HugePagesUnknown int64 = iota
	HugePagesOff
	HugePagesTry
	HugePagesOn
)

// HugePagesWord names a huge_pages value.
func HugePagesWord(v int64) string {
	switch v {
	case HugePagesOn:
		return "on"
	case HugePagesOff:
		return "off"
	case HugePagesTry:
		return "try"
	default:
		return "unknown"
	}
}

// AutovacuumPerWorker is the memory one autovacuum worker may use for
// its dead-tuple store: autovacuum_work_mem, or maintenance_work_mem
// when that is -1; before PostgreSQL 17 it was silently capped at 1 GB.
// major 0 (unknown) applies no cap, so the estimate stays an upper one.
func (m Memory) AutovacuumPerWorker(major int) int64 {
	v := m.AutovacuumWorkMem.Value
	if major > 0 && major < autovacuumCapBeforeVersion && v > autovacuumCapBytes {
		v = autovacuumCapBytes
	}
	return v
}

// WorstCase is the conventional worst-case estimate of the memory
// PostgreSQL may take: shared_buffers + wal_buffers + max_connections ×
// work_mem + autovacuum_max_workers × the autovacuum worker memory. It
// is an estimate, not a bound: one query may use work_mem several times
// (each sort or hash node, times hash_mem_multiplier for hashes, times
// its parallel workers), while most connections use far less. It is
// meaningless while Unparsable is not empty.
func (m Memory) WorstCase(major int) int64 {
	// The ranges ReadMemory enforces keep this far below 2^63; the
	// saturating arithmetic holds the line for a Memory built otherwise.
	return satAdd(satAdd(m.SharedBuffers.Value, m.WALBuffers.Value),
		satAdd(satMul(m.MaxConnections.Value, m.WorkMem.Value),
			satMul(m.AutovacuumMaxWorkers.Value, m.AutovacuumPerWorker(major))))
}

func satAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func satMul(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return max(a, 0) * max(b, 0)
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

// unitBytes is the size of one base unit, in bytes (1 for Unitless).
func unitBytes(u BaseUnit) int64 {
	switch u {
	case KB:
		return 1 << 10
	case Blocks:
		return BlockSize
	case MB:
		return 1 << 20
	default:
		return 1
	}
}

// Ranges PostgreSQL accepts (guc_tables.c, PostgreSQL 13–18, 64-bit
// builds), in base units. MAX_KILOBYTES is INT_MAX where long is 64
// bits; MAX_BACKENDS is 0x3FFFF. wal_buffers and autovacuum_work_mem
// take -1 ("derive it"); their check hooks raise small values instead
// of refusing them, so only the maximum is checked.
var ranges = map[string][2]int64{
	"shared_buffers":         {16, math.MaxInt32 / 2},
	"work_mem":               {64, math.MaxInt32},
	"maintenance_work_mem":   {1024, math.MaxInt32},
	"autovacuum_work_mem":    {-1, math.MaxInt32},
	"effective_cache_size":   {1, math.MaxInt32},
	"wal_buffers":            {-1, math.MaxInt32 / BlockSize},
	"max_connections":        {1, maxBackends},
	"autovacuum_max_workers": {1, maxBackends},
}

const maxBackends = 0x3FFFF

// inRange refuses a value PostgreSQL would refuse to start with.
func inRange(name, raw string, n int64) error {
	r, ok := ranges[name]
	if !ok || n >= r[0] && n <= r[1] {
		return nil
	}
	return refuse(raw, fmt.Sprintf("is outside the range PostgreSQL accepts for %s (%d to %d)", name, r[0], r[1]))
}

// memoryParameters are the memory-unit parameters a comparison may
// normalise, with their base units (PostgreSQL 13–18, pg_settings.unit).
var memoryParameters = map[string]BaseUnit{
	"autovacuum_work_mem": KB, "effective_cache_size": Blocks, "logical_decoding_work_mem": KB,
	"maintenance_work_mem": KB, "max_slot_wal_keep_size": MB, "max_wal_size": MB, "min_dynamic_shared_memory": MB,
	"min_wal_size": MB, "shared_buffers": Blocks, "temp_buffers": Blocks, "temp_file_limit": KB,
	"wal_buffers": Blocks, "wal_keep_size": MB, "work_mem": KB,
}

// Equivalent reports that two declared values of one parameter mean the
// same setting: equal text, or, for a memory parameter, the same number
// of bytes ("1GB" and "1024MB"). An unparsable value is equivalent only
// to the same text.
func Equivalent(name, a, b string) bool {
	if a == b {
		return true
	}
	unit, ok := memoryParameters[strings.ToLower(name)]
	if !ok {
		return false
	}
	x, err := Bytes(a, unit)
	if err != nil {
		return false
	}
	y, err := Bytes(b, unit)
	return err == nil && x == y
}
