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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// fuzzSeedFiles adds the committed info files and manifests of a variant.
func fuzzSeedFiles(f *testing.F, variant string, add func([]byte)) {
	f.Helper()
	paths, err := filepath.Glob(filepath.Join(fixtureRoot, variant, "*", "demo", "*.info*"))
	if err != nil {
		f.Fatal(err)
	}
	manifests, _ := filepath.Glob(filepath.Join(fixtureRoot, variant, "backup", "demo", "*", "backup.manifest"))
	for _, path := range append(paths, manifests...) {
		data, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		add(data)
	}
}

// The INI parser never panics; whatever it accepts is valid UTF-8 JSON
// values whose checksum it verified, and parsing is deterministic.
func FuzzParseINI(f *testing.F) {
	fuzzSeedFiles(f, "plain", func(data []byte) { f.Add(data) })
	f.Add([]byte("[backrest]\nbackrest-format=5\n"))
	f.Add([]byte("[a]\nk=\"v\"\n\n[backrest]\nbackrest-checksum=\"0\"\n"))
	f.Add([]byte("[a]\nk==\"=\"\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		file, err := ParseINI(bytes.NewReader(data), MaxInfoFileBytes, nil)
		if err != nil {
			if file != nil {
				t.Fatal("an error came with a file")
			}
			return
		}
		if file.Format < 1 || file.Format > 999 || len(file.Checksum) != 40 {
			t.Fatalf("accepted format %d, checksum %q", file.Format, file.Checksum)
		}
		for _, value := range file.Values {
			if value.Section == "" || value.Key == "" || !utf8.ValidString(value.Raw) || strings.ContainsAny(value.Key+value.Raw, "\x00\r\n") {
				t.Fatalf("accepted value %+v", value)
			}
			if raw, ok := file.Get(value.Section, value.Key); !ok || raw != value.Raw {
				t.Fatal("Get disagrees with Values")
			}
		}
		again, err := ParseINI(bytes.NewReader(data), MaxInfoFileBytes, nil)
		if err != nil || again.Checksum != file.Checksum || len(again.Values) != len(file.Values) {
			t.Fatal("parsing is not deterministic")
		}
	})
}

// The decryptor never panics, refuses wrong passphrases and corrupt
// envelopes with a typed error, and the stream and one-shot paths agree.
func FuzzDecrypt(f *testing.F) {
	for _, variant := range []string{"encrypted", "format6"} {
		fuzzSeedFiles(f, variant, func(data []byte) { f.Add(data, []byte(fixturePassphrase)) })
	}
	f.Add([]byte("Salted__12345678"), []byte("x"))
	f.Add([]byte("PGBR006_"), []byte("x"))
	f.Add([]byte("PGBR005K..."), []byte(""))
	f.Fuzz(func(t *testing.T, data, passphrase []byte) {
		plain, err := Decrypt(data, passphrase)
		stream, streamErr := NewDecryptReader(bytes.NewReader(data), passphrase)
		var streamed []byte
		if streamErr == nil {
			var buffer bytes.Buffer
			_, streamErr = buffer.ReadFrom(stream)
			streamed = buffer.Bytes()
		}
		if (err == nil) != (streamErr == nil) {
			t.Fatalf("Decrypt() = %v, stream = %v", err, streamErr)
		}
		if err != nil {
			if !errors.Is(err, ErrDecryption) && !errors.Is(err, ErrRepositoryFormatUnsupported) && !errors.Is(err, errTooLarge) {
				t.Fatalf("untyped error %v", err)
			}
			return
		}
		if !bytes.Equal(plain, streamed) {
			t.Fatal("stream and one-shot decryption disagree")
		}
		if DetectEnvelope(data) != EnvelopeSalted || len(data)-len(plain) < 17 || len(data)-len(plain) > 32 {
			t.Fatalf("decrypted %d bytes into %d", len(data), len(plain))
		}
	})
}

// A WAL name the parser accepts round-trips canonically; an archive key the
// classifier accepts as a segment names the same position.
func FuzzParseWALName(f *testing.F) {
	f.Add("000000010000000000000001", uint8(4), "archive/demo/17-1/0000000100000000/000000010000000000000001-0123456789abcdef0123456789abcdef01234567.gz")
	f.Add("000000020000000100000000", uint8(0), "archive/demo/17-1/00000002.history")
	f.Add("00000001000000000000003F", uint8(6), "archive/demo/17-1/0000000100000000/000000010000000000000002.00000028.backup")
	f.Add("not-a-wal", uint8(4), "archive/demo/17-1/0000000100000000/000000010000000000000003.partial-0123456789abcdef0123456789abcdef01234567")
	f.Fuzz(func(t *testing.T, name string, sizeCode uint8, key string) {
		segmentSize := MinWALSegmentSize << (sizeCode % 11)
		timeline, position, err := ParseWALName(name, segmentSize)
		if err == nil {
			canonical, err := WALName(timeline, position, segmentSize)
			if err != nil || canonical != name {
				t.Fatalf("%q did not round-trip: %q, %v", name, canonical, err)
			}
		}
		classified, belongs := ClassifyWALObject(store.Object{Key: key})
		if !belongs {
			return
		}
		if !strings.HasPrefix(key, "archive/"+classified.Stanza+"/"+classified.ArchiveID+"/") {
			t.Fatalf("classified %q under %s/%s", key, classified.Stanza, classified.ArchiveID)
		}
		if classified.Class == WALSegment || classified.Class == WALPartial {
			if len(classified.Name) != 24 || !strings.Contains(key, "/"+classified.Name) {
				t.Fatalf("segment %q named %q", key, classified.Name)
			}
			if _, err := WALPosition(classified.Timeline, classified.Log, classified.Segment, segmentSize); err == nil {
				if _, _, err := ParseWALName(classified.Name, segmentSize); err != nil {
					t.Fatalf("%q has a position but no name: %v", classified.Name, err)
				}
			}
		}
	})
}

// A history the parser accepts is the ancestry of target: strictly
// increasing parents below it and switchpoints, each child the next
// parent and the last child the target, and each switch's segment the one
// that holds its switchpoint (the failover rule's S = L / segsize).
func FuzzParseHistory(f *testing.F) {
	f.Add([]byte("1\t0/E000050\tno recovery target specified\n"), uint32(2), uint8(4))
	f.Add([]byte("1\t0/3000000\tbranch\n# a comment\n2\t1/0\tfailover\n"), uint32(3), uint8(4))
	f.Add([]byte("1\t0/0\tzero\n"), uint32(2), uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, target uint32, sizeCode uint8) {
		segmentSize := MinWALSegmentSize << (sizeCode % 11)
		edges, err := ParseHistory(data, target, segmentSize)
		if err != nil {
			return
		}
		if len(edges) == 0 || len(edges) > MaxHistoryEntries || edges[len(edges)-1].Child != target {
			t.Fatalf("edges %+v for target %d", edges, target)
		}
		for index, edge := range edges {
			// #nosec G115 -- a valid segment size.
			if edge.Parent >= edge.Child || edge.SwitchLSN == 0 || edge.SwitchPosition != edge.SwitchLSN/uint64(segmentSize) {
				t.Fatalf("edge %+v", edge)
			}
			if index > 0 && (edges[index-1].Child != edge.Parent || edges[index-1].SwitchLSN >= edge.SwitchLSN) {
				t.Fatalf("edges %+v are not one ancestry", edges)
			}
		}
	})
}

// FuzzParseLSN: an accepted LSN is %X/%X or PostgreSQL 19's %X/%08X, and
// normalizes to the one %X/%X spelling of the same position.
func FuzzParseLSN(f *testing.F) {
	for _, seed := range []string{"0/0", "0/8000028", "0/08000028", "FFFFFFFF/FFFFFFFF", "1/0000A0B0", "00/1", "0/0800028", "0/008000028", "/", "0/zz"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		lsn, ok := ParseLSN(value)
		normalized, okNormalized := normalizeLSN(value)
		if ok != okNormalized || ok != canonicalLSN(value) {
			t.Fatalf("%q: ParseLSN %v, normalizeLSN %v, canonicalLSN %v", value, ok, okNormalized, canonicalLSN(value))
		}
		if !ok {
			return
		}
		again, okAgain := ParseLSN(normalized)
		high, low, _ := strings.Cut(normalized, "/")
		if !okAgain || again != lsn || (len(high) > 1 && high[0] == '0') || (len(low) > 1 && low[0] == '0') {
			t.Fatalf("%q normalized to %q (%#x, %v), parsed %#x", value, normalized, again, okAgain, lsn)
		}
		if _, padded, _ := strings.Cut(value, "/"); padded != low && len(padded) != 8 {
			t.Fatalf("%q: a padded low half of %d digits was accepted", value, len(padded))
		}
	})
}
