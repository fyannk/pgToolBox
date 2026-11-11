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

package sharedstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Writers of other versions. A document of a newer format version is left
// as it is while a newer console may still be running (a rolling
// upgrade), and taken over once none has shown itself for takeOverAfter
// (a rollback: every replica runs this version, and would otherwise leave
// the documents alone for good, sign-outs unshared and MCP refused).
//
// Every write a replica makes sets the annotation of its own format
// version (writerAnnotation) to a value that differs from the one before
// (the Unix second of the write, or one more than the previous value), so
// that every write by a replica of another version changes what this one
// sees of the others (othersFP). The decision uses only this replica's
// own clock: when it last saw that change. No clock is compared with
// another replica's.
//
// A replica writes its annotation even with nothing else to write (a
// heartbeat, every heartbeatEvery) only while another version may be
// running: while it leaves a newer document alone (it is the older
// replica, and the newer one should know), or while it has seen another
// version's annotation change within takeOverAfter (it is the newer one,
// and the older one must not take over). Replicas of one version alone
// write no heartbeat.
//
// The take-over is safe under optimistic concurrency: it is decided on
// the ConfigMap as read, and written with that read's resourceVersion. A
// newer replica that wrote since changed its annotation, so the write
// conflicts, the re-read shows the change, and the document is left alone
// again.
//
// What a document taken over holds is read as this version's layout and
// merged through the usual validation (a sign-out in it stays refused),
// as the compatibility contract of formatVersion allows; a document of
// takeOverBelow or more is never taken over. grants.json is reset instead:
// a revocation recorded in a newer layout could otherwise be lost and its
// grant come back. The reset is made once a document (grantsReset), so
// that a merge of the same document again cannot erase what was recorded
// since. A replica drops its own grants (not its spent codes) as soon as
// it finds a newer grants document, and the three-way merge of grants
// (mergeGrantsLocked) drops them on every replica that finds the reset
// document, including one that never saw the newer one: no replica
// brings a revoked grant back. MCP assistants then connect again.
//
// An annotation of another version that has not changed for
// takeOverAfter is dropped on this replica's next write (stampLocked).
const (
	// writerAnnotationPrefix, followed by a format version, names the
	// annotation of the ConfigMap that replicas of that version change
	// on every write.
	writerAnnotationPrefix = "pgtoolbox.fyannk.dev/shared-state-format-"
	// heartbeatEvery is how often a replica writes its annotation while
	// another version may be running and it has nothing else to write.
	heartbeatEvery = 10 * time.Minute
	// takeOverAfter is how long no write of another version is seen
	// before a newer document is taken over: three heartbeats, well
	// above the write debounce (10 s), the write and watch backoffs (at
	// most a minute) and tickEvery.
	takeOverAfter = 30 * time.Minute
	// tickEvery is how often the heartbeat and the take-over are
	// checked when nothing is read or written.
	tickEvery = time.Minute
)

// writerAnnotation is the annotation of format version v.
func writerAnnotation(v int) string {
	return writerAnnotationPrefix + strconv.Itoa(v)
}

// newerDocument reports whether raw is a document of a format version
// newer than this console's.
func newerDocument(raw string) bool {
	v, ok := documentVersion(raw)
	return ok && v > formatVersion
}

// takeable reports whether raw, a newer document, keeps the compatibility
// contract (formatVersion) and so may be taken over.
func takeable(raw string) bool {
	v, ok := documentVersion(raw)
	return ok && v > formatVersion && v < takeOverBelow
}

// beat is an annotation of another version as last seen: its value, and
// when (this replica's clock) it last changed.
type beat struct {
	value string
	since time.Time
}

// observeWritersLocked records, before cm's documents are merged, what
// cm shows of the replicas of other versions and of this one, and
// decides whether its newer documents are taken over (s.takeOver).
func (s *Store) observeWritersLocked(cm *corev1.ConfigMap) {
	now := s.opts.Now()
	own := cm.Annotations[writerAnnotation(formatVersion)]
	if own != s.ownBeat || s.ownSince.IsZero() {
		s.ownBeat, s.ownSince = own, now
	}
	sum := sha256.New()
	var keys []string
	for key := range cm.Annotations {
		if strings.HasPrefix(key, writerAnnotationPrefix) && key != writerAnnotation(formatVersion) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	beats := make(map[string]beat, len(keys))
	for _, key := range keys {
		v := cm.Annotations[key]
		sum.Write([]byte(key + "=" + v + "\n"))
		if b, ok := s.othersBeats[key]; ok && b.value == v {
			beats[key] = b
		} else {
			beats[key] = beat{value: v, since: now}
		}
	}
	s.othersBeats = beats
	s.newer = false
	for _, key := range documentKeys {
		if raw := cm.Data[key]; newerDocument(raw) {
			s.newer = true
			sum.Write([]byte(key + "=" + raw + "\n"))
		} else {
			delete(s.takeOverLogged, key)
			if key == KeyGrants {
				s.grantsReset = ""
			}
		}
	}
	fp := ""
	if len(keys) > 0 || s.newer {
		fp = hex.EncodeToString(sum.Sum(nil))
	}
	if fp != s.othersFP || s.othersSince.IsZero() {
		s.othersFP, s.othersSince = fp, now
	}
	s.takeOver = s.newer && now.Sub(s.othersSince) >= takeOverAfter
}

// beatDueLocked reports whether this replica should write its annotation
// although nothing else changed.
func (s *Store) beatDueLocked() bool {
	now := s.opts.Now()
	othersLive := s.othersFP != "" && now.Sub(s.othersSince) < takeOverAfter
	if !s.newer && !othersLive {
		return false
	}
	return s.ownBeat == "" || now.Sub(s.ownSince) >= heartbeatEvery
}

// stampLocked sets this replica's annotation on cm, the ConfigMap about
// to be written: a value different from the one cm holds.
func (s *Store) stampLocked(cm *corev1.ConfigMap) {
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	// An annotation of another version unchanged for takeOverAfter is
	// dropped: that version is gone (the take-over waits as long), and a
	// replica started later would otherwise count it as a live writer and
	// send heartbeats for takeOverAfter after each start. Not while a newer
	// document is left alone or taken over: the change of what this
	// replica sees of the others would restart the take-over's wait.
	if !s.newer {
		for key, b := range s.othersBeats {
			if s.opts.Now().Sub(b.since) >= takeOverAfter && cm.Annotations[key] == b.value {
				delete(cm.Annotations, key)
			}
		}
	}
	key := writerAnnotation(formatVersion)
	next := s.opts.Now().Unix()
	if prev, err := strconv.ParseInt(cm.Annotations[key], 10, 64); err == nil && prev >= next {
		next = prev + 1
	}
	cm.Annotations[key] = strconv.FormatInt(next, 10)
}

// wroteLocked records a write that landed (updated, as the API server
// returned it): this replica's own heartbeat.
func (s *Store) wroteLocked(updated *corev1.ConfigMap) {
	s.ownBeat, s.ownSince = updated.Annotations[writerAnnotation(formatVersion)], s.opts.Now()
	s.landedLocked(updated.Data[KeyGrants])
}

// takingOverLocked reports whether the document raw of key is a newer
// one taken over in this merge, and logs the take-over once.
func (s *Store) takingOverLocked(key, raw string) bool {
	if !s.takeOver || !takeable(raw) {
		return false
	}
	if !s.takeOverLogged[key] {
		s.takeOverLogged[key] = true
		v, _ := documentVersion(raw)
		what := "what it holds that this version reads is merged"
		if key == KeyGrants {
			what = "it is reset (MCP assistants connect again)"
		}
		s.opts.Logger.Warn("shared state: taking over a document of a newer format version: no console of another version "+
			"has written the ConfigMap for "+takeOverAfter.String()+" (a rollback); "+what,
			slog.String("configmap", s.opts.Name), slog.String("document", key), slog.Int("version", v), slog.Int("writes", formatVersion))
	}
	return true
}

// tick checks, when nothing was read or written for a while, whether a
// heartbeat is due or a newer document is now to be taken over.
func (s *Store) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.base == nil || s.mode == modeMissing || s.mode == modeForbidden {
		return
	}
	if s.newer && !s.takeOver && s.opts.Now().Sub(s.othersSince) >= takeOverAfter {
		// The documents are merged again, this time taken over; the
		// merge marks the write due.
		s.mergeLocked(s.base)
		return
	}
	if s.beatDueLocked() {
		s.changedLocked(false)
	}
}

// ticker runs tick every tickEvery until ctx ends.
func (s *Store) ticker(ctx context.Context) {
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick()
		}
	}
}
