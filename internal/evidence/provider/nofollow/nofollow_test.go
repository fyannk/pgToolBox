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

package nofollow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestClientFollowsNoRedirect(t *testing.T) {
	t.Parallel()
	var elsewhere atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
	}))
	t.Cleanup(target.Close)
	for _, status := range []int{301, 302, 303, 307, 308} {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Redirect(w, &http.Request{}, target.URL+"/stolen", status)
		}))
		t.Cleanup(origin.Close)
		request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, origin.URL, http.NoBody)
		request.Header.Set("X-Amz-Server-Side-Encryption-Customer-Key", "c3NlLWMta2V5")
		response, err := Client(http.DefaultTransport).Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		if !errors.Is(err, ErrRedirect) {
			t.Errorf("status %d: error %v, want ErrRedirect", status, err)
		}
	}
	if elsewhere.Load() != 0 {
		t.Fatalf("the redirect target was reached %d times", elsewhere.Load())
	}
	// A 301 without a Location is an answer.
	answer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	t.Cleanup(answer.Close)
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, answer.URL, http.NoBody)
	response, err := Client(http.DefaultTransport).Do(request)
	if err != nil || response.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("a 301 without Location: %v, %v", response, err)
	}
	_ = response.Body.Close()
}
