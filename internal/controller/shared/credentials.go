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

// Package shared holds the helpers used by more than one reconciler:
// basic-auth credential reading, object-ownership comparisons, and application
// identity.
package shared

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// BasicAuthCredential is the parsed content of a kubernetes.io/basic-auth
// Secret together with the resourceVersion it was read at.
type BasicAuthCredential struct {
	Username        string
	Password        string
	ResourceVersion string
}

// SecretFormatError reports a Secret that is not usable credential material.
type SecretFormatError struct {
	key client.ObjectKey
}

// Error identifies the offending Secret by key only, never echoing its
// contents, so the message is safe for logs, events and status conditions.
func (e *SecretFormatError) Error() string {
	return fmt.Sprintf("referenced secret %s/%s is not a valid credential", e.key.Namespace, e.key.Name)
}

// ReadCredential is the credential-content boundary for mounted
// authentication modes. Callers must pass the live API reader; no returned
// value may be retained outside the current reconciliation.
//
// The keys are a parameter because providers publish credentials under
// different keys. Empty keys fall back to the kubernetes.io/basic-auth
// convention.
func ReadCredential(
	ctx context.Context,
	reader client.Reader,
	key client.ObjectKey,
	usernameKey, passwordKey string,
) (*BasicAuthCredential, error) {
	if usernameKey == "" {
		usernameKey = corev1.BasicAuthUsernameKey
	}
	if passwordKey == "" {
		passwordKey = corev1.BasicAuthPasswordKey
	}
	var secret corev1.Secret
	if err := reader.Get(ctx, key, &secret); err != nil {
		return nil, err
	}
	username := secret.Data[usernameKey]
	password := secret.Data[passwordKey]
	if len(username) == 0 || len(password) == 0 ||
		strings.ContainsAny(string(username), "\r\n") || strings.ContainsAny(string(password), "\r\n") {
		return nil, &SecretFormatError{key: key}
	}
	return &BasicAuthCredential{
		Username:        string(username),
		Password:        string(password),
		ResourceVersion: secret.ResourceVersion,
	}, nil
}

// ReadBasicAuthCredential reads a credential stored under the conventional
// username/password keys.
func ReadBasicAuthCredential(
	ctx context.Context,
	reader client.Reader,
	key client.ObjectKey,
) (*BasicAuthCredential, error) {
	return ReadCredential(ctx, reader, key, corev1.BasicAuthUsernameKey, corev1.BasicAuthPasswordKey)
}

// ReadLocalPasswordHash reads a bcrypt hash from a Secret for the proxy's
// local authentication mode. The default key is "password"; any other key can
// be requested through key. Empty values or values containing newlines are
// rejected, and the value must parse as a bcrypt hash.
func ReadLocalPasswordHash(
	ctx context.Context,
	reader client.Reader,
	secretKey client.ObjectKey,
	key string,
) (string, string, error) {
	if key == "" {
		key = "password"
	}
	var secret corev1.Secret
	if err := reader.Get(ctx, secretKey, &secret); err != nil {
		return "", "", err
	}
	hash := secret.Data[key]
	if len(hash) == 0 || strings.ContainsAny(string(hash), "\r\n") {
		return "", "", &SecretFormatError{key: secretKey}
	}
	if _, err := bcrypt.Cost(hash); err != nil {
		return "", "", &SecretFormatError{key: secretKey}
	}
	return string(hash), secret.ResourceVersion, nil
}
