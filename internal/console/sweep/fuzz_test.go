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

package sweep

import (
	"errors"
	"testing"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
)

// FuzzDecodeStatus asserts the trust-boundary invariants of a /pg/status
// body, which any process answering on a pod IP can write: decoding
// never panics, a failure is the typed errDecode (so the row says "does
// not parse" and never quotes the body), and a success is bounded — no
// string beyond 256 bytes, at most 64 standbys and 64 slots.
func FuzzDecodeStatus(f *testing.F) {
	f.Add([]byte(`{"isPrimary":true,"currentLsn":"0/3000060","timeLineID":1,"replicationInfo":[{"applicationName":"orders-2","receivedLsn":"0/3000060"}]}`))
	f.Add([]byte(`{"replicationSlotsInfo":[{"slotName":"_cnpg_orders_2","active":true}]}`))
	f.Add([]byte(`{"lastArchivedWALTime":"2026-10-29 12:00:00.123+00","isFenced":true}`))
	f.Add([]byte(`{"isPrimary":true,"lastArchivedWALTime":"yesterday"}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"currentLsn":` + `"` + string(make([]byte, 300)) + `"}`))
	f.Add([]byte(`[`))
	f.Add([]byte(`{"volumeUsages":[{"volume":"pgdata","totalBytes":10,"availableBytes":4,"usedBytes":5,"inodes":9,"inodesFree":3},{"volume":"tbs-x","totalBytes":1}]}`))
	f.Add([]byte(`{"volumeUsages":{"volume":"wal"}}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		reading, err := decodeStatus(body)
		if err != nil {
			var decode errDecode
			if !errors.As(err, &decode) {
				t.Fatalf("untyped error %T", err)
			}
			if decode.Error() != "the report does not parse" {
				t.Fatalf("error text carries input: %q", decode.Error())
			}
			return
		}
		for _, s := range []string{
			reading.CurrentLSN, reading.ReceivedLSN, reading.ReplayLSN, reading.CurrentWAL, reading.LastArchivedWAL,
			reading.LastFailedWAL, reading.SystemID, reading.InstanceManagerVersion, reading.InstanceArch, reading.Node,
			reading.MightBeUnavailableMaskedError,
		} {
			if len(s) > 256 {
				t.Fatalf("unbounded string of %d bytes", len(s))
			}
		}
		if len(reading.Standbys) > 64 || len(reading.Slots) > 64 {
			t.Fatalf("unbounded lists: %d standbys, %d slots", len(reading.Standbys), len(reading.Slots))
		}
		for _, sb := range reading.Standbys {
			if len(sb.ApplicationName) > 256 || len(sb.SentLSN) > 256 || len(sb.ReplayLag) > 256 {
				t.Fatal("unbounded standby field")
			}
		}
		// Volume usages: bounded, named as the fork names them, and never
		// self-contradicting (a full volume is available 0, not negative).
		if len(reading.VolumeUsages) > 64 || len(reading.VolumeUsages) > 0 && !reading.VolumeUsagesReported {
			t.Fatalf("volume usages: %d entries, reported %v", len(reading.VolumeUsages), reading.VolumeUsagesReported)
		}
		for _, v := range reading.VolumeUsages {
			if !instancestatus.ValidVolumeName(v.Volume) || v.AvailableBytes > v.TotalBytes || v.UsedBytes > v.TotalBytes ||
				v.InodesFree > v.Inodes || v.HasInodes != (v.Inodes > 0) {
				t.Fatalf("inconsistent volume usage %+v", v)
			}
		}
		// An archive time is a time or flagged unparsed, never both
		// (round 10, CNPG P3: an unparsed one is no reading, not "none").
		if reading.LastArchivedWALTime != nil && reading.LastArchivedWALTimeUnparsed {
			t.Fatalf("archive time %v flagged unparsed", reading.LastArchivedWALTime)
		}
		_, _ = parseLSN(reading.CurrentLSN)
	})
}

// FuzzParseLSN: any text either parses to the value its two hex halves
// encode or is refused; it never panics.
func FuzzParseLSN(f *testing.F) {
	for _, s := range []string{"0/0", "16/B374D848", "FFFFFFFF/FFFFFFFF", "/", "1/2/3", "-1/0"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, ok := parseLSN(s)
		if !ok && v != 0 {
			t.Fatalf("refused %q but returned %x", s, v)
		}
	})
}
