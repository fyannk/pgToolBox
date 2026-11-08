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

package viewmodel

import "time"

// LogTail is the bounded tail of one container's log, read on demand
// (the LogViewer's first page; the live follow is the logs stream).
// The console keeps no log corpus: each request reads the API server's
// pods/log once, bounded in lines and bytes.
type LogTail struct {
	Ref       ClusterRef `json:"ref"`
	Pod       string     `json:"pod"`
	Container string     `json:"container"`
	// Containers are the pod's containers, for the selector.
	Containers []string  `json:"containers"`
	ReadAt     time.Time `json:"readAt" doc:"When the console read the tail."`
	Lines      []LogLine `json:"lines"`
	// Truncated reports that the byte bound cut the tail: older lines
	// exist that were not read.
	Truncated bool `json:"truncated,omitempty"`
	// Previous reports the tail of the previous (crashed) container.
	Previous bool `json:"previous,omitempty"`
}

// LogLine is one log line. At is the container runtime's timestamp
// (kubelet), absent when the line carried none; the database's own
// timestamp stays part of Text.
type LogLine struct {
	At   *time.Time `json:"at,omitempty"`
	Text string     `json:"text"`
}

// LogStreamLine is the data of a "line" event on the logs stream.
type LogStreamLine struct {
	At   *time.Time `json:"at,omitempty"`
	Text string     `json:"text"`
}

// LogStreamGap is the data of a "gap" event: lines were missed (a
// reconnect, a rotation, a restart, a slow client); the viewer draws a
// marker and never joins across it.
type LogStreamGap struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// LogStreamEnd is the data of the "end" event: the stream is over (the
// container ended, the bound was reached, the session ends).
type LogStreamEnd struct {
	Reason string `json:"reason"`
}
