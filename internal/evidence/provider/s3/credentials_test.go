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

package s3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/fyannk/pgToolBox/internal/evidence/fault"
)

// writeProjectedSecret lays out a directory the way the kubelet projects a
// Secret: the visible key names are symlinks through ..data into a versioned
// directory, and an update swaps ..data atomically.
func writeProjectedSecret(t *testing.T, root, version string, values map[string]string) {
	t.Helper()
	versioned := filepath.Join(root, version)
	if err := os.MkdirAll(versioned, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, value := range values {
		if err := os.WriteFile(filepath.Join(versioned, name), []byte(value), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	next := filepath.Join(root, "..data_tmp")
	if err := os.Symlink(version, next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, filepath.Join(root, "..data")); err != nil {
		t.Fatal(err)
	}
	for name := range values {
		link := filepath.Join(root, name)
		if _, err := os.Lstat(link); errors.Is(err, os.ErrNotExist) {
			if err := os.Symlink(filepath.Join("..data", name), link); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestFileCredentialsProviderReloadsRotatedProjectedSecret(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeProjectedSecret(t, root, "v1", map[string]string{"access": "AKIA-OLD\n", "secret": "old-secret\n", "token": "old-token"})
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	provider, err := newFileCredentialsProvider(filepath.Join(root, "access"), filepath.Join(root, "secret"), filepath.Join(root, "token"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	first, err := provider.Retrieve(context.Background())
	if err != nil || first.AccessKeyID != "AKIA-OLD" || first.SecretAccessKey != "old-secret" || first.SessionToken != "old-token" {
		t.Fatalf("first Retrieve = %#v, %v", first, err)
	}
	if !first.CanExpire || !first.Expires.Equal(now.Add(credentialRecheckInterval)) {
		t.Fatalf("credentials must expire at the recheck interval: %#v", first)
	}

	now = now.Add(credentialRecheckInterval)
	unchanged, err := provider.Retrieve(context.Background())
	if err != nil || unchanged.AccessKeyID != "AKIA-OLD" || !unchanged.Expires.Equal(now.Add(credentialRecheckInterval)) {
		t.Fatalf("unchanged Retrieve = %#v, %v", unchanged, err)
	}

	writeProjectedSecret(t, root, "v2", map[string]string{"access": "AKIA-NEW", "secret": "new-secret", "token": "new-token"})
	rotated, err := provider.Retrieve(context.Background())
	if err != nil || rotated.AccessKeyID != "AKIA-NEW" || rotated.SecretAccessKey != "new-secret" || rotated.SessionToken != "new-token" {
		t.Fatalf("rotated Retrieve = %#v, %v", rotated, err)
	}
}

func TestFileCredentialsProviderKeepsLastGoodKeysOnTransientFailure(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	access, secret := filepath.Join(directory, "access"), filepath.Join(directory, "secret")
	for path, value := range map[string]string{access: "AKIA-GOOD", secret: "good-secret"} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	provider, err := newFileCredentialsProvider(access, secret, "", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Retrieve(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(){
		"removed":   func() { _ = os.Remove(secret) },
		"truncated": func() { _ = os.WriteFile(secret, nil, 0o600) },
	} {
		mutate()
		kept, err := provider.Retrieve(context.Background())
		if err != nil || kept.AccessKeyID != "AKIA-GOOD" || kept.SecretAccessKey != "good-secret" {
			t.Fatalf("%s: Retrieve = %#v, %v; want last good keys", name, kept, err)
		}
	}
}

func TestFileCredentialsProviderFailsClosedWithoutDisclosure(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	access, secret := filepath.Join(directory, "access-canary"), filepath.Join(directory, "secret-canary")
	if err := os.WriteFile(access, []byte("AKIA-CANARY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte(strings.Repeat("s", maxCredentialFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	provider, err := newFileCredentialsProvider(access, secret, "", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Retrieve(context.Background())
	if fault.Categorize(err) != fault.Authentication || strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), directory) {
		t.Fatalf("Retrieve error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Retrieve(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Retrieve = %v", err)
	}
	for _, paths := range [][3]string{{"relative/access", secret, ""}, {access, "", ""}, {access, secret, directory + "/../x/token"}} {
		if _, err := newFileCredentialsProvider(paths[0], paths[1], paths[2], time.Now); fault.Categorize(err) != fault.InvalidConfig {
			t.Fatalf("newFileCredentialsProvider(%v) = %v", paths, err)
		}
	}
}

func TestFileCredentialsProviderWorksBehindTheSDKCredentialsCache(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeProjectedSecret(t, root, "v1", map[string]string{"access": "AKIA-ONE", "secret": "one"})
	provider, err := newFileCredentialsProvider(filepath.Join(root, "access"), filepath.Join(root, "secret"), "", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	cache := aws.NewCredentialsCache(provider)
	first, err := cache.Retrieve(context.Background())
	if err != nil || first.AccessKeyID != "AKIA-ONE" {
		t.Fatalf("cache Retrieve = %#v, %v", first, err)
	}
	writeProjectedSecret(t, root, "v2", map[string]string{"access": "AKIA-TWO", "secret": "two"})
	if cached, _ := cache.Retrieve(context.Background()); cached.AccessKeyID != "AKIA-ONE" {
		t.Fatal("cache re-read before expiry")
	}
	cache.Invalidate()
	second, err := cache.Retrieve(context.Background())
	if err != nil || second.AccessKeyID != "AKIA-TWO" {
		t.Fatalf("cache Retrieve after expiry = %#v, %v", second, err)
	}
}

func TestNewAcceptsOnlyCompleteCredentialFileSets(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	access, secret := filepath.Join(directory, "access"), filepath.Join(directory, "secret")
	for _, path := range []string{access, secret} {
		if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	valid := Options{Bucket: "synthetic-bucket", Region: "eu-west-1", RequestTimeout: time.Second, AccessKeyIDFile: access, SecretAccessKeyFile: secret}
	if adapter, err := New(context.Background(), valid); err != nil || adapter == nil {
		t.Fatalf("New() = %#v, %v", adapter, err)
	}
	for name, mutate := range map[string]func(*Options){
		"access only": func(options *Options) { options.SecretAccessKeyFile = "" },
		"token without access": func(options *Options) {
			options.AccessKeyIDFile, options.SecretAccessKeyFile, options.SessionTokenFile = "", "", access
		},
		"files and fixed keys": func(options *Options) { options.AccessKeyID, options.SecretAccessKey = []byte("a"), []byte("b") },
		"relative file":        func(options *Options) { options.AccessKeyIDFile = "access" },
	} {
		options := valid
		mutate(&options)
		if adapter, err := New(context.Background(), options); adapter != nil || fault.Categorize(err) != fault.InvalidConfig {
			t.Fatalf("%s: New() = %#v, %v", name, adapter, err)
		}
	}
}

// A Secret update that lands between reading the key files must not pair an
// old access key with a new secret: the read is retried once, and refused if
// the files keep changing.
func TestFileCredentialsProviderNeverMixesKeyGenerations(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeProjectedSecret(t, root, "v1", map[string]string{"access": "AKIA-ONE", "secret": "secret-one"})
	provider, err := newFileCredentialsProvider(filepath.Join(root, "access"), filepath.Join(root, "secret"), "", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var observed [][]byte
	provider.loaded = func(values ...[]byte) { observed = append(observed, values...) }
	swaps := 0
	provider.afterRead = func() {
		if swaps == 0 {
			writeProjectedSecret(t, root, "v2", map[string]string{"access": "AKIA-TWO", "secret": "secret-two"})
		}
		swaps++
	}
	credentials, err := provider.Retrieve(context.Background())
	if err != nil || credentials.AccessKeyID != "AKIA-TWO" || credentials.SecretAccessKey != "secret-two" || swaps != 2 {
		t.Fatalf("Retrieve across a swap = %#v, %v after %d reads", credentials, err, swaps)
	}
	if len(observed) == 0 || string(observed[0]) != "AKIA-TWO" || string(observed[1]) != "secret-two" {
		t.Fatalf("loaded callback saw %q", observed)
	}

	// Files that change on every read are refused; the last good keys stay.
	version := 3
	provider.afterRead = func() {
		writeProjectedSecret(t, root, fmt.Sprintf("v%d", version), map[string]string{"access": fmt.Sprintf("AKIA-%d", version), "secret": fmt.Sprintf("secret-%d", version)})
		version++
	}
	writeProjectedSecret(t, root, "v-changed", map[string]string{"access": "AKIA-CHANGED", "secret": "secret-changed"})
	kept, err := provider.Retrieve(context.Background())
	if err != nil || kept.AccessKeyID != "AKIA-TWO" || kept.SecretAccessKey != "secret-two" {
		t.Fatalf("unstable files = %#v, %v; want last consistent keys", kept, err)
	}
}

type rotatingKeys struct {
	access, secret string
	err            error
	calls          int
}

func (k *rotatingKeys) Keys(context.Context) ([]byte, []byte, []byte, error) {
	k.calls++
	if k.err != nil {
		return nil, nil, nil, k.err
	}
	return []byte(k.access), []byte(k.secret), nil, nil
}

func TestKeySourceProviderFollowsRotationAndKeepsLastGoodKeys(t *testing.T) {
	t.Parallel()
	source := &rotatingKeys{err: errors.New("secret minio-creds: forbidden")}
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	provider := newKeySourceProvider(source, func() time.Time { return now })
	var loaded [][]byte
	provider.loaded = func(values ...[]byte) { loaded = append(loaded, values...) }

	if _, err := provider.Retrieve(context.Background()); err == nil || strings.Contains(err.Error(), "minio") {
		t.Fatalf("no keys yet: err = %v (must fail closed without detail)", err)
	}
	source.err, source.access, source.secret = nil, "AKIA-OLD", "old-secret"
	first, err := provider.Retrieve(context.Background())
	if err != nil || first.AccessKeyID != "AKIA-OLD" || !first.CanExpire || !first.Expires.Equal(now.Add(credentialRecheckInterval)) {
		t.Fatalf("first = %#v, %v", first, err)
	}
	source.access, source.secret = "AKIA-NEW", "new-secret"
	if rotated, err := provider.Retrieve(context.Background()); err != nil || rotated.SecretAccessKey != "new-secret" {
		t.Fatalf("rotated = %#v, %v", rotated, err)
	}
	source.err = errors.New("transient")
	if kept, err := provider.Retrieve(context.Background()); err != nil || kept.AccessKeyID != "AKIA-NEW" {
		t.Fatalf("a failed read must keep the last good keys: %#v, %v", kept, err)
	}
	if len(loaded) != 6 || string(loaded[3]) != "AKIA-NEW" {
		t.Fatalf("the redactor was not taught each key set once: %q", loaded)
	}
	if !invalidOptions(Options{Bucket: "b", Region: "r", RequestTimeout: time.Second, KeySource: source, AccessKeyIDFile: "/a", SecretAccessKeyFile: "/s"}) {
		t.Fatal("a key source mixed with credential files was accepted")
	}
	if invalidOptions(Options{Bucket: "b", Region: "r", RequestTimeout: time.Second, KeySource: source}) {
		t.Fatal("a key source alone was refused")
	}
}

// TestPodIdentityProviderSendsTheProjectedToken: EKS Pod Identity reads
// its token file at every refresh (the kubelet rotates it) and presents it
// to the Pod Identity Agent.
func TestPodIdentityProviderSendsTheProjectedToken(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "eks-pod-identity-token")
	if err := os.WriteFile(path, []byte("synthetic-token-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen = append(seen, request.Header.Get("Authorization"))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"AccessKeyId":"AKIDEXAMPLE","SecretAccessKey":"synthetic","Token":"session","Expiration":"2099-01-01T00:00:00Z"}`))
	}))
	defer server.Close()
	provider := podIdentityProvider(server.URL+"/v1/credentials", path)
	credentials, err := provider.Retrieve(context.Background())
	if err != nil || credentials.AccessKeyID != "AKIDEXAMPLE" {
		t.Fatalf("Retrieve = %+v, %v", credentials, err)
	}
	if err := os.WriteFile(path, []byte("synthetic-token-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Retrieve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "synthetic-token-1" || seen[1] != "synthetic-token-2" {
		t.Fatalf("Authorization headers = %q", seen)
	}
	if _, err := tokenFile("").GetToken(); err == nil {
		t.Fatal("an empty token path was read")
	}
	if _, err := tokenFile(filepath.Join(directory, "absent")).GetToken(); err == nil {
		t.Fatal("a missing token file was read")
	}
}

// TestRotationSignerFollowsASecretRotatedUnderTheSameAccessKey: the SDK
// signer caches its derived key by access key ID and day, so it signs with
// a rotated secret's predecessor (seen in the lab: a restored secret kept
// being refused until a restart). The rotation signer must sign exactly as
// a fresh signer given the current secret.
func TestRotationSignerFollowsASecretRotatedUnderTheSameAccessKey(t *testing.T) {
	t.Parallel()
	signingTime := time.Date(2026, 11, 3, 6, 26, 35, 0, time.UTC)
	sign := func(signer awss3.HTTPSignerV4, secret string) string {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://garage.example:3900/barman/?list-type=2", nil)
		if err != nil {
			t.Fatal(err)
		}
		credentials := aws.Credentials{AccessKeyID: "GK-SAME", SecretAccessKey: secret}
		if err := signer.SignHTTP(context.Background(), credentials, request, v4EmptyPayload, "s3", "us-east-1", signingTime); err != nil {
			t.Fatal(err)
		}
		return request.Header.Get("Authorization")
	}
	fresh := func(secret string) string {
		return sign(v4.NewSigner(func(options *v4.SignerOptions) { options.DisableURIPathEscaping = true }), secret)
	}

	sdk := v4.NewSigner(func(options *v4.SignerOptions) { options.DisableURIPathEscaping = true })
	if sign(sdk, "old-secret"); sign(sdk, "new-secret") != fresh("old-secret") {
		t.Log("the SDK signer no longer caches by access key ID only; rotationSigner may be unnecessary")
	}

	signer := newRotationSigner()
	if got := sign(signer, "old-secret"); got != fresh("old-secret") {
		t.Fatalf("first signature differs from the SDK's: %s", got)
	}
	if got := sign(signer, "new-secret"); got != fresh("new-secret") {
		t.Fatal("a secret rotated under the same access key ID was signed with the previous secret")
	}
	if got := sign(signer, "new-secret"); got != fresh("new-secret") {
		t.Fatal("an unchanged secret signed differently")
	}

	endpoint, err := url.Parse("https://garage.example:3900")
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(context.Background(), Options{Bucket: "b", Region: "us-east-1", RequestTimeout: time.Second, KeySource: &rotatingKeys{access: "GK-SAME", secret: "s"}, Endpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.client.(*awss3.Client).Options().HTTPSignerV4.(*rotationSigner); !ok {
		t.Fatal("the S3 client does not sign with rotationSigner")
	}
}

const v4EmptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
