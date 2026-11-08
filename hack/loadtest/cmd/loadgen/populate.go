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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	postgresImage = "ghcr.io/cloudnative-pg/postgresql:18.1"
	operatorImage = "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.0"
	// releaseCommit is the build commit CloudNativePG 1.30.0 writes into
	// status.cloudNativePGCommitHash; the console detects the operator
	// version from it (pkg/cnpgstatus/version.go).
	releaseCommit = "4b5e244a7d031f67e025c83c1555e7726ecbbfa1"
	systemID      = "7412345678901234567"
)

type populateOpts struct {
	kube       kubeFlags
	namespaces int
	clusters   int
	instances  int
	backups    int
	events     int
	prefix     string
	selectors  bool
	workers    int
	wait       time.Duration
}

func runPopulate(ctx context.Context, args []string) error {
	var o populateOpts
	fs := flag.NewFlagSet("populate", flag.ContinueOnError)
	o.kube.register(fs, 300, 600)
	fs.IntVar(&o.namespaces, "namespaces", 2, "namespaces to fill (lt-01 ... ; the console watches the ones cluster.sh up was given)")
	fs.IntVar(&o.clusters, "clusters", 20, "Clusters per namespace")
	fs.IntVar(&o.instances, "instances", 3, "member Pods per Cluster")
	fs.IntVar(&o.backups, "backups", 2, "completed Backups per Cluster")
	fs.IntVar(&o.events, "events", 2, "Events per Cluster")
	fs.StringVar(&o.prefix, "namespace-prefix", "lt", "namespace prefix")
	fs.BoolVar(&o.selectors, "service-selectors", false, "give the Services their Pod selectors (EndpointSlices and kube-proxy rules for every Pod: realistic, but heavy on the host)")
	fs.IntVar(&o.workers, "workers", 24, "Clusters built in parallel")
	fs.DurationVar(&o.wait, "wait-running", 3*time.Minute, "wait this long for every Pod to be Running with an IP (0: do not wait)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.namespaces < 1 || o.clusters < 1 || o.instances < 1 || o.workers < 1 {
		return errors.New("--namespaces, --clusters, --instances and --workers must be at least 1")
	}
	k, err := o.kube.connect()
	if err != nil {
		return err
	}
	return populate(ctx, k, &o)
}

type counters struct {
	created, existed, failed atomic.Int64
	firstErr                 atomic.Value
}

func (c *counters) fail(err error) {
	c.failed.Add(1)
	c.firstErr.CompareAndSwap(nil, err.Error())
}

func populate(ctx context.Context, k *kube, o *populateOpts) error {
	started := time.Now()
	nodes, err := kwokNodes(ctx, k)
	if err != nil {
		return err
	}
	fmt.Printf("populate: %d namespaces x %d clusters x %d instances on %d KWOK nodes\n", o.namespaces, o.clusters, o.instances, len(nodes))
	for i := 1; i <= o.namespaces; i++ {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName(o.prefix, i), Labels: map[string]string{harnessLabel: "true"}}}
		if _, err := ensure(ctx, k.client, ns); err != nil {
			return fmt.Errorf("namespace %s: %w", ns.Name, err)
		}
	}

	var ctr counters
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(o.workers)
	total := o.namespaces * o.clusters
	var done atomic.Int64
	for i := 1; i <= o.namespaces; i++ {
		for j := 1; j <= o.clusters; j++ {
			ns, name, ordinal := nsName(o.prefix, i), clusterName(j), (i-1)*o.clusters+j
			g.Go(func() error {
				if err := buildCluster(gctx, k.client, &ctr, o, nodes, ns, name, ordinal); err != nil {
					ctr.fail(fmt.Errorf("%s/%s: %w", ns, name, err))
				}
				if n := done.Add(1); n%50 == 0 || int(n) == total {
					fmt.Printf("populate: %d/%d clusters (%.0fs)\n", n, total, time.Since(started).Seconds())
				}
				return nil
			})
		}
	}
	_ = g.Wait()
	elapsed := time.Since(started)
	objects := ctr.created.Load() + ctr.existed.Load()
	fmt.Printf("populate: %d objects (%d created, %d already there, %d failed) in %s = %.0f objects/s\n",
		objects, ctr.created.Load(), ctr.existed.Load(), ctr.failed.Load(), elapsed.Round(time.Millisecond), float64(objects)/elapsed.Seconds())
	if msg, ok := ctr.firstErr.Load().(string); ok {
		fmt.Println("populate: first error:", msg)
	}
	if o.wait > 0 && ctr.failed.Load() == 0 {
		want := o.namespaces * o.clusters * o.instances
		if err := waitRunning(ctx, k, o.prefix, o.namespaces, want, o.wait); err != nil {
			return err
		}
	}
	if ctr.failed.Load() > 0 {
		return fmt.Errorf("%d objects failed", ctr.failed.Load())
	}
	return nil
}

func kwokNodes(ctx context.Context, k *kube) ([]string, error) {
	var list corev1.NodeList
	if err := k.client.List(ctx, &list, client.MatchingLabels{"type": "kwok"}); err != nil {
		return nil, fmt.Errorf("list KWOK nodes (%s): %w", kwokNodeLabel, err)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("no node labelled %s: run cluster.sh up first", kwokNodeLabel)
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	return names, nil
}

// ensure creates obj; an object that already exists is read back instead,
// so callers get its UID either way. It reports whether it created it.
func ensure(ctx context.Context, c client.Client, obj client.Object) (bool, error) {
	err := c.Create(ctx, obj)
	if err == nil {
		return true, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return false, err
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return false, err
	}
	return false, nil
}

func (c *counters) ensure(ctx context.Context, cl client.Client, obj client.Object) error {
	created, err := ensure(ctx, cl, obj)
	switch {
	case err != nil:
		return fmt.Errorf("%T %s: %w", obj, obj.GetName(), err)
	case created:
		c.created.Add(1)
	default:
		c.existed.Add(1)
	}
	return nil
}

func buildCluster(ctx context.Context, cl client.Client, ctr *counters, o *populateOpts, nodes []string, ns, name string, ordinal int) error {
	cluster := newCluster(ns, name, o.instances)
	if err := ctr.ensure(ctx, cl, cluster); err != nil {
		return err
	}
	// Status is written on every run (a merge patch of equal content is a
	// no-op on the server), so a run that died between create and status
	// is repaired by the next one.
	if err := patchStatus(ctx, cl, cluster, clusterStatus(cluster, o.instances, time.Now().UTC(), 0)); err != nil {
		return fmt.Errorf("cluster status: %w", err)
	}
	owner := metav1.OwnerReference{
		APIVersion: cnpgv1.SchemeGroupVersion.String(), Kind: "Cluster", Name: name, UID: cluster.UID,
		Controller: new(true), BlockOwnerDeletion: new(true),
	}
	for _, suffix := range []string{"rw", "ro", "r"} {
		if err := ctr.ensure(ctx, cl, newService(ns, name, suffix, owner, o.selectors)); err != nil {
			return err
		}
	}
	for n := 1; n <= o.instances; n++ {
		node := nodes[(ordinal+n)%len(nodes)]
		pv, pvc, pod := newInstance(ns, name, n, node, owner)
		for _, obj := range []client.Object{pv, pvc, pod} {
			if err := ctr.ensure(ctx, cl, obj); err != nil {
				return err
			}
		}
		// The pair is pre-bound, but the PV controller writes the Bound
		// status at a few objects per second: thousands of PVCs would
		// keep changing (and the console invalidating) for many minutes
		// after populate returns. Written here, there is nothing left
		// for it to change.
		if err := patchStatus(ctx, cl, pv, map[string]any{"phase": corev1.VolumeBound}); err != nil {
			return fmt.Errorf("pv status: %w", err)
		}
		if err := patchStatus(ctx, cl, pvc, map[string]any{
			"phase":       corev1.ClaimBound,
			"accessModes": pvc.Spec.AccessModes,
			"capacity":    pvc.Spec.Resources.Requests,
		}); err != nil {
			return fmt.Errorf("pvc status: %w", err)
		}
	}
	for n := 1; n <= o.backups; n++ {
		backup := newBackup(ns, name, n, owner)
		created, err := ensure(ctx, cl, backup)
		if err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		if created {
			ctr.created.Add(1)
		} else {
			ctr.existed.Add(1)
		}
		if err := patchStatus(ctx, cl, backup, backupStatus(name, n)); err != nil {
			return fmt.Errorf("backup status: %w", err)
		}
	}
	if o.backups > 0 {
		if err := ctr.ensure(ctx, cl, newScheduledBackup(ns, name, owner)); err != nil {
			return err
		}
	}
	for n := 1; n <= o.events; n++ {
		if err := ctr.ensure(ctx, cl, newEvent(ns, name, n, cluster.UID)); err != nil {
			return err
		}
	}
	return nil
}

// patchStatus merge-patches {"status": status}.
func patchStatus(ctx context.Context, cl client.Client, obj client.Object, status any) error {
	body, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		return err
	}
	return cl.Status().Patch(ctx, obj, client.RawPatch(types.MergePatchType, body))
}

func newCluster(ns, name string, instances int) *cnpgv1.Cluster {
	return &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{harnessLabel: "true", "app": name}},
		Spec: cnpgv1.ClusterSpec{
			Instances:            instances,
			ImageName:            postgresImage,
			StorageConfiguration: cnpgv1.StorageConfiguration{Size: "10Gi"},
		},
	}
}

// clusterConditions are what a healthy CloudNativePG 1.30 Cluster carries.
// revision makes the message differ between two calls, so churn can write a
// change the API server does not collapse into a no-op.
func clusterConditions(at time.Time, revision int) []metav1.Condition {
	t := metav1.NewTime(at.Truncate(time.Second))
	return []metav1.Condition{
		{Type: string(cnpgv1.ConditionClusterReady), Status: metav1.ConditionTrue, Reason: string(cnpgv1.ClusterReady), Message: fmt.Sprintf("Cluster is Ready (revision %d)", revision), LastTransitionTime: t},
		{Type: "ConsistentSystemID", Status: metav1.ConditionTrue, Reason: "Consistent", Message: "A single, unique system ID was found across reporting instances.", LastTransitionTime: t},
	}
}

// clusterStatus is the status of a healthy Cluster of the given size. All
// instances are healthy, instance 1 is the primary.
func clusterStatus(c *cnpgv1.Cluster, instances int, at time.Time, revision int) cnpgv1.ClusterStatus {
	names := make([]string, 0, instances)
	reported := make(map[cnpgv1.PodName]cnpgv1.InstanceReportedState, instances)
	for n := 1; n <= instances; n++ {
		pod := instanceName(c.Name, n)
		names = append(names, pod)
		reported[cnpgv1.PodName(pod)] = cnpgv1.InstanceReportedState{IsPrimary: n == 1, TimeLineID: 1}
	}
	return cnpgv1.ClusterStatus{
		Phase:                   cnpgv1.PhaseHealthy,
		Instances:               instances,
		ReadyInstances:          instances,
		Selector:                "cnpg.io/cluster=" + c.Name + ",cnpg.io/podRole=instance",
		InstancesStatus:         map[cnpgv1.PodStatus][]string{cnpgv1.PodHealthy: names},
		InstancesReportedState:  reported,
		TimelineID:              1,
		CurrentPrimary:          instanceName(c.Name, 1),
		TargetPrimary:           instanceName(c.Name, 1),
		CurrentPrimaryTimestamp: at.Format(time.RFC3339Nano),
		PVCCount:                int32(instances), //nolint:gosec // bounded by --instances
		HealthyPVC:              names,
		WriteService:            c.Name + "-rw",
		ReadService:             c.Name + "-r",
		CommitHash:              releaseCommit[:7],
		OperatorHash:            releaseCommit,
		Conditions:              clusterConditions(at, revision),
		InstanceNames:           names,
		Image:                   postgresImage,
		PGDataImageInfo:         &cnpgv1.ImageInfo{Image: postgresImage, MajorVersion: 18},
		SystemID:                systemID,
	}
}

// newService is one of the three Services CloudNativePG owns per Cluster. With
// selectors it selects the member Pods like the real one, which makes the
// control plane build EndpointSlices and kube-proxy program rules for every
// Pod: realistic, and noisy for a host that also runs the measurement.
func newService(ns, cluster, suffix string, owner metav1.OwnerReference, selectors bool) *corev1.Service {
	var selector map[string]string
	if selectors {
		selector = map[string]string{clusterLabel: cluster}
		switch suffix {
		case "rw":
			selector["cnpg.io/instanceRole"] = "primary"
		case "ro":
			selector["cnpg.io/instanceRole"] = "replica"
		}
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: cluster + "-" + suffix,
			Labels:          map[string]string{clusterLabel: cluster, harnessLabel: "true"},
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selector,
			Ports:    []corev1.ServicePort{{Name: "postgres", Port: 5432, Protocol: corev1.ProtocolTCP}},
		},
	}
}

// newInstance returns the PV, PVC and Pod of member n. The PV and PVC are
// pre-bound to each other so the kube-controller-manager settles them as
// Bound without a provisioner; the Pod is created on a KWOK node, which
// gives it Running status and an IP.
func newInstance(ns, cluster string, n int, node string, owner metav1.OwnerReference) (*corev1.PersistentVolume, *corev1.PersistentVolumeClaim, *corev1.Pod) {
	pod := instanceName(cluster, n)
	role := "replica"
	if n == 1 {
		role = "primary"
	}
	labels := map[string]string{
		clusterLabel:           cluster,
		"cnpg.io/instanceName": pod,
		"cnpg.io/podRole":      "instance",
		"cnpg.io/instanceRole": role,
		"role":                 role,
		harnessLabel:           "true",
	}
	size := resource.MustParse("10Gi")
	pvName := "pv-" + ns + "-" + pod
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName, Labels: map[string]string{harnessLabel: "true"}},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: size},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			StorageClassName:              "kwok",
			PersistentVolumeSource:        corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/tmp/loadtest/" + pvName}},
			ClaimRef:                      &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: ns, Name: pod},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: pod, Labels: mergeLabels(labels, map[string]string{"cnpg.io/pvcRole": "PG_DATA"}),
			Annotations: map[string]string{
				"cnpg.io/nodeSerial": strconv.Itoa(n), "cnpg.io/pvcStatus": "ready",
				// What the PV controller adds when it completes a binding.
				"pv.kubernetes.io/bind-completed": "yes",
			},
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
			StorageClassName: new("kwok"),
			VolumeName:       pvName,
		},
	}
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: pod, Labels: labels,
			Annotations:     map[string]string{"cnpg.io/nodeSerial": strconv.Itoa(n)},
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: corev1.PodSpec{
			NodeName:                      node,
			TerminationGracePeriodSeconds: new(int64(30)),
			InitContainers: []corev1.Container{{
				Name: "bootstrap-controller", Image: operatorImage,
				Command: []string{"/manager", "bootstrap", "/controller/manager"},
			}},
			Containers: []corev1.Container{{
				Name: "postgres", Image: postgresImage,
				Command: []string{
					"/controller/manager", "instance", "run",
					"--status-port-tls",
					"--pg-data=/var/lib/postgresql/data/pgdata",
					"--pod-name=" + pod,
				},
				Ports: []corev1.ContainerPort{
					{Name: "postgresql", ContainerPort: 5432, Protocol: corev1.ProtocolTCP},
					{Name: "status", ContainerPort: 8000, Protocol: corev1.ProtocolTCP},
					{Name: "metrics", ContainerPort: 9187, Protocol: corev1.ProtocolTCP},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "pgdata", MountPath: "/var/lib/postgresql/data"}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "pgdata",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pod}},
			}},
		},
	}
	return pv, pvc, p
}

func mergeLabels(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func newBackup(ns, cluster string, n int, owner metav1.OwnerReference) *cnpgv1.Backup {
	return &cnpgv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: fmt.Sprintf("%s-backup-%02d", cluster, n),
			Labels:          map[string]string{clusterLabel: cluster, harnessLabel: "true"},
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: cnpgv1.BackupSpec{
			Cluster: cnpgv1.LocalObjectReference{Name: cluster},
			Method:  cnpgv1.BackupMethodPlugin,
			PluginConfiguration: &cnpgv1.BackupPluginConfiguration{
				Name: "barman-cloud.cloudnative-pg.io",
			},
		},
	}
}

func backupStatus(cluster string, n int) cnpgv1.BackupStatus {
	stopped := time.Now().UTC().Add(-time.Duration(n) * 24 * time.Hour).Truncate(time.Second)
	started := stopped.Add(-4 * time.Minute)
	return cnpgv1.BackupStatus{
		Phase:     cnpgv1.BackupPhaseCompleted,
		Method:    cnpgv1.BackupMethodPlugin,
		BackupID:  started.Format("20060102T150405"),
		StartedAt: new(metav1.NewTime(started)),
		StoppedAt: new(metav1.NewTime(stopped)),
		BeginWal:  fmt.Sprintf("0000000100000000%08X", 2*n),
		EndWal:    fmt.Sprintf("0000000100000000%08X", 2*n+1),
		BeginLSN:  fmt.Sprintf("0/%X000028", 2*n),
		EndLSN:    fmt.Sprintf("0/%X000100", 2*n+1),
		InstanceID: &cnpgv1.InstanceID{
			PodName:     instanceName(cluster, 1),
			ContainerID: "containerd://" + fmt.Sprintf("%064x", n),
		},
	}
}

func newScheduledBackup(ns, cluster string, owner metav1.OwnerReference) *cnpgv1.ScheduledBackup {
	return &cnpgv1.ScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: cluster + "-nightly",
			Labels:          map[string]string{clusterLabel: cluster, harnessLabel: "true"},
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: cnpgv1.ScheduledBackupSpec{
			Cluster:  cnpgv1.LocalObjectReference{Name: cluster},
			Schedule: "0 0 2 * * *",
			Method:   cnpgv1.BackupMethodPlugin,
			PluginConfiguration: &cnpgv1.BackupPluginConfiguration{
				Name: "barman-cloud.cloudnative-pg.io",
			},
		},
	}
}

func newEvent(ns, cluster string, n int, uid types.UID) *corev1.Event {
	now := metav1.Now()
	reasons := []struct{ reason, msg string }{
		{"CreatingInstance", "Primary instance (initdb)"},
		{"ClusterIsReady", "Cluster is Ready"},
		{"UpdatingStatus", "Cluster status updated"},
	}
	r := reasons[(n-1)%len(reasons)]
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: fmt.Sprintf("%s.%02d", cluster, n),
			Labels: map[string]string{harnessLabel: "true"},
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: cnpgv1.SchemeGroupVersion.String(), Kind: "Cluster", Namespace: ns, Name: cluster, UID: uid,
		},
		Type: corev1.EventTypeNormal, Reason: r.reason, Message: r.msg, Count: 1,
		FirstTimestamp: now, LastTimestamp: now,
		ReportingController: "cloudnative-pg", ReportingInstance: "cnpg-controller-manager",
		Source: corev1.EventSource{Component: "cloudnative-pg"},
	}
}

// waitRunning polls until want harness Pods are Running with an IP, or the
// timeout; it prints how long KWOK took, which is the "pods settle" figure.
func waitRunning(ctx context.Context, k *kube, prefix string, namespaces, want int, timeout time.Duration) error {
	started := time.Now()
	deadline := time.After(timeout)
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		ready := 0
		for i := 1; i <= namespaces; i++ {
			var pods corev1.PodList
			if err := k.client.List(ctx, &pods, client.InNamespace(nsName(prefix, i)), client.MatchingLabels{harnessLabel: "true"}); err != nil {
				return err
			}
			for j := range pods.Items {
				if pods.Items[j].Status.Phase == corev1.PodRunning && pods.Items[j].Status.PodIP != "" {
					ready++
				}
			}
		}
		if ready >= want {
			fmt.Printf("populate: %d/%d Pods Running with an IP after %s\n", ready, want, time.Since(started).Round(100*time.Millisecond))
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			fmt.Fprintf(os.Stderr, "populate: only %d/%d Pods Running after %s (is KWOK running?)\n", ready, want, timeout)
			return fmt.Errorf("timed out waiting for Pods: %d/%d", ready, want)
		case <-tick.C:
		}
	}
}
