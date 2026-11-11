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

package docsbundle

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

var archiveSources = []Source{
	{Path: "/p/1/", File: "p/1.md", Title: "One", Markdown: "# One\n\n## A\n\nalpha beta\n\n## B\n\ngamma beta\n"},
	{Path: "/p/2/", File: "p/2.md", Title: "Two", Markdown: "# Two\n\n## C\n\nbeta délta\n"},
}

// The archive holds exactly the files buildFiles produced, byte for byte.
func TestArchiveRoundTrip(t *testing.T) {
	want, err := buildFiles("1.0.0", archiveSources)
	if err != nil {
		t.Fatal(err)
	}
	archive, size, err := pack(want)
	if err != nil {
		t.Fatal(err)
	}
	got, gotSize, err := unpack(archive)
	if err != nil {
		t.Fatal(err)
	}
	if gotSize != size {
		t.Errorf("decompressed size = %d, pack said %d", gotSize, size)
	}
	if !slices.Equal(slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want))) {
		t.Fatalf("files = %v, want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
	for name, data := range want {
		if got[name] != string(data) {
			t.Errorf("%s changed in the round trip", name)
		}
	}
	// And through the public path: the archive Build writes loads to the
	// same pages.
	files, err := Build("1.0.0", archiveSources)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(fstest.MapFS{archiveName: &fstest.MapFile{Data: files[archiveName]}})
	if err != nil {
		t.Fatal(err)
	}
	if b.DecompressedSize() != size {
		t.Errorf("DecompressedSize = %d, want %d", b.DecompressedSize(), size)
	}
	if d, ok := b.Page("/p/2/"); !ok || !strings.Contains(d.Markdown, "beta délta") {
		t.Errorf("Page = %+v, %v", d, ok)
	}
}

// Two builds of the same pages are the same bytes, and the gzip header names
// no file and no time.
func TestArchiveIsReproducible(t *testing.T) {
	first, err := Build("1.0.0", archiveSources)
	if err != nil {
		t.Fatal(err)
	}
	reversed := slices.Clone(archiveSources)
	slices.Reverse(reversed)
	second, err := Build("1.0.0", reversed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first[archiveName], second[archiveName]) {
		t.Fatal("two builds of the same pages differ")
	}
	zr, err := gzip.NewReader(bytes.NewReader(first[archiveName]))
	if err != nil {
		t.Fatal(err)
	}
	if zr.Name != "" || zr.Comment != "" || !zr.ModTime.IsZero() || zr.Extra != nil {
		t.Errorf("the gzip header must be fixed: %+v", zr.Header)
	}
}

func gz(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// container writes the decompressed container from name/content pairs as
// given, so a test can break the order or the lengths.
func container(pairs ...string) []byte {
	var b bytes.Buffer
	b.WriteString(archiveMagic)
	putI(&b, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		putI(&b, len(pairs[i]))
		b.WriteString(pairs[i])
		putI(&b, len(pairs[i+1]))
		b.WriteString(pairs[i+1])
	}
	return b.Bytes()
}

// A corrupt, truncated or malformed archive is an error, never a panic and
// never ErrNotEmbedded.
func TestCorruptArchivesAreRefused(t *testing.T) {
	files, err := Build("1.0.0", archiveSources)
	if err != nil {
		t.Fatal(err)
	}
	good := files[archiveName]
	flipped := slices.Clone(good)
	flipped[len(flipped)/2] ^= 0xff
	badCRC := slices.Clone(good)
	badCRC[len(badCRC)-6] ^= 0xff
	ix := string(fixtureIndex(t))
	cases := map[string][]byte{
		"empty":                 nil,
		"not gzip":              []byte("nope"),
		"gzip header only":      good[:10],
		"truncated by half":     good[:len(good)/2],
		"truncated trailer":     good[:len(good)-1],
		"a flipped byte":        flipped,
		"a bad checksum":        badCRC,
		"not a container":       gz(t, []byte("PGTBDOC1\nsomething else")),
		"no index":              gz(t, container("pages/a.md", "x")),
		"truncated file":        gz(t, container("index.bin", ix)[:len(archiveMagic)+20]),
		"out of order":          gz(t, container("pages/b.md", "x", "index.bin", ix)),
		"repeated file":         gz(t, container("index.bin", ix, "index.bin", ix)),
		"unexpected file":       gz(t, container("../etc/passwd", "x", "index.bin", ix)),
		"trailing bytes":        gz(t, append(container("index.bin", ix), 0)),
		"corrupt index inside":  gz(t, container("index.bin", "PGTBDOC1\n\xff")),
		"count past the end":    gz(t, []byte(archiveMagic+"\x05")),
		"length past the end":   gz(t, []byte(archiveMagic+"\x01\x09index.bin\xff\xff\x03")),
		"two concatenated gzip": append(slices.Clone(good), good...),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(fstest.MapFS{archiveName: &fstest.MapFile{Data: data}})
			t.Log(err)
			if err == nil || errors.Is(err, ErrNotEmbedded) {
				t.Errorf("Load = %v, want an error that is not ErrNotEmbedded", err)
			}
		})
	}
}

func fixtureIndex(t *testing.T) []byte {
	t.Helper()
	files, err := buildFiles("1.0.0", archiveSources)
	if err != nil {
		t.Fatal(err)
	}
	return files["index.bin"]
}

// A stream that inflates past readLimit is refused while it is read: Load
// never holds more than readLimit+1 decompressed bytes.
func TestOversizedDecompressionIsRefused(t *testing.T) {
	raw := append(container("index.bin", string(fixtureIndex(t))), make([]byte, readLimit)...)
	bomb := gz(t, raw)
	if len(bomb) > readLimit/100 {
		t.Fatalf("the test stream should be small compressed, it is %d bytes", len(bomb))
	}
	_, err := Load(fstest.MapFS{archiveName: &fstest.MapFile{Data: bomb}})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("more than %d bytes", readLimit)) {
		t.Errorf("Load = %v, want the decompression limit", err)
	}
}

// The budgets: the error names which limit was hit and by how much.
func TestBudget(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		compressed, decompressed int
		want                     string
	}{
		{"within both", MaxEmbeddedBytes, MaxBundleBytes, ""},
		{"compressed over", MaxEmbeddedBytes + 367, MaxBundleBytes, "367 over the 1048576-byte (1 MiB) embedded budget (MaxEmbeddedBytes)"},
		{"decompressed over", 100, MaxBundleBytes + 1, "1 over the 4194304-byte (4 MiB) in-memory budget (MaxBundleBytes)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkBudget(tc.compressed, tc.decompressed)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("checkBudget = %v, want nil", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("checkBudget = %v, want %q", err, tc.want)
			}
		})
	}
	if readLimit < MaxBundleBytes {
		t.Error("Load must accept every bundle Build accepts")
	}
	// Build enforces the in-memory budget: pages that compress well but
	// decompress past MaxBundleBytes are refused.
	big := strings.Repeat("word ", MaxBundleBytes/5+1)
	if _, err := Build("1.0.0", []Source{{Path: "/big/", File: "big.md", Title: "Big", Markdown: big}}); err == nil || !strings.Contains(err.Error(), "MaxBundleBytes") {
		t.Errorf("Build of an oversized bundle = %v, want the MaxBundleBytes error", err)
	}
}
