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

package pgbackrest

import (
	"bytes"
	"crypto/sha1" // #nosec G505 -- the test recomputes pgBackRest's SHA-1 info checksum.
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// withChecksum appends the [backrest] backrest-checksum section to body,
// computing the canonical JSON independently of ParseINI: it is the oracle
// the parser's hashing is tested against. body must be well formed.
func withChecksum(t testing.TB, body string) []byte {
	t.Helper()
	var canonical strings.Builder
	canonical.WriteString("{")
	section, last := "", ""
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}
		key, value, ok := "", "", false
		for index := strings.IndexByte(line, '='); index >= 0; {
			if json.Valid([]byte(line[index+1:])) {
				key, value, ok = line[:index], line[index+1:], true
				break
			}
			next := strings.IndexByte(line[index+1:], '=')
			if next < 0 {
				break
			}
			index += next + 1
		}
		if !ok {
			key, value, _ = strings.Cut(line, "=")
		}
		if section == "backrest" && key == "backrest-checksum" {
			continue
		}
		if section != last {
			if last != "" {
				canonical.WriteString("},")
			}
			canonical.WriteString(`"` + section + `":{`)
			last = section
		} else {
			canonical.WriteString(",")
		}
		canonical.WriteString(`"` + strings.NewReplacer(`"`, `\"`, `\`, `\\`, "\t", `\t`).Replace(key) + `":` + value)
	}
	canonical.WriteString("}}")
	sum := sha1.Sum([]byte(canonical.String())) // #nosec G401 -- see the import.
	return []byte(body + "\n[backrest]\nbackrest-checksum=\"" + hex.EncodeToString(sum[:]) + "\"\n")
}

const minimalINI = "[backrest]\nbackrest-format=5\nbackrest-version=\"2.60.0\"\n\n[db]\ndb-id=1\n"

func TestParseINIFollowsPgBackRestStrictRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		data    []byte
		wantErr error
		check   func(*testing.T, *INIFile)
	}{
		{name: "minimal file", data: withChecksum(t, minimalINI), check: func(t *testing.T, file *INIFile) {
			if file.Format != 5 || file.Version != "2.60.0" {
				t.Fatalf("format %d version %q", file.Format, file.Version)
			}
			if raw, ok := file.Get("db", "db-id"); !ok || raw != "1" {
				t.Fatalf("db-id = %q, %v", raw, ok)
			}
		}},
		{name: "blank lines anywhere", data: withChecksum(t, "\n\n[backrest]\n\nbackrest-format=5\n\n\nbackrest-version=\"2.60.0\"\n[db]\n\ndb-id=1\n\n")},
		{name: "last line without a newline", data: bytes.TrimSuffix(withChecksum(t, minimalINI), []byte("\n"))},
		{name: "a key that contains =", data: withChecksum(t, minimalINI+"[target:file]\npg_data/a=b={\"size\":1}\n"), check: func(t *testing.T, file *INIFile) {
			if raw, ok := file.Get("target:file", "pg_data/a=b"); !ok || raw != `{"size":1}` {
				t.Fatalf("value = %q, %v", raw, ok)
			}
		}},
		{name: "a key pgBackRest escapes in the checksum", data: withChecksum(t, minimalINI+"[target:file]\npg_data/a\"b\\c\td={}\n")},
		{name: "the checksum written first, as older versions did", data: func() []byte {
			full := withChecksum(t, minimalINI)
			index := bytes.Index(full, []byte("\n[backrest]\nbackrest-checksum="))
			checksumLine := full[index+len("\n[backrest]\n"):]
			return append([]byte("[backrest]\n"+string(bytes.TrimSuffix(checksumLine, []byte("\n")))+"\n"), full[len("[backrest]\n"):index]...)
		}()},
		{name: "a value that is an array", data: withChecksum(t, minimalINI+"[x]\nlist=[1,2]\n")},
		{name: "an empty file", data: nil, wantErr: ErrChecksumMismatch},
		{name: "no checksum", data: []byte(minimalINI), wantErr: ErrChecksumMismatch},
		{name: "a changed value", data: bytes.Replace(withChecksum(t, minimalINI), []byte("db-id=1"), []byte("db-id=2"), 1), wantErr: ErrChecksumMismatch},
		{name: "a reordered section", data: func() []byte {
			data := withChecksum(t, minimalINI+"[a]\nx=1\n")
			return bytes.Replace(data, []byte("[db]\ndb-id=1\n[a]\nx=1"), []byte("[a]\nx=1\n[db]\ndb-id=1"), 1)
		}(), wantErr: ErrChecksumMismatch},
		{name: "an upper-case checksum", data: func() []byte {
			data := withChecksum(t, minimalINI)
			index := bytes.LastIndex(data, []byte("=\""))
			return append(data[:index+2:index+2], bytes.ToUpper(data[index+2:])...)
		}(), wantErr: ErrChecksumMismatch},
		{name: "a comment", data: append([]byte("# note\n"), withChecksum(t, minimalINI)...), wantErr: errMalformedINI},
		{name: "a key outside a section", data: append([]byte("a=1\n"), withChecksum(t, minimalINI)...), wantErr: errMalformedINI},
		{name: "an empty section", data: append(withChecksum(t, minimalINI), []byte("[]\n")...), wantErr: errMalformedINI},
		{name: "an empty key", data: withChecksum(t, minimalINI+"[x]\n=1\n"), wantErr: errMalformedINI},
		{name: "a value that is not JSON", data: []byte(minimalINI + "[x]\nkey=value\n"), wantErr: errMalformedINI},
		{name: "a repeated key", data: withChecksum(t, minimalINI+"db-id=1\n"), wantErr: errMalformedINI},
		{name: "a repeated key in a reopened section", data: withChecksum(t, minimalINI+"[x]\na=1\n[db]\ndb-id=2\n"), wantErr: errMalformedINI},
		{name: "a carriage return", data: bytes.ReplaceAll(withChecksum(t, minimalINI), []byte("\n"), []byte("\r\n")), wantErr: errMalformedINI},
		{name: "a NUL byte", data: withChecksum(t, minimalINI+"[x]\na=\"\x00\"\n"), wantErr: errMalformedINI},
		{name: "invalid UTF-8", data: withChecksum(t, minimalINI+"[x]\na=\"\xff\"\n"), wantErr: errMalformedINI},
		{name: "no format", data: withChecksum(t, "[backrest]\nbackrest-version=\"2.60.0\"\n"), wantErr: errMalformedINI},
		{name: "a format that is not a number", data: withChecksum(t, "[backrest]\nbackrest-format=\"5\"\n"), wantErr: errMalformedINI},
		{name: "a line over the bound", data: withChecksum(t, minimalINI+"[x]\na=\""+strings.Repeat("a", MaxINILineBytes)+"\"\n"), wantErr: errMalformedINI},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			file, err := ParseINI(bytes.NewReader(test.data), MaxInfoFileBytes, nil)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("ParseINI() error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseINI() error = %v", err)
			}
			if test.check != nil {
				test.check(t, file)
			}
		})
	}
}

func TestParseINIRefusesInputOverItsBound(t *testing.T) {
	t.Parallel()
	data := withChecksum(t, minimalINI)
	if _, err := ParseINI(bytes.NewReader(data), int64(len(data)), nil); err != nil {
		t.Fatalf("ParseINI() at the bound = %v", err)
	}
	if _, err := ParseINI(bytes.NewReader(data), int64(len(data)-1), nil); !errors.Is(err, errMalformedINI) || !errors.Is(err, errTooLarge) || problemOf(err) != ProblemTooLarge {
		t.Fatalf("ParseINI() over the bound = %v", err)
	}
	if _, err := ParseINI(bytes.NewReader(data), 0, nil); err == nil {
		t.Fatal("ParseINI() with no bound succeeded")
	}
}

func TestParseINIKeepsOnlyRequestedSectionsButHashesAll(t *testing.T) {
	t.Parallel()
	data := withChecksum(t, minimalINI+"[target:file]\npg_data/x={\"size\":1}\n")
	file, err := ParseINI(bytes.NewReader(data), MaxInfoFileBytes, func(section string) bool { return section == "db" })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := file.Get("target:file", "pg_data/x"); ok || len(file.Values) != 1 {
		t.Fatalf("values = %#v", file.Values)
	}
	tampered := bytes.Replace(data, []byte(`"size":1`), []byte(`"size":2`), 1)
	if _, err := ParseINI(bytes.NewReader(tampered), MaxInfoFileBytes, func(string) bool { return false }); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("a change in a skipped section was not detected: %v", err)
	}
}

// Every plain info file and manifest that real pgBackRest 2.60.0 wrote must
// verify, and a one-byte change anywhere in it must not.
func TestParseINIVerifiesEveryRealPlainFile(t *testing.T) {
	t.Parallel()
	var files []string
	for _, pattern := range []string{"*/*/*.info", "*/*/*.info.copy", "*/*/*/backup.manifest", "*/*/*/backup.manifest.copy"} {
		matches, err := filepath.Glob(filepath.Join(fixtureRoot, "plain", pattern))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	if len(files) != 12 {
		t.Fatalf("found %d plain metadata files, want 12", len(files))
	}
	for _, path := range files {
		t.Run(filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path), func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			file, err := ParseINI(bytes.NewReader(data), MaxManifestBytes, nil)
			if err != nil {
				t.Fatalf("ParseINI(%s) = %v", path, err)
			}
			if file.Format != 5 || file.Version != "2.60.0" {
				t.Fatalf("format %d version %q", file.Format, file.Version)
			}
			index := bytes.Index(data, []byte("\n[db]"))
			if index < 0 {
				index = bytes.Index(data, []byte("\n[backup:db]"))
			}
			tampered := bytes.Clone(data)
			tampered[index-2] ^= 1
			if _, err := ParseINI(bytes.NewReader(tampered), MaxManifestBytes, nil); err == nil {
				t.Fatal("a one-bit change verified")
			}
		})
	}
}

func TestAppendJSONStringEscapesLikePgBackRest(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"plain":      `"plain"`,
		"a\"b":       `"a\"b"`,
		`a\b`:        `"a\\b"`,
		"\n\r\t\b\f": `"\n\r\t\b\f"`,
		"\x01<>& é":  "\"\x01<>& é\"",
	} {
		if got := string(appendJSONString(nil, []byte(input))); got != want {
			t.Errorf("appendJSONString(%q) = %q, want %q", input, got, want)
		}
	}
}

// keyFlood streams a manifest whose [target:file] section, which
// ParseManifest hashes but does not keep, holds keys distinct keys: the shape
// of a cluster with that many files, or of a crafted file meant to exhaust
// the agent's memory.
type keyFlood struct {
	keys, next int
	pending    []byte
}

func (f *keyFlood) Read(buffer []byte) (int, error) {
	for len(f.pending) < len(buffer) && f.next <= f.keys {
		if f.next == 0 {
			f.pending = append(f.pending, "[backrest]\nbackrest-format=5\n[backup]\nbackup-label=\"x\"\n[target:file]\n"...)
		} else {
			f.pending = strconv.AppendInt(append(f.pending, "pg_data/base/1/"...), int64(f.next), 10)
			f.pending = append(f.pending, "={\"size\":8192,\"timestamp\":1700000000}\n"...)
		}
		f.next++
	}
	if len(f.pending) == 0 {
		return 0, io.EOF
	}
	written := copy(buffer, f.pending)
	f.pending = f.pending[:copy(f.pending, f.pending[written:])]
	return written, nil
}

// The reader's memory must not grow with the number of keys of a section it
// discards (a 167 MiB manifest drove the heap to 1.5 GiB
// against the agent's 512Mi limit). One million keys is about 64 MiB of input.
//
// Not parallel: testing.AllocsPerRun counts every allocation of the process.
func TestParseINIMemoryIsIndependentOfDiscardedKeys(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not those of the shipped build under the race detector")
	}
	const keys = 1_000_000
	allocations := testing.AllocsPerRun(1, func() {
		file, err := ParseINI(&keyFlood{keys: keys}, MaxManifestBytes, manifestSection)
		if !errors.Is(err, ErrChecksumMismatch) || file != nil {
			t.Errorf("ParseINI() = %v, want a checksum mismatch (the flood has none)", err)
		}
	})
	if allocations > 200 {
		t.Fatalf("ParseINI allocated %.0f times for %d discarded keys, want a constant under 200", allocations, keys)
	}

	// A repeat in a discarded section is hashed, not refused; one in a kept
	// section still is.
	repeated := withChecksum(t, minimalINI+"[target:file]\npg_data/x={}\npg_data/x={}\n")
	if _, err := ParseINI(bytes.NewReader(repeated), MaxInfoFileBytes, func(section string) bool { return section == "db" }); err != nil {
		t.Fatalf("a repeat in a discarded section: %v", err)
	}
	if _, err := ParseINI(bytes.NewReader(repeated), MaxInfoFileBytes, nil); !errors.Is(err, errMalformedINI) {
		t.Fatalf("a repeat in a kept section: %v", err)
	}
	twice := withChecksum(t, minimalINI+"[backrest]\nbackrest-format=5\n")
	if _, err := ParseINI(bytes.NewReader(twice), MaxInfoFileBytes, func(string) bool { return false }); !errors.Is(err, errMalformedINI) {
		t.Fatalf("a repeated backrest-format: %v", err)
	}
}
