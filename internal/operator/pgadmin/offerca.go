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

package pgadmin

import (
	"bytes"
	"cmp"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/passfilesync"
)

// offerCAs copies into doc the server CA of every Cluster it offers, read
// past the cache from the Cluster's CA Secret: only the CERTIFICATE blocks
// of ca.crt that parse, re-encoded, so ca.key (CloudNativePG's <cluster>-ca
// holds it, and signs client certificates with it when the Cluster names
// no clientCASecret) never leaves the operator. pgAdmin's ServiceAccount
// reads no CA Secret. CloudNativePG records the CA Secret's version in the
// Cluster's status, so a renewed CA reaches the offer through the Cluster
// watch. A CA Secret that is absent, holds no certificate or is too large
// leaves its Cluster out (the sidecar refuses its servers and says why);
// any other failed read fails the loop.
func (r *Reconciler) offerCAs(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, doc *offer.Document) error {
	seen := map[string]bool{}
	for _, server := range doc.Connections() {
		key := server.CAKey()
		if seen[key] {
			continue
		}
		seen[key] = true
		namespace := cmp.Or(server.Namespace, p.Namespace)
		secret := &corev1.Secret{}
		err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: server.CASecretName}, secret)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return fmt.Errorf("read the server CA Secret %s/%s: %w", namespace, server.CASecretName, err)
		}
		if ca := certificatesPEM(secret.Data["ca.crt"]); ca != "" && len(ca) <= passfilesync.MaxSecretBytes {
			if doc.CAs == nil {
				doc.CAs = map[string]string{}
			}
			doc.CAs[key] = ca
		}
	}
	return nil
}

// certificatesPEM re-encodes the CERTIFICATE blocks of raw that parse as
// X.509 certificates, in order; whatever else raw carries is dropped.
func certificatesPEM(raw []byte) string {
	var out bytes.Buffer
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			return out.String()
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err == nil {
			_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}) // a bytes.Buffer write cannot fail
		}
	}
}
