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
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/iotest"
)

// encryptForTest is OpenSSL's `enc -aes-256-cbc -md sha1 -salt`, which is
// what pgBackRest writes at repository format 5.
func encryptForTest(t testing.TB, plain, passphrase, salt []byte) []byte {
	t.Helper()
	key, iv := bytesToKey(passphrase, salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	padding := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(bytes.Clone(plain), bytes.Repeat([]byte{byte(padding)}, padding)...)
	sealed := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(sealed, padded)
	return append(append([]byte(saltedMagic), salt...), sealed...)
}

func TestDecryptOpensEveryRealEncryptedInfoFile(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"backup/demo/backup.info", "backup/demo/backup.info.copy", "archive/demo/archive.info", "archive/demo/archive.info.copy"} {
		data, err := os.ReadFile(filepath.Join(fixtureRoot, "encrypted", key))
		if err != nil {
			t.Fatal(err)
		}
		if DetectEnvelope(data) != EnvelopeSalted {
			t.Fatalf("%s is not salted", key)
		}
		plain, err := Decrypt(data, []byte(fixturePassphrase))
		if err != nil {
			t.Fatalf("Decrypt(%s) = %v", key, err)
		}
		if _, err := ParseINI(bytes.NewReader(plain), MaxInfoFileBytes, nil); err != nil {
			t.Fatalf("decrypted %s does not verify: %v", key, err)
		}
		// One byte at a time, as a slow provider returns them.
		reader, err := NewDecryptReader(iotest.OneByteReader(bytes.NewReader(data)), []byte(fixturePassphrase))
		if err != nil {
			t.Fatal(err)
		}
		streamed, err := io.ReadAll(reader)
		if err != nil || !bytes.Equal(streamed, plain) {
			t.Fatalf("streamed decryption of %s differs: %v", key, err)
		}
	}
}

func TestDecryptRoundTripsEveryLength(t *testing.T) {
	t.Parallel()
	passphrase, salt := []byte("test-only"), []byte("12345678")
	for length := 0; length <= 3*aes.BlockSize+1; length++ {
		plain := bytes.Repeat([]byte{'x'}, length)
		got, err := Decrypt(encryptForTest(t, plain, passphrase, salt), passphrase)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("length %d: Decrypt() = %q, %v", length, got, err)
		}
	}
}

func TestDecryptRefusesWhatDoesNotOpen(t *testing.T) {
	t.Parallel()
	passphrase, salt := []byte("test-only"), []byte("12345678")
	valid := encryptForTest(t, []byte("[backrest]\n"), passphrase, salt)
	formatSix := append([]byte("PGBR006_"), valid[8:]...)
	tests := []struct {
		name       string
		data       []byte
		passphrase []byte
		want       error
	}{
		{name: "wrong passphrase", data: encryptForTest(t, bytes.Repeat([]byte{'y'}, 64), passphrase, salt), passphrase: []byte("other"), want: ErrDecryption},
		{name: "no passphrase", data: valid, want: ErrDecryption},
		{name: "plain text", data: []byte("[backrest]\nbackrest-format=5\n"), passphrase: passphrase, want: ErrDecryption},
		{name: "header only", data: valid[:cipherHeaderBytes], passphrase: passphrase, want: ErrDecryption},
		{name: "short header", data: valid[:10], passphrase: passphrase, want: ErrDecryption},
		{name: "not whole blocks", data: valid[:len(valid)-1], passphrase: passphrase, want: ErrDecryption},
		{name: "format 6 header", data: formatSix, passphrase: passphrase, want: ErrRepositoryFormatUnsupported},
		{name: "format header with a key id marker", data: append([]byte("PGBR006K"), valid[8:]...), passphrase: passphrase, want: ErrRepositoryFormatUnsupported},
		{name: "oversized passphrase", data: valid, passphrase: bytes.Repeat([]byte{'p'}, MaxPassphraseBytes+1), want: ErrDecryption},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Decrypt(test.data, test.passphrase); !errors.Is(err, test.want) {
				t.Fatalf("Decrypt() = %v, want %v", err, test.want)
			}
		})
	}
	var formatErr FormatError
	if _, err := Decrypt(formatSix, passphrase); !errors.As(err, &formatErr) || formatErr.Format != 6 {
		t.Fatalf("Decrypt(format 6) = %v", err)
	}
}

func TestUnpadAcceptsOnlyPKCS7(t *testing.T) {
	t.Parallel()
	for padding := 0; padding <= 255; padding++ {
		block := bytes.Repeat([]byte{byte(padding)}, aes.BlockSize)
		_, ok := unpad(block)
		if want := padding >= 1 && padding <= aes.BlockSize; ok != want {
			t.Fatalf("padding %d: ok = %v", padding, ok)
		}
	}
	block := bytes.Repeat([]byte{4}, aes.BlockSize)
	block[aes.BlockSize-3] = 5
	if _, ok := unpad(block); ok {
		t.Fatal("inconsistent padding bytes were accepted")
	}
	block = append(bytes.Repeat([]byte{'a'}, aes.BlockSize-2), 9, 2)
	if _, ok := unpad(block); ok {
		t.Fatal("padding 2 with one wrong byte was accepted")
	}
	if got, ok := unpad(append(bytes.Repeat([]byte{'a'}, aes.BlockSize-1), 1)); !ok || len(got) != aes.BlockSize-1 {
		t.Fatalf("unpad(1) = %q, %v", got, ok)
	}
}

func TestDetectEnvelopeOnRealFormatSixFiles(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"backup/demo/backup.info", "archive/demo/archive.info"} {
		data, err := os.ReadFile(filepath.Join(fixtureRoot, "format6", key))
		if err != nil {
			t.Fatal(err)
		}
		if DetectEnvelope(data) != EnvelopeFormatHeader || formatHeaderFormat(data) != 6 {
			t.Fatalf("%s: envelope %v format %d", key, DetectEnvelope(data), formatHeaderFormat(data))
		}
		var formatErr FormatError
		if _, err := Decrypt(data, []byte(fixturePassphrase)); !errors.As(err, &formatErr) || formatErr.Format != 6 {
			t.Fatalf("Decrypt(%s) = %v", key, err)
		}
	}
}

// BenchmarkDecryptReader streams 1 MiB through the decrypting reader: its
// buffers are reused from one 32 KiB read to the next, so the allocations
// do not grow with the size.
func BenchmarkDecryptReader(b *testing.B) {
	passphrase, salt := []byte("test-only"), []byte("12345678")
	data := encryptForTest(b, bytes.Repeat([]byte("pgbackrest "), 1<<20/11), passphrase, salt)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		reader, err := NewDecryptReader(bytes.NewReader(data), passphrase)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			b.Fatal(err)
		}
	}
}

// TestDecryptReaderAllocationsDoNotGrowWithSize pins the reuse.
func TestDecryptReaderAllocationsDoNotGrowWithSize(t *testing.T) {
	passphrase, salt := []byte("test-only"), []byte("12345678")
	allocations := func(size int) float64 {
		data := encryptForTest(t, bytes.Repeat([]byte{'x'}, size), passphrase, salt)
		return testing.AllocsPerRun(5, func() {
			reader, err := NewDecryptReader(bytes.NewReader(data), passphrase)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, reader); err != nil {
				t.Fatal(err)
			}
		})
	}
	small, large := allocations(64*1024), allocations(4<<20)
	if large > small+8 {
		t.Fatalf("%v allocations for 4 MiB, %v for 64 KiB: the reader allocates per read", large, small)
	}
}
