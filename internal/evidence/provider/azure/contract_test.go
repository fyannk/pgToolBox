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

package azure

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/fyannk/pgToolBox/internal/evidence/provider/cursor"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
)

func TestAzureSharedReaderContract(t *testing.T) {
	storetest.ReaderContract(t, func(t *testing.T) store.Reader {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("comp") {
			case "list":
				w.Header().Set("Content-Type", "application/xml")
				_, _ = w.Write([]byte(`<?xml version="1.0"?><EnumerationResults><Blobs><Blob><Name>root/value</Name><Properties><Content-Length>3</Content-Length></Properties></Blob></Blobs><NextMarker/></EnumerationResults>`))
			case "properties":
				w.Header().Set("Content-Length", "3")
			default:
				_, _ = w.Write([]byte("abc"))
			}
		}))
		t.Cleanup(server.Close)
		client, err := azblob.NewClientWithNoCredential(server.URL+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		codec, err := cursor.New()
		if err != nil {
			t.Fatal(err)
		}
		return &Store{client: client, container: "container", prefix: "root/", timeout: time.Second, cursor: codec}
	})
}
