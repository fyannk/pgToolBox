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
	"bufio"
	"bytes"
	"crypto/sha1" // #nosec G505 -- pgBackRest's info checksum is SHA-1; it is verified, never used for security.
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

// Bounds applied to every pgBackRest info file before and while parsing.
const (
	// MaxInfoFileBytes bounds backup.info and archive.info (decrypted). One
	// backup is about 1.2 KiB of backup.info, so this holds thousands.
	MaxInfoFileBytes int64 = 8 << 20
	// MaxManifestBytes bounds one backup.manifest (decrypted). Its size grows
	// with the number of files in the cluster: one [target:file] line of
	// about 100 to 130 bytes per file, so 256 MiB holds about two million
	// files (a cluster with many partitions reaches that). The whole file is
	// hashed for its checksum, streamed: only the [backup], [backup:db] and
	// [metadata] sections are kept, so the parser's memory does not depend on
	// this bound (a few MiB for a manifest at it, against the agent's default
	// 512Mi limit). What the bound costs is read time.
	MaxManifestBytes int64 = 256 << 20
	// MaxINILineBytes bounds one line of an info file or manifest.
	MaxINILineBytes = 256 * 1024
	// MaxINIValues bounds the values kept from one file.
	MaxINIValues = 100_000
)

// The [backrest] section and its keys, shared by every pgBackRest info file.
const (
	sectionBackrest = "backrest"
	keyChecksum     = "backrest-checksum"
	keyFormat       = "backrest-format"
	keyVersion      = "backrest-version"
)

// ErrChecksumMismatch reports an info file or manifest whose content does not
// match its [backrest] backrest-checksum (or that has none).
var ErrChecksumMismatch = errors.New("pgbackrest: info checksum does not match its content")

// errMalformedINI reports a file pgBackRest's strict reader would refuse, or
// one this reader refuses on purpose (see ParseINI).
var errMalformedINI = errors.New("pgbackrest: malformed info file")

// INIValue is one key=value line. Raw is the value exactly as written: the
// JSON text that the checksum covers.
type INIValue struct {
	Section, Key, Raw string
}

// INIFile is a parsed and checksum-verified pgBackRest info file: its
// repository format, the pgBackRest version that wrote it, its checksum, and
// the values of the sections the caller kept, in file order.
type INIFile struct {
	Format   int
	Version  string
	Checksum string
	Values   []INIValue
	index    map[string]map[string]int
}

// Get returns the raw JSON value of key in section.
func (f *INIFile) Get(section, key string) (string, bool) {
	keys, ok := f.index[section]
	if !ok {
		return "", false
	}
	position, ok := keys[key]
	if !ok {
		return "", false
	}
	return f.Values[position].Raw, true
}

// Section returns the values of one section in file order.
func (f *INIFile) Section(name string) []INIValue {
	result := make([]INIValue, 0, len(f.index[name]))
	for _, value := range f.Values {
		if value.Section == name {
			result = append(result, value)
		}
	}
	return result
}

// ParseINI reads a pgBackRest info file (backup.info, archive.info,
// backup.manifest) the way pgBackRest 2.60.0's strict reader does
// (src/common/ini.c with .strict, src/info/info.c infoNewLoad) and verifies
// its checksum:
//
//   - lines are split on '\n' and never trimmed; empty lines are skipped;
//     there are no comments;
//   - a line that starts with '[' and ends with ']' opens a section, which
//     must not be empty; a section may be opened again later (pgBackRest
//     itself writes [backrest] twice: the format first, the checksum last);
//   - any other line is key=value, inside a section, with a non-empty key and
//     a value that is valid JSON; when the value after the first '=' is not
//     JSON the split moves to the next '=' (a key may contain '=');
//   - the checksum is SHA-1 over the canonical JSON pgBackRest builds while
//     reading: {"section":{"key":value,...},...} with sections in file order,
//     the key JSON-escaped as pgBackRest escapes, the value as raw text, and
//     [backrest] backrest-checksum left out.
//
// The reader is stricter than pgBackRest where pgBackRest never writes the
// input: a repeated key in a section the caller keeps (or a repeated
// format, version or checksum in [backrest]), a NUL or '\r' byte, a file that
// is not UTF-8 and a file over limit bytes are refused rather than
// interpreted. Values of sections for which keep returns false are hashed but
// neither kept nor checked for repeats: a manifest's file list is most of the
// file, and remembering its keys made the reader's memory grow with the
// number of files (a crafted 167 MiB manifest held 1.5 GiB). What the reader
// holds is bounded by MaxINIValues kept values and one MaxINILineBytes line,
// whatever the size of the file. [backrest] is always read. A checksum
// failure returns ErrChecksumMismatch.
func ParseINI(source io.Reader, limit int64, keep func(section string) bool) (*INIFile, error) {
	file, expected, actual, haveFormat, err := readINI(source, limit, keep)
	if err != nil {
		return nil, err
	}
	// pgBackRest verifies the checksum before it looks at the format, so a
	// file that is not an info file at all is a checksum failure.
	if expected == "" || expected != actual {
		return nil, ErrChecksumMismatch
	}
	if !haveFormat {
		return nil, fmt.Errorf("%w: no repository format", errMalformedINI)
	}
	file.Checksum = actual
	return file, nil
}

// INIChecksum returns the checksum pgBackRest computes for an info file or
// manifest, as ParseINI does, without comparing it with the file's own
// backrest-checksum (which may be absent or wrong). The file must otherwise
// be one ParseINI reads. It is for tests and test tooling that write info
// files; the agent never signs anything.
func INIChecksum(source io.Reader, limit int64) (string, error) {
	_, _, actual, _, err := readINI(source, limit, func(string) bool { return false })
	return actual, err
}

// readINI is ParseINI without the verdict: the file read so far, the
// checksum the file states, the one computed over its content and whether
// it has a repository format.
func readINI(source io.Reader, limit int64, keep func(section string) bool) (*INIFile, string, string, bool, error) {
	if limit <= 0 {
		return nil, "", "", false, errMalformedINI
	}
	lines := iniLineReader{reader: bufio.NewReaderSize(&boundedReader{source: source, remaining: limit}, 64*1024)}
	hash := sha1.New() // #nosec G401 -- see the import.
	_, _ = hash.Write([]byte{'{'})
	file := &INIFile{index: make(map[string]map[string]int)}
	var (
		section, hashSection string
		kept                 bool
		expected             string
		haveFormat           bool
		backrestSeen         [3]bool
		scratch              []byte
	)
	for {
		line, err := lines.next()
		if errors.Is(err, io.EOF) && line == nil {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			if errors.Is(err, errTooLarge) {
				return nil, "", "", false, fmt.Errorf("%w: %w: larger than %d bytes", errMalformedINI, errTooLarge, limit)
			}
			return nil, "", "", false, err
		}
		if len(line) == 0 {
			continue
		}
		if bytes.IndexByte(line, 0) >= 0 || bytes.IndexByte(line, '\r') >= 0 || !utf8.Valid(line) {
			return nil, "", "", false, fmt.Errorf("%w: invalid characters", errMalformedINI)
		}
		if line[0] == '[' && line[len(line)-1] == ']' {
			if len(line) == 2 {
				return nil, "", "", false, fmt.Errorf("%w: empty section", errMalformedINI)
			}
			if name := line[1 : len(line)-1]; section == "" || string(name) != section {
				section = string(name)
				kept = section != sectionBackrest && (keep == nil || keep(section))
			}
			continue
		}
		if section == "" {
			return nil, "", "", false, fmt.Errorf("%w: key/value outside a section", errMalformedINI)
		}
		key, raw, ok := splitINIValue(line)
		if !ok {
			return nil, "", "", false, fmt.Errorf("%w: no JSON value", errMalformedINI)
		}
		if len(key) == 0 {
			return nil, "", "", false, fmt.Errorf("%w: empty key", errMalformedINI)
		}

		if section == sectionBackrest {
			if slot := backrestSlot(key); slot >= 0 {
				if backrestSeen[slot] {
					return nil, "", "", false, fmt.Errorf("%w: repeated key", errMalformedINI)
				}
				backrestSeen[slot] = true
			}
		}
		if section == sectionBackrest && string(key) == keyChecksum {
			if err := json.Unmarshal(raw, &expected); err != nil || len(expected) != sha1.Size*2 {
				return nil, "", "", false, fmt.Errorf("%w: invalid checksum value", errMalformedINI)
			}
		} else {
			if hashSection == "" || hashSection != section {
				if hashSection != "" {
					_, _ = hash.Write([]byte("},"))
				}
				// The section name goes in as written, unlike a key:
				// pgBackRest's INFO_CHECKSUM_SECTION (src/info/info.c)
				// quotes it without JSON escaping, and INFO_CHECKSUM_KEY_VALUE
				// escapes the key (jsonFromVar). A section holding a quote or
				// a backslash hashes as pgBackRest hashes it.
				scratch = append(scratch[:0], '"')
				scratch = append(scratch, section...)
				scratch = append(scratch, `":{`...)
				_, _ = hash.Write(scratch)
				hashSection = section
			} else {
				_, _ = hash.Write([]byte{','})
			}
			scratch = appendJSONString(scratch[:0], key)
			scratch = append(scratch, ':')
			_, _ = hash.Write(scratch)
			_, _ = hash.Write(raw)
		}

		switch {
		case section == sectionBackrest && string(key) == keyFormat:
			format, convErr := strconv.Atoi(string(raw))
			if convErr != nil || format < 1 || format > 999 {
				return nil, "", "", false, fmt.Errorf("%w: invalid repository format", errMalformedINI)
			}
			file.Format, haveFormat = format, true
		case section == sectionBackrest && string(key) == keyVersion:
			if err := json.Unmarshal(raw, &file.Version); err != nil || !boundedText(file.Version) {
				return nil, "", "", false, fmt.Errorf("%w: invalid pgBackRest version", errMalformedINI)
			}
		}
		if kept {
			keys := file.index[section]
			if keys == nil {
				keys = make(map[string]int)
				file.index[section] = keys
			}
			if _, duplicate := keys[string(key)]; duplicate {
				return nil, "", "", false, fmt.Errorf("%w: repeated key", errMalformedINI)
			}
			if len(file.Values) >= MaxINIValues {
				return nil, "", "", false, fmt.Errorf("%w: more than %d values", errMalformedINI, MaxINIValues)
			}
			name := string(key)
			keys[name] = len(file.Values)
			file.Values = append(file.Values, INIValue{Section: section, Key: name, Raw: string(raw)})
		}
	}
	// pgBackRest closes the canonical JSON with "}}" even when no value
	// opened a section (INFO_CHECKSUM_END).
	_, _ = hash.Write([]byte("}}"))
	return file, expected, hex.EncodeToString(hash.Sum(nil)), haveFormat, nil
}

// backrestSlot numbers the [backrest] keys the reader interprets, so a
// repeat of one is refused without remembering any other key.
func backrestSlot(key []byte) int {
	switch string(key) {
	case keyFormat:
		return 0
	case keyVersion:
		return 1
	case keyChecksum:
		return 2
	default:
		return -1
	}
}

var errTooLarge = errors.New("pgbackrest: input exceeds its bound")

// boundedReader returns errTooLarge once more than remaining bytes were read,
// so a file over its bound is refused instead of being silently truncated.
type boundedReader struct {
	source    io.Reader
	remaining int64
}

func (r *boundedReader) Read(buffer []byte) (int, error) {
	if r.remaining < 0 {
		return 0, errTooLarge
	}
	if int64(len(buffer)) > r.remaining+1 {
		buffer = buffer[:r.remaining+1]
	}
	read, err := r.source.Read(buffer)
	r.remaining -= int64(read)
	if r.remaining < 0 {
		return read, errTooLarge
	}
	return read, err
}

// iniLineReader returns the next line without its '\n'; a '\r' before it
// is kept (and refused by the caller), as pgBackRest splits on '\n' only.
// The last line may end without a newline, as pgBackRest's reader allows; it
// is returned with io.EOF. The line is only valid until the next call: it
// points into the bufio buffer or into one reused buffer, so reading a file
// allocates nothing per line.
type iniLineReader struct {
	reader *bufio.Reader
	long   []byte
}

func (r *iniLineReader) next() ([]byte, error) {
	chunk, err := r.reader.ReadSlice('\n')
	if err == nil {
		if len(chunk) > MaxINILineBytes+1 {
			return nil, fmt.Errorf("%w: line longer than %d bytes", errMalformedINI, MaxINILineBytes)
		}
		return chunk[:len(chunk)-1], nil
	}
	r.long = append(r.long[:0], chunk...)
	line := r.long
	defer func() { r.long = line[:0] }()
	for {
		if len(line) > MaxINILineBytes+1 {
			return nil, fmt.Errorf("%w: line longer than %d bytes", errMalformedINI, MaxINILineBytes)
		}
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			chunk, err = r.reader.ReadSlice('\n')
			line = append(line, chunk...)
		case errors.Is(err, io.EOF):
			if len(line) == 0 {
				return nil, io.EOF
			}
			return line, io.EOF
		default:
			return nil, err
		}
	}
}

// splitINIValue splits key=value at the first '=' whose remainder is valid
// JSON, as pgBackRest's strict reader retries. pgBackRest also accepts raw
// control characters inside JSON strings, which encoding/json does not; such
// a value is refused here (pgBackRest never escapes them, so they would only
// appear in a manifest that names a file with a control character in it).
func splitINIValue(line []byte) ([]byte, []byte, bool) {
	for offset := 0; offset < len(line); {
		position := bytes.IndexByte(line[offset:], '=')
		if position < 0 {
			return nil, nil, false
		}
		position += offset
		value := line[position+1:]
		if json.Valid(value) {
			return line[:position], value, true
		}
		offset = position + 1
	}
	return nil, nil, false
}

// appendJSONString writes value as pgBackRest's jsonWriteStr does: only
// '"', '\\', '\n', '\r', '\t', '\b' and '\f' are escaped; every other byte,
// control characters and non-ASCII included, is written as is.
func appendJSONString(destination, value []byte) []byte {
	destination = append(destination, '"')
	for index := 0; index < len(value); index++ {
		switch character := value[index]; character {
		case '"':
			destination = append(destination, '\\', '"')
		case '\\':
			destination = append(destination, '\\', '\\')
		case '\n':
			destination = append(destination, '\\', 'n')
		case '\r':
			destination = append(destination, '\\', 'r')
		case '\t':
			destination = append(destination, '\\', 't')
		case '\b':
			destination = append(destination, '\\', 'b')
		case '\f':
			destination = append(destination, '\\', 'f')
		default:
			destination = append(destination, character)
		}
	}
	return append(destination, '"')
}
