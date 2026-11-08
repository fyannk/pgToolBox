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

package kube

import (
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// maxEventMessage bounds an event message at the boundary.
const maxEventMessage = 1024

// convertEvent converts a raw event and reports whether it is a
// candidate of the cluster. A malformed event is not a candidate.
func (c *Client) convertEvent(content map[string]any) (observe.EventFacts, bool) {
	var event corev1.Event
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(content, &event); err != nil {
		return observe.EventFacts{}, false
	}
	if !c.eventIsCandidate(&event) {
		return observe.EventFacts{}, false
	}

	message := event.Message
	if len(message) > maxEventMessage {
		message = message[:maxEventMessage]
	}
	count := int(event.Count)
	if event.Series != nil && int(event.Series.Count) > count {
		count = int(event.Series.Count)
	}
	if count < 1 {
		count = 1
	}

	return observe.EventFacts{
		Name:     event.Name,
		UID:      string(event.UID),
		Kind:     event.InvolvedObject.Kind,
		Object:   event.InvolvedObject.Name,
		Type:     event.Type,
		Reason:   event.Reason,
		Message:  message,
		Count:    count,
		LastSeen: eventTime(&event),
	}, true
}

// eventIsCandidate keeps events involving the cluster object itself,
// Pod-kind objects carrying the cluster name prefix, and the operator's
// Backup, ScheduledBackup and Pooler objects. The prefix and the kinds
// are selection only; rendering admits pod events solely for verified
// members, and the secondary kinds solely for objects the respective
// catalog lists as this cluster's.
func (c *Client) eventIsCandidate(event *corev1.Event) bool {
	involved := event.InvolvedObject
	if involved.Namespace != "" && involved.Namespace != c.opts.Namespace {
		return false
	}
	group, _, ok := splitAPIVersion(involved.APIVersion)
	if involved.Kind == "Cluster" && ok && group == clusterGVR.Group {
		return involved.Name == c.opts.ClusterName
	}
	if ok && group == clusterGVR.Group {
		switch involved.Kind {
		case "Backup", "ScheduledBackup", "Pooler":
			return true
		}
	}
	if involved.Kind == "Pod" {
		return involved.Name == c.opts.ClusterName ||
			strings.HasPrefix(involved.Name, c.opts.ClusterName+"-")
	}
	return false
}

// eventTime returns the most recent occurrence time the API server
// reports, falling back through the event's timestamp fields.
func eventTime(event *corev1.Event) time.Time {
	switch {
	case !event.LastTimestamp.IsZero():
		return event.LastTimestamp.Time
	case event.Series != nil && !event.Series.LastObservedTime.IsZero():
		return event.Series.LastObservedTime.Time
	case !event.EventTime.IsZero():
		return event.EventTime.Time
	case !event.FirstTimestamp.IsZero():
		return event.FirstTimestamp.Time
	default:
		return event.CreationTimestamp.Time
	}
}
