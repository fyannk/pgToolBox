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

//go:build integration

package azure

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"

	"github.com/fyannk/pgToolBox/internal/evidence/provider/cursor"
	"github.com/fyannk/pgToolBox/internal/evidence/provider/providertest"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

func TestAzuriteJourneyWithReadOnlySAS(t *testing.T) {
	ctx := context.Background()
	endpoint := os.Getenv("AZURITE_BLOB_ENDPOINT")
	if endpoint == "" {
		t.Skip("AZURITE_BLOB_ENDPOINT is required")
	}
	developmentConnection := fmt.Sprintf("DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=%s;", endpoint)
	admin, err := azblob.NewClientFromConnectionString(developmentConnection, nil)
	if err != nil {
		t.Fatal(err)
	}
	container := "evidence-agent-proof"
	_, _ = admin.DeleteContainer(ctx, container, nil)
	if _, err = admin.CreateContainer(ctx, container, nil); err != nil {
		t.Fatal(err)
	}
	for _, object := range providertest.BarmanFixture(t) {
		if _, err = admin.UploadBuffer(ctx, container, "repository/"+object.Key, object.Data, nil); err != nil {
			t.Fatal(err)
		}
	}
	readURL, err := admin.ServiceClient().GetSASURL(
		sas.AccountResourceTypes{Service: true, Container: true, Object: true},
		sas.AccountPermissions{Read: true, List: true}, time.Now().UTC().Add(time.Hour), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	readClient, err := azblob.NewClientWithNoCredential(readURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := cursor.New()
	if err != nil {
		t.Fatal(err)
	}
	reader := &Store{client: readClient, container: container, prefix: "repository/", timeout: 5 * time.Second, cursor: codec}
	first, err := reader.List(ctx, store.ListRequest{Limit: 1})
	if err != nil || len(first.Objects) != 1 || first.NextCursor == "" {
		t.Fatalf("first paginated List() = %#v, %v", first, err)
	}
	second, err := reader.List(ctx, store.ListRequest{Limit: 1, Cursor: first.NextCursor})
	if err != nil || len(second.Objects) != 1 || second.Objects[0].Key == first.Objects[0].Key {
		t.Fatalf("second paginated List() = %#v, %v", second, err)
	}
	normalized := providertest.BarmanCatalogJourney(t, reader)
	providertest.WriteJourneyResult(t, normalized)
	if _, err = readClient.UploadBuffer(ctx, container, "must-be-denied", []byte("x"), nil); err == nil {
		t.Fatal("read-only SAS unexpectedly allowed upload")
	}
}
