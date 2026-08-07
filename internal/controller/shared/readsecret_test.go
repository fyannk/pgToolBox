/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package shared

import (
	"context"
	"testing"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReadBasicAuth(t *testing.T) {
	valid := &corev1.Secret{
		ObjectMeta: objectMeta("valid"),
		Data: map[string][]byte{
			corev1.BasicAuthUsernameKey: []byte("alice"),
			corev1.BasicAuthPasswordKey: []byte("secret"),
		},
	}

	r := &stubReader{secret: valid}
	cred, err := ReadBasicAuthCredential(context.Background(), r, client.ObjectKeyFromObject(valid))
	if err != nil {
		t.Fatalf("read valid credential: %v", err)
	}
	if cred.Username != "alice" || cred.Password != "secret" {
		t.Fatalf("credential = %+v", cred)
	}
}

func TestReadBasicAuthRejectsEmpty(t *testing.T) {
	invalid := &corev1.Secret{
		ObjectMeta: objectMeta("invalid"),
		Data: map[string][]byte{
			corev1.BasicAuthUsernameKey: []byte("alice"),
		},
	}

	r := &stubReader{secret: invalid}
	_, err := ReadBasicAuthCredential(context.Background(), r, client.ObjectKeyFromObject(invalid))
	if err == nil {
		t.Fatalf("empty password must be rejected")
	}
}

func TestReadLocalPasswordHash(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("generate hash: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: objectMeta("local"),
		Data:       map[string][]byte{"password": hash},
	}

	r := &stubReader{secret: secret}
	got, _, err := ReadLocalPasswordHash(context.Background(), r, client.ObjectKeyFromObject(secret), "")
	if err != nil {
		t.Fatalf("read local hash: %v", err)
	}
	if got != string(hash) {
		t.Fatalf("hash mismatch")
	}
}

func TestReadLocalPasswordHashRejectsInvalid(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: objectMeta("local"),
		Data:       map[string][]byte{"password": []byte("not-a-hash")},
	}

	r := &stubReader{secret: secret}
	_, _, err := ReadLocalPasswordHash(context.Background(), r, client.ObjectKeyFromObject(secret), "")
	if err == nil {
		t.Fatalf("invalid hash must be rejected")
	}
}

func objectMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: "test"}
}

type stubReader struct {
	secret *corev1.Secret
	err    error
}

func (r *stubReader) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if r.err != nil {
		return r.err
	}
	s := obj.(*corev1.Secret)
	*s = *r.secret
	return nil
}

func (r *stubReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return r.err
}
