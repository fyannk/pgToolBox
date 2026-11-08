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

// Package nofollow is the HTTP client of every object-store provider: it
// never follows a redirect. A request to the object store carries what
// must reach that store only: an SSE-C customer key header, a shared-key
// or SigV4 signature, a bearer token. Go's client drops Authorization on a
// redirect to another host, and the AWS SDK's X-Amz-Security-Token, but
// neither drops a header such as x-amz-server-side-encryption-customer-key,
// and the AWS SDK follows 307 and 308 to any host. The object store has no
// reason to send the agent elsewhere, so a redirect is refused and reported.
package nofollow

import (
	"net/http"
)

// ErrRedirect is the error of a request the object store answered with a
// redirect. It names no URL: a Location may carry a signed query. It is
// not retried: azcore's retry policy honours NonRetriable, the AWS SDK's
// RetryableError.
var ErrRedirect error = redirectError{}

type redirectError struct{}

func (redirectError) Error() string {
	return "the object store answered with a redirect, which the agent does not follow"
}

// NonRetriable marks the error for azcore's retry policy.
func (redirectError) NonRetriable() {}

// RetryableError marks the error for the AWS SDK's retryer.
func (redirectError) RetryableError() bool { return false }

// Client returns an http.Client over transport that follows no redirect:
// a response with a redirect status and a Location fails with ErrRedirect
// before the client can follow it, and CheckRedirect keeps any other
// redirect response as the response.
func Client(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: refuse{next: transport},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Transport wraps transport for a layer that sits above it: the Cloud
// Storage client puts its oauth2 transport, which sets the bearer token on
// every request it sends (a followed redirect included), over this one.
// RoundTrip fails with ErrRedirect on a redirect, so whatever client sits
// above never sees one to follow.
func Transport(transport http.RoundTripper) http.RoundTripper {
	return refuse{next: transport}
}

type refuse struct{ next http.RoundTripper }

func (r refuse) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := r.next.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if redirect(response) {
		_ = response.Body.Close()
		return nil, ErrRedirect
	}
	return response, nil
}

// redirect is a status Go's client would follow, with somewhere to go: a
// 301 without a Location (S3's PermanentRedirect for a bucket in another
// region) is an answer, not a redirect, and stays one.
func redirect(response *http.Response) bool {
	switch response.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return response.Header.Get("Location") != ""
	default:
		return false
	}
}
