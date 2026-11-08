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
	"slices"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
)

// setup.py is pgAdmin's only supported writer of its settings database. It
// runs only from these Jobs, never from the operator's
// Reconcile and never from a long-running sidecar: a Job per offered set
// (registration) and a Job per image change (pre-upgrade backup). Both use
// pgAdmin's own image, mount the settings volume next to the running
// pgAdmin (same node: the volume is ReadWriteOnce), take the file lock that
// serializes them, and report their outcome through the Job's status and
// their container's termination message.

const (
	jobDeadlineSeconds int64 = 600
	jobTTLSeconds      int32 = 3600
)

// authSource is pgAdmin's authentication source of the people signing in.
func authSource(p *pgtoolboxv1alpha1.PgAdmin) string {
	if p.Spec.Auth.OpenShiftOAuthProxy != nil {
		return "webserver"
	}
	return "oauth2"
}

// registrationHash identifies one registration: the offered set, the
// administrators, the authentication source, and the settings volume (a
// new volume is an empty database to register into again).
func registrationHash(p *pgtoolboxv1alpha1.PgAdmin, offerHash, volumeUID string) string {
	return specs.Hash([]string{offerHash, strings.Join(p.Spec.Admins, ","), authSource(p), volumeUID})
}

func jobPodAffinity(p *pgtoolboxv1alpha1.PgAdmin) *corev1.Affinity {
	return &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: identity(p).SelectorLabels(componentPgAdmin)},
			TopologyKey:   "kubernetes.io/hostname",
		}},
	}}
}

// buildJob is the common shape of both Jobs.
func buildJob(p *pgtoolboxv1alpha1.PgAdmin, in buildInputs, name, component string, image specs.ResolvedImage, args []string, extraVolumes []corev1.Volume, extraMounts []corev1.VolumeMount) *batchv1.Job {
	mounts := append([]corev1.VolumeMount{
		{Name: binVolume, MountPath: binMountPath, ReadOnly: true},
		{Name: settingsVolume, MountPath: settingsMountPath},
		{Name: tmpVolume, MountPath: "/tmp"},
	}, extraMounts...)
	volumes := append([]corev1.Volume{
		{Name: binVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(256<<20, resource.BinarySI)}}},
		settingsVolumeSource(p),
		{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(256<<20, resource.BinarySI)}}},
	}, extraVolumes...)
	var affinity *corev1.Affinity
	if p.Spec.ConfigDatabase == nil {
		// Next to pgAdmin: the settings volume is ReadWriteOnce.
		affinity = jobPodAffinity(p)
	}
	job := &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: objectMeta(p, name, component, pgtoolboxv1alpha1.PropagationTargetWorkload),
		Spec: batchv1.JobSpec{
			BackoffLimit:            new(int32(2)),
			ActiveDeadlineSeconds:   new(jobDeadlineSeconds),
			TTLSecondsAfterFinished: new(jobTTLSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: identity(p).Labels(component)},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           namesOf(p).base,
					AutomountServiceAccountToken: new(false),
					EnableServiceLinks:           new(false),
					SecurityContext:              specs.PodSecurityContext(in.PlatformAssignsGroups, fsGroup),
					ImagePullSecrets:             image.PullSecrets,
					Affinity:                     affinity,
					InitContainers:               []corev1.Container{installContainer(in)},
					Containers: []corev1.Container{{
						Name:            component,
						Image:           image.Reference,
						ImagePullPolicy: image.PullPolicy,
						Command:         append([]string{binPath}, args...),
						Env: []corev1.EnvVar{
							{Name: "TMPDIR", Value: "/tmp"},
							{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}},
						},
						SecurityContext:          specs.ContainerSecurityContext(),
						Resources:                pgAdminResources(p.Spec.Resources),
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
						VolumeMounts:             mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
	specs.SetHash(job, specs.Hash(job.Spec))
	return job
}

// buildRegistrationJob registers the offered set as pgAdmin shared servers
// owned by the internal bootstrap account, and the administrators.
func buildRegistrationJob(p *pgtoolboxv1alpha1.PgAdmin, in buildInputs, hash string) *batchv1.Job {
	args := make([]string, 0, 11+2*len(p.Spec.Admins))
	args = append(args,
		"pgadmin-register",
		"--offer", offerMountPath+"/"+offer.SecretKey,
		"--owner", bootstrapEmail,
		"--auth-source", authSource(p),
		"--settings-db", settingsDBPath,
		"--lock", lockFile,
	)
	for _, admin := range p.Spec.Admins {
		args = append(args, "--admin", admin)
	}
	// The users whose personal servers the last registration managed and
	// the offer no longer gives any: their managed personal servers go.
	for _, owner := range p.Status.PersonalServerOwners {
		if !slices.Contains(in.Owners, owner) {
			args = append(args, "--forget-owner", owner)
		}
	}
	volumes := []corev1.Volume{{Name: offerVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName:  namesOf(p).offer,
		DefaultMode: new(int32(0o440)),
	}}}}
	mounts := []corev1.VolumeMount{{Name: offerVolume, MountPath: offerMountPath, ReadOnly: true}}
	if in.ConfigDatabase.URI != "" {
		// setup.py opens the settings database like pgAdmin does: the Job
		// writes the settings database's credential files itself, from the
		// Secrets the offer names, through the Pod's name-scoped Role, onto
		// its own memory-backed volume at the path the URI names.
		args = append(args, "--config-database-uri", in.ConfigDatabase.URI, "--credentials-dir", credentialsMountPath)
		volumes = append(volumes,
			corev1.Volume{Name: credentialsVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium:    corev1.StorageMediumMemory,
				SizeLimit: resource.NewQuantity(credentialsVolumeBytes, resource.BinarySI),
			}}},
			apiAccessProjection(),
		)
		mounts = append(mounts,
			corev1.VolumeMount{Name: credentialsVolume, MountPath: credentialsMountPath},
			corev1.VolumeMount{Name: apiAccessVolume, MountPath: apiAccessMountPath, ReadOnly: true},
		)
	}
	job := buildJob(p, in, registrationJobName(p, hash), componentRegistration, in.Image, args, volumes, mounts)
	job.Annotations = withAnnotation(job.Annotations, annotationRegistrationHash, hash)
	return job
}

// buildBackupJob copies the settings database with SQLite's online backup
// before a new image migrates it, using the image that wrote it.
func buildBackupJob(p *pgtoolboxv1alpha1.PgAdmin, in buildInputs, running specs.ResolvedImage, hash string, keep int) *batchv1.Job {
	args := []string{
		"pgadmin-backup",
		"--settings-db", settingsDBPath,
		"--dir", backupDir,
		"--keep", strconv.Itoa(keep),
		"--lock", lockFile,
		"--label", hash[:8],
	}
	job := buildJob(p, in, backupJobName(p, hash), componentBackup, running, args, nil, nil)
	job.Annotations = withAnnotation(job.Annotations, annotationUpgradeTarget, in.Image.Reference)
	return job
}

const (
	annotationRegistrationHash = pgtoolboxv1alpha1.ReservedKeyPrefix + "registration-hash"
	annotationUpgradeTarget    = pgtoolboxv1alpha1.ReservedKeyPrefix + "upgrade-target"
)

func withAnnotation(annotations map[string]string, key, value string) map[string]string {
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[key] = value
	return annotations
}
