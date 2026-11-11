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
	"io"
	"sort"
	"strings"
)

// The bundle's budgets, in one place. Build refuses a bundle over either, so
// `make docs-bundle` (and the image build) fails instead of shipping it.
const (
	// MaxEmbeddedBytes bounds the compressed archive, the bytes go:embed puts
	// in the console binary: it is what the documentation adds to the binary
	// and to the image.
	MaxEmbeddedBytes = 1 << 20
	// MaxBundleBytes bounds the archive once decompressed (the pages, the
	// index and their names): the console holds all of it in memory once it
	// is loaded, plus the decoded index, so it bounds the console's
	// memory, and it stops the documentation from growing unchecked behind
	// a good compression ratio.
	MaxBundleBytes = 4 << 20
	// readLimit is what Load decompresses at most before it refuses the
	// archive: a defence against a stream that inflates without end, twice
	// the build budget so that it never refuses a bundle Build accepted.
	readLimit = 2 * MaxBundleBytes
)

// archiveName is the one file of a bundle: a gzip stream of the container
// below, holding index.bin and pages/**.
const archiveName = "bundle.gz"

// archiveMagic starts the decompressed container. It is followed by the
// number of files, then each file as its name and its bytes, each a uvarint
// length and the bytes, in name order.
const archiveMagic = "PGTBDOCS1\n"

// checkBudget names the limit a bundle broke and by how much.
func checkBudget(compressed, decompressed int) error {
	if decompressed > MaxBundleBytes {
		return fmt.Errorf("docsbundle: the bundle is %d bytes decompressed, %d over the %d-byte (4 MiB) in-memory budget (MaxBundleBytes): shorten or split pages",
			decompressed, decompressed-MaxBundleBytes, MaxBundleBytes)
	}
	if compressed > MaxEmbeddedBytes {
		return fmt.Errorf("docsbundle: the bundle is %d bytes compressed, %d over the %d-byte (1 MiB) embedded budget (MaxEmbeddedBytes): shorten or split pages",
			compressed, compressed-MaxEmbeddedBytes, MaxEmbeddedBytes)
	}
	return nil
}

// pack writes files as one container and compresses it. The bytes are a pure
// function of files (for one Go toolchain, which mise.toml pins): the files go
// in name order and the gzip header is fixed, with no name, comment or
// modification time, at a fixed compression level.
func pack(files map[string][]byte) (archive []byte, decompressed int, err error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var raw bytes.Buffer
	raw.WriteString(archiveMagic)
	putI(&raw, len(names))
	for _, name := range names {
		putI(&raw, len(name))
		raw.WriteString(name)
		putI(&raw, len(files[name]))
		raw.Write(files[name])
	}
	var out bytes.Buffer
	zw, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return nil, 0, fmt.Errorf("docsbundle: %w", err)
	}
	// The zero Header: no name, no comment, ModTime zero (written as 0).
	if _, err := zw.Write(raw.Bytes()); err != nil {
		return nil, 0, fmt.Errorf("docsbundle: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, 0, fmt.Errorf("docsbundle: %w", err)
	}
	return out.Bytes(), raw.Len(), nil
}

// unpack decompresses an archive, readLimit bytes at most, and returns its
// files. The file contents are substrings of one string: the archive is
// decompressed once and held once.
func unpack(archive []byte) (map[string]string, int, error) {
	// bytes.Reader is a flate.Reader, so gzip reads no further than its
	// member and what is left after it is exactly the trailing garbage.
	br := bytes.NewReader(archive)
	zr, err := gzip.NewReader(br)
	if err != nil {
		return nil, 0, fmt.Errorf("docsbundle: archive: %w", err)
	}
	zr.Multistream(false)
	raw, err := io.ReadAll(io.LimitReader(zr, readLimit+1))
	if err != nil {
		return nil, 0, fmt.Errorf("docsbundle: archive: %w", err)
	}
	if len(raw) > readLimit {
		return nil, 0, fmt.Errorf("docsbundle: archive: decompresses to more than %d bytes", readLimit)
	}
	if err := zr.Close(); err != nil {
		return nil, 0, fmt.Errorf("docsbundle: archive: %w", err)
	}
	if br.Len() != 0 {
		return nil, 0, errors.New("docsbundle: archive: trailing bytes after the gzip stream")
	}
	if !bytes.HasPrefix(raw, []byte(archiveMagic)) {
		return nil, 0, errors.New("docsbundle: archive: not a docs bundle")
	}
	all := string(raw[len(archiveMagic):])
	r := &reader{b: raw[len(archiveMagic):]}
	at := func() int { return len(all) - len(r.b) }
	files := map[string]string{}
	prev := ""
	for i, n := 0, r.int(); i < n && r.err == nil; i++ {
		name := r.s()
		size := r.int()
		if r.err != nil {
			break
		}
		if size > len(r.b) {
			return nil, 0, fmt.Errorf("docsbundle: archive: %s is truncated", name)
		}
		if name == "" || (i > 0 && name <= prev) {
			return nil, 0, fmt.Errorf("docsbundle: archive: file %q is out of order or repeated", name)
		}
		if name != "index.bin" && !strings.HasPrefix(name, "pages/") {
			return nil, 0, fmt.Errorf("docsbundle: archive: unexpected file %q", name)
		}
		start := at()
		files[name] = all[start : start+size]
		r.b = r.b[size:]
		prev = name
	}
	if r.err != nil {
		return nil, 0, fmt.Errorf("docsbundle: archive: %w", r.err)
	}
	if len(r.b) != 0 {
		return nil, 0, errors.New("docsbundle: archive: trailing bytes")
	}
	if _, ok := files["index.bin"]; !ok {
		return nil, 0, errors.New("docsbundle: archive: no index.bin")
	}
	return files, len(raw), nil
}
