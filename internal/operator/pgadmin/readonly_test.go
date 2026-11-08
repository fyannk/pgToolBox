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
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestEveryContainerHasAReadOnlyRootFilesystem: the pgAdmin container has
// a read-only root filesystem like the others, and what its image writes goes to the
// run volume (config_distro.py, passwd), the settings volume and /tmp.
func TestEveryContainerHasAReadOnlyRootFilesystem(t *testing.T) {
	for _, p := range []*pgadminT{oauth2PgAdmin(), openshiftPgAdmin()} {
		in := testInputs(p, p.Spec.Auth.OpenShiftOAuthProxy != nil)
		hash := registrationHash(p, in.OfferHash, "uid")
		pods := map[string]corev1.PodSpec{
			"deployment":   buildDeployment(p, in).Spec.Template.Spec,
			"registration": buildRegistrationJob(p, in, hash).Spec.Template.Spec,
			"backup":       buildBackupJob(p, in, in.Image, hash, 3).Spec.Template.Spec,
		}
		for kind, pod := range pods {
			for _, c := range append(pod.InitContainers, pod.Containers...) {
				sc := c.SecurityContext
				if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
					t.Errorf("%s/%s %s: root filesystem is writable", p.Name, kind, c.Name)
				}
			}
		}
		pgadmin := pods["deployment"].Containers[0]
		if pgadmin.Name != "pgadmin" {
			t.Fatalf("first container is %s", pgadmin.Name)
		}
		mounted := map[string]string{}
		for _, m := range pgadmin.VolumeMounts {
			mounted[m.MountPath] = m.Name
		}
		for _, path := range []string{runMountPath, settingsMountPath, "/tmp"} {
			if mounted[path] == "" {
				t.Errorf("%s: nothing writable mounted at %s", p.Name, path)
			}
		}
		var distro string
		for _, e := range pgadmin.Env {
			if e.Name == "PGADMIN_CUSTOM_CONFIG_DISTRO_FILE" {
				distro = e.Value
			}
		}
		if distro != configDistroPath {
			t.Errorf("%s: PGADMIN_CUSTOM_CONFIG_DISTRO_FILE=%q, want %q", p.Name, distro, configDistroPath)
		}
	}
}
