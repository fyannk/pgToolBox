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

// Package kube converts the CloudNativePG and Kubernetes objects the
// console's informer cache holds into the observe facts pkg/diagnose reads
// (Facts) and into history observations (Observation). It performs no I/O:
// the console reads through the label-filtered cache of
// internal/console/cache.
package kube

import (
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// target names the one Cluster the converters judge membership against.
type target struct {
	// Namespace is the Cluster's namespace.
	Namespace string
	// ClusterName is the Cluster's name.
	ClusterName string
}

// Client holds the converters' target and the logger they report
// excluded objects to. It has no API client.
type Client struct {
	opts   target
	logger *slog.Logger
	// now is the instant the converters judge times against (a Backup
	// time in the future is not plausible: observe.SaneBackupTimes).
	now time.Time
}

// clusterGVR is the CloudNativePG Cluster resource.
var clusterGVR = schema.GroupVersionResource{
	Group:    "postgresql.cnpg.io",
	Version:  "v1",
	Resource: "clusters",
}
