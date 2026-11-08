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

package passfilesync

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
)

// capture records every log line.
type capture struct {
	mu    sync.Mutex
	lines []string
}

func (c *capture) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.lines = append(c.lines, prefix+" "+args)
	}, funcr.Options{Verbosity: 10})
}

func (c *capture) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

func offerSecret(t *testing.T, servers ...offer.Server) *corev1.Secret {
	t.Helper()
	raw, err := offer.Encode(offer.Document{Servers: servers})
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "team-pgadmin-offer", Namespace: "team"},
		Data:       map[string][]byte{offer.SecretKey: raw},
	}
}

func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func fileContains(path, text string) bool {
	raw, err := os.ReadFile(path) // #nosec G304 -- test path
	return err == nil && strings.Contains(string(raw), text)
}

func TestRunFollowsRotationAndOfferChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := server("one", "app", "one-app", "")
	client := fake.NewClientset(
		offerSecret(t, srv),
		secret("one-app", "", map[string]string{"username": "app", "password": "pw-before"}),
		caSecret("one-ca", caPEM1),
	)
	files := Files{Dir: t.TempDir()}
	logs := &capture{}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Client: client, Namespace: "team", OfferSecret: "team-pgadmin-offer", Files: files,
			Log: logs.logger(), Debounce: 20 * time.Millisecond, Backoff: 50 * time.Millisecond,
		})
	}()

	eventually(t, "the first password", func() bool { return fileContains(files.PassFile(), "pw-before") })

	// Rotation: the Secret changes, the files follow without a restart.
	rotated := secret("one-app", "", map[string]string{"username": "app", "password": "pw-after"})
	if _, err := client.CoreV1().Secrets("team").Update(ctx, rotated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the rotated password", func() bool {
		return fileContains(files.PassFile(), "pw-after") && !fileContains(files.PassFile(), "pw-before") &&
			fileContains(files.ServiceFile(), "password=pw-after")
	})

	// The offer drops the server: it is withdrawn from both files.
	if _, err := client.CoreV1().Secrets("team").Update(ctx, offerSecret(t), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the server withdrawn", func() bool {
		return !fileContains(files.PassFile(), "pw-after") && !fileContains(files.ServiceFile(), "[pgtoolbox-one-app]")
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
	logged := logs.all()
	if logged == "" {
		t.Fatal("nothing was logged; the capture is not wired")
	}
	for _, pw := range []string{"pw-before", "pw-after"} {
		if strings.Contains(logged, pw) {
			t.Fatalf("a password reached the log:\n%s", logged)
		}
	}
}

func TestRunKeepsFilesOnInvalidOffer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := server("one", "app", "one-app", "")
	client := fake.NewClientset(
		offerSecret(t, srv),
		secret("one-app", "", map[string]string{"username": "app", "password": "pw-kept"}),
		caSecret("one-ca", caPEM1),
	)
	files := Files{Dir: t.TempDir()}
	go func() {
		_ = Run(ctx, Options{
			Client: client, Namespace: "team", OfferSecret: "team-pgadmin-offer", Files: files,
			Log: logr.Discard(), Debounce: 20 * time.Millisecond, Backoff: 50 * time.Millisecond,
		})
	}()
	eventually(t, "the first password", func() bool { return fileContains(files.PassFile(), "pw-kept") })

	broken := offerSecret(t)
	broken.Data[offer.SecretKey] = []byte(`{"version":1,"servers":[],"surprise":1}`)
	if _, err := client.CoreV1().Secrets("team").Update(ctx, broken, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if !fileContains(files.PassFile(), "pw-kept") {
		t.Fatal("a rejected offer withdrew the files")
	}
}

// TestDrainFilesOnlyItsOwnName: an event for another object, its deletion
// included, is never delivered under the watched name.
func TestDrainFilesOnlyItsOwnName(t *testing.T) {
	w := watch.NewFake()
	var got []any
	done := make(chan struct{})
	go func() {
		drain(context.Background(), w, "mine", func(obj any) { got = append(got, obj) })
		close(done)
	}()
	w.Add(secret("other", "1", nil))
	w.Delete(secret("other", "1", nil))
	w.Modify(secret("mine", "2", map[string]string{"password": "x"}))
	w.Delete(secret("mine", "3", nil))
	w.Stop()
	<-done
	if len(got) != 2 || got[0] == nil || got[1] != nil {
		t.Fatalf("delivered %v, want the Modify then the deletion of mine", got)
	}
}
