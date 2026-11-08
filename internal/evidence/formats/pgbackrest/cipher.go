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
	"crypto/sha1" // #nosec G505 -- EVP_BytesToKey with SHA-1 is how repository format 5 derives its key; reading it needs the same.
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
)

// The envelope of an encrypted repository file (src/common/crypto/cipherBlock.c
// and src/common/format/cipherBlockFormat.c in pgBackRest 2.60.0): eight bytes
// of magic, then an eight-byte salt, then AES-256-CBC with PKCS#7 padding.
const (
	// saltedMagic starts every file encrypted at repository format 5. It is
	// OpenSSL's, so `openssl enc -d -aes-256-cbc -md sha1` opens the file.
	saltedMagic = "Salted__"
	// formatHeaderMagic starts the header that replaces saltedMagic from
	// repository format 6: "PGBR", three format digits, and a marker ('_', or
	// 'K' when a key id follows).
	formatHeaderMagic = "PGBR"
	cipherSaltBytes   = 8
	cipherHeaderBytes = len(saltedMagic) + cipherSaltBytes
	cipherKeyBytes    = 32
	// MaxPassphraseBytes bounds a passphrase; pgBackRest generates 64-byte
	// sub-passphrases and an operator's repository passphrase is far shorter.
	MaxPassphraseBytes = 4096
)

// ErrRepositoryFormatUnsupported reports a repository written at a format this
// reader does not read: anything but 5. Format 6 is opt-in in pgBackRest
// 2.60.0 (repo-format=6); it changes the key derivation to SHA-256 and adds
// key rotation, and is detected rather than guessed at.
var ErrRepositoryFormatUnsupported = errors.New("pgbackrest: repository format is not supported")

// ErrDecryption reports an encrypted file that does not open with the
// passphrase given: a wrong passphrase, a corrupt file, or a file that is not
// encrypted at all. The causes are deliberately not told apart.
var ErrDecryption = errors.New("pgbackrest: file does not decrypt with the configured passphrase")

// FormatError is ErrRepositoryFormatUnsupported with the format that was found.
type FormatError struct {
	Format int
}

func (e FormatError) Error() string {
	return fmt.Sprintf("pgbackrest: repository format %d is not supported (only format 5 is read)", e.Format)
}

// Is makes errors.Is(err, ErrRepositoryFormatUnsupported) hold.
func (FormatError) Is(target error) bool { return target == ErrRepositoryFormatUnsupported }

// Envelope is what the first bytes of a stored repository file say it is.
type Envelope int

const (
	// EnvelopePlain is a file that is not encrypted (or not one pgBackRest
	// wrote).
	EnvelopePlain Envelope = iota
	// EnvelopeSalted is a file encrypted at repository format 5.
	EnvelopeSalted
	// EnvelopeFormatHeader is a file with a format header (format 6 or later).
	EnvelopeFormatHeader
)

// DetectEnvelope classifies a stored file from its first bytes.
func DetectEnvelope(prefix []byte) Envelope {
	switch {
	case bytes.HasPrefix(prefix, []byte(saltedMagic)):
		return EnvelopeSalted
	case bytes.HasPrefix(prefix, []byte(formatHeaderMagic)):
		return EnvelopeFormatHeader
	default:
		return EnvelopePlain
	}
}

// formatHeaderFormat reads the format digits of a format header, or -1.
func formatHeaderFormat(header []byte) int {
	if len(header) < len(formatHeaderMagic)+3 {
		return -1
	}
	format := 0
	for _, digit := range header[len(formatHeaderMagic) : len(formatHeaderMagic)+3] {
		if digit < '0' || digit > '9' {
			return -1
		}
		format = format*10 + int(digit-'0')
	}
	return format
}

// NewDecryptReader returns the plaintext of a file encrypted at repository
// format 5, decrypted as it is read: the key and IV come from OpenSSL's
// EVP_BytesToKey with SHA-1, one iteration, over passphrase and the salt; the
// cipher is AES-256-CBC; the PKCS#7 padding is checked in constant time on
// the last block and removed. A format header (format 6 and later) returns a
// FormatError before anything is decrypted. Any other failure, including a
// bad padding at the end of the stream, is ErrDecryption.
func NewDecryptReader(source io.Reader, passphrase []byte) (io.Reader, error) {
	if len(passphrase) == 0 || len(passphrase) > MaxPassphraseBytes {
		return nil, ErrDecryption
	}
	header := make([]byte, cipherHeaderBytes)
	if _, err := io.ReadFull(source, header); err != nil {
		return nil, ErrDecryption
	}
	switch DetectEnvelope(header) {
	case EnvelopeFormatHeader:
		return nil, FormatError{Format: formatHeaderFormat(header)}
	case EnvelopeSalted:
	default:
		return nil, ErrDecryption
	}
	key, iv := bytesToKey(passphrase, header[len(saltedMagic):])
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrDecryption
	}
	return &decryptReader{source: source, mode: cipher.NewCBCDecrypter(block, iv)}, nil
}

// Decrypt is NewDecryptReader over a bounded byte slice.
func Decrypt(data, passphrase []byte) ([]byte, error) {
	reader, err := NewDecryptReader(bytes.NewReader(data), passphrase)
	if err != nil {
		return nil, err
	}
	plain, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	return plain, nil
}

// bytesToKey is OpenSSL's EVP_BytesToKey(EVP_aes_256_cbc(), EVP_sha1(), salt,
// passphrase, count 1): D1 = SHA1(pass || salt), Di = SHA1(Di-1 || pass ||
// salt), concatenated until key (32 bytes) and IV (16 bytes) are filled.
//
// The weak key derivation is pgBackRest's, not a choice made here: every file
// of an encrypted repository at format 5 was written with exactly this KDF
// (cipherBlock.c, pgBackRest 2.60.0), so a reader that derived its key any
// other way could open none of them. Nothing derived here is stored or sent:
// the key opens one file in memory and is dropped. Format 6 (SHA-256) is
// refused with ErrRepositoryFormatUnsupported until it is read.
// (hack/security-accepted.txt, go/weak-sensitive-data-hashing.)
func bytesToKey(passphrase, salt []byte) ([]byte, []byte) {
	material := make([]byte, 0, cipherKeyBytes+aes.BlockSize+sha1.Size)
	var previous []byte
	for len(material) < cipherKeyBytes+aes.BlockSize {
		digest := sha1.New() // #nosec G401 -- see the import.
		_, _ = digest.Write(previous)
		_, _ = digest.Write(passphrase)
		_, _ = digest.Write(salt)
		previous = digest.Sum(nil)
		material = append(material, previous...)
	}
	return material[:cipherKeyBytes], material[cipherKeyBytes : cipherKeyBytes+aes.BlockSize]
}

// decryptReader decrypts CBC block by block and holds back the last block
// until the end of the stream, where its padding is checked and removed.
type decryptReader struct {
	source  io.Reader
	mode    cipher.BlockMode
	pending []byte // ciphertext not yet decrypted (less than one block, or the held-back block)
	plain   []byte // decrypted bytes ready to return
	held    []byte // the last decrypted block, released once more data follows
	// chunk is the read buffer and output the backing array of plain,
	// both reused: fill runs only once plain is drained.
	chunk, output []byte
	done          bool
	err           error
}

func (r *decryptReader) Read(buffer []byte) (int, error) {
	for len(r.plain) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.done {
			return 0, io.EOF
		}
		r.fill()
	}
	read := copy(buffer, r.plain)
	r.plain = r.plain[read:]
	return read, nil
}

// decryptChunkBytes is how much ciphertext one fill reads.
const decryptChunkBytes = 32 * 1024

func (r *decryptReader) fill() {
	if r.chunk == nil {
		r.chunk = make([]byte, decryptChunkBytes)
	}
	read, err := r.source.Read(r.chunk)
	r.pending = append(r.pending, r.chunk[:read]...)
	// plain is drained: its backing array is reused.
	r.plain = r.output[:0]
	whole := len(r.pending) / aes.BlockSize * aes.BlockSize
	if whole > 0 {
		// CBC decrypts in place (dst and src overlap exactly).
		r.mode.CryptBlocks(r.pending[:whole], r.pending[:whole])
		r.plain = append(r.plain, r.held...)
		r.plain = append(r.plain, r.pending[:whole-aes.BlockSize]...)
		r.held = append(r.held[:0], r.pending[whole-aes.BlockSize:whole]...)
		r.pending = append(r.pending[:0], r.pending[whole:]...)
	}
	defer func() { r.output = r.plain[:0] }()
	switch {
	case errors.Is(err, io.EOF):
		r.done = true
		if len(r.pending) != 0 || len(r.held) != aes.BlockSize {
			r.err = ErrDecryption
			return
		}
		unpadded, ok := unpad(r.held)
		if !ok {
			r.err = ErrDecryption
			return
		}
		r.plain = append(r.plain, unpadded...)
		r.held = nil
	case err != nil:
		r.err = err
	}
}

// unpad removes PKCS#7 padding from the last block. Every byte of the block is
// examined whatever the padding value, so the time taken does not tell a
// valid padding from an invalid one.
func unpad(block []byte) ([]byte, bool) {
	padByte := block[aes.BlockSize-1]
	padding := int(padByte)
	good := subtle.ConstantTimeLessOrEq(1, padding) & subtle.ConstantTimeLessOrEq(padding, aes.BlockSize)
	for index := 0; index < aes.BlockSize; index++ {
		inPadding := subtle.ConstantTimeLessOrEq(aes.BlockSize, index+padding)
		matches := subtle.ConstantTimeByteEq(block[index], padByte)
		good &= subtle.ConstantTimeSelect(inPadding, matches, 1)
	}
	if good != 1 {
		return nil, false
	}
	return block[:aes.BlockSize-padding], true
}
