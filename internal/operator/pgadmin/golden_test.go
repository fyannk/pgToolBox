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
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files")

// TestGoldenChildren renders every child object of three PgAdmin shapes
// and compares them with testdata/golden/<shape>/<kind>-<name>.yaml.
// Builders are pure: the same input renders the same bytes, so a no-op
// reconcile applies nothing new.
func TestGoldenChildren(t *testing.T) {
	shapes := []struct {
		name      string
		build     func() (objs []client.Object)
		openshift bool
	}{
		{name: "oauth2-clusterip", build: func() []client.Object { return renderAll(oauth2PgAdmin(), false) }},
		{name: "openshift-route-netpol", build: func() []client.Object { return renderAll(openshiftPgAdmin(), true) }},
		{name: "ingress-tls", build: func() []client.Object { return renderAll(ingressPgAdmin(), false) }},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			dir := filepath.Join("testdata", "golden", shape.name)
			objs := shape.build()
			again := shape.build()
			seen := map[string]bool{}
			for i, obj := range objs {
				raw, err := yaml.Marshal(obj)
				if err != nil {
					t.Fatal(err)
				}
				rawAgain, _ := yaml.Marshal(again[i])
				if string(raw) != string(rawAgain) {
					t.Fatalf("%T %s is not deterministic", obj, obj.GetName())
				}
				file := strings.ToLower(obj.GetObjectKind().GroupVersionKind().Kind) + "-" + obj.GetName() + ".yaml"
				seen[file] = true
				path := filepath.Join(dir, file)
				if *updateGolden {
					if err := os.MkdirAll(dir, 0o750); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, raw, 0o600); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(path) // #nosec G304 -- golden fixture
				if err != nil {
					t.Fatalf("%s: %v (run go test -run TestGoldenChildren -update)", path, err)
				}
				if string(want) != string(raw) {
					t.Errorf("%s differs from golden (go test -run TestGoldenChildren -update):\n%s", path, raw)
				}
			}
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				if !seen[entry.Name()] && !*updateGolden {
					t.Errorf("stale golden file %s", entry.Name())
				}
			}
		})
	}
}

func renderAll(p interface {
	client.Object
}, openshift bool,
) []client.Object {
	pga := p.(*pgadminT)
	in := testInputs(pga, openshift)
	hash := registrationHash(pga, in.OfferHash, "pvc-uid")
	objs := []client.Object{
		buildServiceAccount(pga),
		buildCredentialsRole(pga, in.OfferSecrets),
		buildCredentialsRoleBinding(pga),
		buildOfferSecret(pga, in),
		buildSettingsPVC(pga),
		buildService(pga),
		buildDeployment(pga, in),
		buildRegistrationJob(pga, in, hash),
		buildBackupJob(pga, in, in.Image, hash, 3),
	}
	if np := buildNetworkPolicy(pga, in); np != nil {
		objs = append(objs, np)
	}
	switch pga.Spec.Exposure.TypeOrDefault() {
	case pgtoolboxv1alpha1.ExposureTypeRoute:
		objs = append(objs, buildRoute(pga))
	case pgtoolboxv1alpha1.ExposureTypeIngress:
		objs = append(objs, buildIngress(pga))
	case pgtoolboxv1alpha1.ExposureTypeGateway:
		objs = append(objs, buildHTTPRoute(pga))
	case pgtoolboxv1alpha1.ExposureTypeClusterIP:
	}
	return objs
}
