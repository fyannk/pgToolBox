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
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
)

// commentPad returns history comment lines of exactly size bytes.
func commentPad(size int) string {
	var builder strings.Builder
	for size > 0 {
		line := min(size, 1024)
		if size-line == 1 {
			line-- // keep every line at least "#\n"
		}
		builder.WriteString("#" + strings.Repeat("x", line-2) + "\n")
		size -= line
	}
	return builder.String()
}

// iniPad returns [section] key="value" lines of exactly size bytes, every
// key distinct, so a strict INI reader reads to the end before it can judge.
func iniPad(size int) string {
	var builder strings.Builder
	for index := 0; size > 0; index++ {
		key := fmt.Sprintf("k%07d=\"", index)
		line := min(size, 1024)
		if size-line < len(key)+2 && size-line > 0 {
			line = size - (len(key) + 2)
		}
		builder.WriteString(key + strings.Repeat("x", line-len(key)-2) + "\"\n")
		size -= line
	}
	return builder.String()
}

// truncationTrap is a plaintext whose first limit bytes are acceptable on
// their own and whose next block is a whole block of PKCS#7 padding: an
// encrypted copy cut at limit+32 bytes (header, limit, one block) decrypts
// cleanly to exactly the first limit bytes. Only a reader that asks for one
// byte more than it accepts can tell the file is larger.
func truncationTrap(prefix []byte, limit int64, pad func(int) string) []byte {
	plain := append(bytes.Clone(prefix), pad(int(limit)-len(prefix))...)
	plain = append(plain, bytes.Repeat([]byte{0x10}, 16)...)
	return append(plain, "2\t0/F000000\tlater\n"...)
}

// servingFake serves body for every key through storetest.Fake, which cuts
// it at MaxBytes like the provider adapters, and records the MaxBytes asked.
func servingFake(body []byte, asked *atomic.Int64) store.Reader {
	return &storetest.Fake{OpenFunc: func(_ context.Context, request store.OpenRequest) (io.ReadCloser, error) {
		asked.Store(request.MaxBytes)
		return io.NopCloser(bytes.NewReader(body)), nil
	}}
}

func TestHistoryReadsAskOneByteOverTheLimit(t *testing.T) {
	t.Parallel()
	passphrase := []byte("history-cipher-pass")
	salt := []byte("saltsalt")
	history := []byte("1\t0/E000050\tno recovery target specified\n")
	stored := MaxHistoryBytes + int64(cipherHeaderBytes) + 16
	object := HistoryObject{Key: "archive/demo/17-1/00000002.history", Timeline: 2} // listed size understated (0)

	var asked atomic.Int64
	atLimit := encryptForTest(t, append(bytes.Clone(history), commentPad(int(MaxHistoryBytes)-len(history))...), passphrase, salt)
	if int64(len(atLimit)) != stored {
		t.Fatalf("the at-limit file is %d bytes, want %d", len(atLimit), stored)
	}
	data, err := readHistory(context.Background(), servingFake(atLimit, &asked), object, passphrase)
	if err != nil || int64(len(data)) != MaxHistoryBytes {
		t.Fatalf("a history at the limit: %d bytes, %v", len(data), err)
	}
	if asked.Load() != stored+1 {
		t.Fatalf("Open MaxBytes = %d, want %d", asked.Load(), stored+1)
	}
	if _, err := ParseHistory(data, 2, 16<<20); err != nil {
		t.Fatalf("the at-limit history does not parse: %v", err)
	}

	trap := encryptForTest(t, truncationTrap(history, MaxHistoryBytes, commentPad), passphrase, salt)
	if _, err := readHistory(context.Background(), servingFake(trap, &asked), object, passphrase); !errors.Is(err, errTooLarge) {
		t.Fatalf("an encrypted history over the limit was read: %v", err)
	}
	plainOver := truncationTrap(history, stored, commentPad)
	if _, err := readHistory(context.Background(), servingFake(plainOver, &asked), object, nil); !errors.Is(err, errTooLarge) {
		t.Fatalf("a plain history over the limit was read: %v", err)
	}
}

func TestInfoReadsAskOneByteOverTheLimit(t *testing.T) {
	t.Parallel()
	passphrase := []byte("repository-passphrase")
	key := "backup/demo/backup.info"
	listed := map[string]store.Object{key: {Key: key}} // listed size understated (0)
	stored := MaxInfoFileBytes + int64(cipherHeaderBytes) + 16

	var asked atomic.Int64
	trap := encryptForTest(t, truncationTrap([]byte("[backrest]\n"), MaxInfoFileBytes, iniPad), passphrase, []byte("saltsalt"))
	_, pair := LoadBackupInfo(context.Background(), servingFake(trap, &asked), listed, "demo", passphrase)
	if asked.Load() != stored+1 {
		t.Fatalf("Open MaxBytes = %d, want %d", asked.Load(), stored+1)
	}
	if !pair.Main.Present || pair.Main.Problem != ProblemTooLarge {
		t.Fatalf("an encrypted backup.info over the limit: %+v", pair.Main)
	}
}
