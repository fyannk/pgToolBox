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

package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/fyannk/pgToolBox/internal/evidence/evidenceapi"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// SharedTokenUser is the audit name of a caller that presented the shared
// token: the agent knows no more about it than that it holds the Secret.
const SharedTokenUser = "shared-token"

// SharedTokenAuthenticator is AGENT_AUTH=sharedtoken: the
// pgtools chart runs the agent without the operator and without the
// cluster-scoped grant a TokenReview needs, so the console and the agent
// share one random token from a Secret both mount. It makes no API call.
//
// The file is re-read when it changes (stat-based, like the serving
// certificate), so a rotated Secret takes effect without a restart. A
// missing, empty or malformed file refuses every request until a valid one
// appears; it never takes the agent down, and unlike the certificate it
// never keeps the previous value: a rotation is usually a revocation.
type SharedTokenAuthenticator struct {
	path   string
	logger *slog.Logger

	mu sync.Mutex
	// stamped is false until the first stat; info is the file's stat, nil
	// while it is missing. The kubelet updates a Secret volume by swapping
	// a symlink, so a rotation shows up as another file, size or
	// modification time.
	stamped bool
	info    os.FileInfo
	// digest is the SHA-256 of the current token; valid is false while
	// the file is missing or malformed.
	digest [sha256.Size]byte
	valid  bool
}

func sameFile(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// NewSharedTokenAuthenticator checks the path's shape only: the file may
// appear after the agent starts (the Secret is created by the chart in the
// same release).
func NewSharedTokenAuthenticator(ctx context.Context, path string, logger *slog.Logger) (*SharedTokenAuthenticator, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("shared token authenticator configuration is invalid: the path must be absolute and clean")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	authenticator := &SharedTokenAuthenticator{path: path, logger: logger}
	authenticator.reload(ctx)
	return authenticator, nil
}

// Authenticate implements evidenceapi.Authenticator. The comparison is of
// two SHA-256 digests with subtle.ConstantTimeCompare: equal lengths, so the
// time taken says nothing about how much of the token matched.
func (a *SharedTokenAuthenticator) Authenticate(ctx context.Context, token evidencev1alpha1.BearerToken) evidenceapi.AuthDecision {
	if !token.Valid() {
		return evidenceapi.AuthDecision{Outcome: evidenceapi.AuthUnauthenticated}
	}
	expected, ok := a.reload(ctx)
	if !ok {
		// The agent's own configuration is missing, not the caller's
		// credential: a 503 the console retries, not a 401.
		return evidenceapi.AuthDecision{Outcome: evidenceapi.AuthUnavailable}
	}
	presented := sha256.Sum256([]byte(token.Value()))
	if subtle.ConstantTimeCompare(presented[:], expected[:]) != 1 {
		return evidenceapi.AuthDecision{Outcome: evidenceapi.AuthUnauthenticated}
	}
	return evidenceapi.AuthDecision{Outcome: evidenceapi.AuthAllowed, User: SharedTokenUser}
}

// reload stats the file and re-reads it when it changed; it returns the
// current digest and whether there is one. A stat per authenticated request
// costs microseconds and requests are bounded by the handler's concurrency
// limit, so there is no recheck interval: a rotation is seen on the next
// request. Transitions are logged, never the content.
func (a *SharedTokenAuthenticator) reload(ctx context.Context) ([sha256.Size]byte, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	info, err := os.Stat(a.path)
	if err != nil {
		info = nil
	}
	if a.stamped && sameFile(info, a.info) {
		return a.digest, a.valid
	}
	first, wasValid := !a.stamped, a.valid
	a.stamped, a.info = true, info
	token, err := evidencev1alpha1.ReadSharedTokenFile(a.path)
	if err != nil {
		a.digest, a.valid = [sha256.Size]byte{}, false
		if first || wasValid {
			a.logger.WarnContext(ctx, "the shared token file is missing, empty or malformed; every evidence request is refused until it is valid")
		}
		return a.digest, false
	}
	digest := sha256.Sum256([]byte(token.Value()))
	switch {
	case first || !wasValid:
		a.logger.InfoContext(ctx, "shared token loaded")
	case digest != a.digest:
		a.logger.InfoContext(ctx, "shared token rotated")
	}
	a.digest, a.valid = digest, true
	return a.digest, true
}

// SharedTokenSyntax is the Authorization parser that goes with this
// authenticator: an opaque token68, not a JWS.
func SharedTokenSyntax(header string) (evidencev1alpha1.BearerToken, error) {
	return evidencev1alpha1.ParseSharedAuthorization(header)
}

var _ evidenceapi.Authenticator = (*SharedTokenAuthenticator)(nil)
