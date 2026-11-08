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

package viewmodel_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel/fixtures"
)

func TestAppendJSON(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		v    any
		want string
	}{
		"html left alone":     {map[string]string{"a": "<b>&</b>"}, `{"a":"<b>&</b>"}`},
		"line separators":     {"\u2028\u2029", `"\u2028\u2029"`},
		"invalid utf-8":       {"\xff", `"` + "\ufffd" + `"`},
		"time":                {fixtures.Now, `"2026-10-29T12:00:00Z"`},
		"nil slice":           {[]string(nil), `null`},
		"appends, no newline": {1, `1`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			buf.WriteString("x")
			if err := viewmodel.AppendJSON(&buf, tc.v); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != "x"+tc.want {
				t.Fatalf("got %s, want x%s", got, tc.want)
			}
		})
	}
	if err := viewmodel.AppendJSON(&bytes.Buffer{}, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("a year past 9999 encoded")
	}
}

func TestRowWithJSON(t *testing.T) {
	t.Parallel()
	row := fixtures.FailingOverRow()
	if row.EncodedJSON() != nil {
		t.Fatal("a built row carries an encoding")
	}
	sealed := row.WithJSON()
	var enc bytes.Buffer
	e := json.NewEncoder(&enc)
	e.SetEscapeHTML(false) // as the API's format
	if err := e.Encode(row); err != nil {
		t.Fatal(err)
	}
	want := bytes.TrimSuffix(enc.Bytes(), []byte("\n"))
	if !bytes.Equal(sealed.EncodedJSON(), want) {
		t.Fatalf("encoding = %s, want %s", sealed.EncodedJSON(), want)
	}
	if !bytes.Equal(sealed.WithJSON().EncodedJSON(), want) {
		t.Fatal("re-encoding a sealed row changed its bytes (the old encoding leaked in)")
	}
	plain, _ := json.Marshal(row)
	reencoded, err := json.Marshal(sealed)
	if err != nil || !bytes.Equal(reencoded, plain) {
		t.Fatalf("encoding/json sees the attached encoding: %s", reencoded)
	}
	if sealed.WithoutJSON().EncodedJSON() != nil {
		t.Fatal("WithoutJSON kept the encoding")
	}
	if row.EncodedJSON() != nil {
		t.Fatal("WithJSON changed the receiver")
	}
	bad := row
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	bad.Phase.Since = &far
	if bad.WithJSON().EncodedJSON() != nil {
		t.Fatal("a row that cannot be encoded carries an encoding")
	}
}
