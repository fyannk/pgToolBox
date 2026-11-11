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

// Package sharedstate keeps the few facts every replica of a console must
// agree on, and that must survive a restart even without a history volume,
// in one ConfigMap the console owns
// (<console>-state, STATE_CONFIGMAP):
//
//   - signed-out sessions: the SHA-256 of each session ID signed out on
//     any replica, with the session's absolute expiry, when it was
//     recorded and a keyed hash of the person (signouts below). A copy of
//     the cookie of a signed-out session is refused by every replica, and
//     after a restart, until it would have expired anyway. The session
//     ID itself is never written.
//   - "since" times: when an instance's status read entered its state,
//     when a Cluster entered its phase, when a diagnostics finding was
//     first seen. Keyed by object UID, so a Cluster or pod recreated
//     under the same name inherits nothing.
//   - digest delivery marks: which channel delivered which daily digest,
//     and when. Only the replica holding the digest Lease
//     delivers; the marks it leaves here let the next leader, after a
//     roll or a failover, send only what was not sent.
//   - MCP clients' console grants (grants.go): the index of
//     grants (ID, a hash of the user, the client's declared name, scope,
//     times, refresh generation, revocation) and the authorization codes
//     already redeemed, so a revoked grant, a reused refresh token or a
//     replayed code is refused by every replica.
//   - restore drill results (drills.go): the outcome of each finished
//     drill (drill, source, backup, result, times, the console's reason),
//     so a restart without a history volume does not lose the evidence
//     that a restore was tested.
//
// Merge rule, the same on every replica, so they converge: for one key
// and the same state the earliest observation wins; a different state
// with a later time replaces the entry (it is the newer transition); an
// entry whose object is gone from the cache is pruned; expired sign-outs
// are pruned; a delivery mark, once set, stays (the earliest time wins)
// until it is older than the digest retention. Writes are batched (a
// sign-out or a delivery mark within about a second, the rest within
// ten), carry the resourceVersion read (a conflict is read again, merged
// and retried), and stay under a size budget well below the 1 MiB object
// limit: over it, the oldest "since" entries are dropped first, never a
// sign-out, a delivery mark, a grant or a redeemed code; and the whole
// ConfigMap stays within MaxData, past which the oldest restore drill
// results go.
//
// The ConfigMap is optional. Missing or forbidden, the console keeps
// today's local behaviour (each replica its own clocks and sign-outs),
// logs one warning and says so on the admin page; it never fails the
// start.
package sharedstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Data keys of the ConfigMap.
const (
	KeySessions = "sessions.json"
	KeySince    = "since.json"
	KeyDigests  = "digests.json"
	// formatVersion is the version of every document this console
	// writes. It changes whenever a field, a merge rule or the ordering
	// rule of a bound changes, so that replicas never decide one document
	// by two rules: 2 since the sign-outs' fair-share tie-break and the
	// codes' marks (MarkCode), which a replica of version 1 would drop on
	// its next write.
	//
	// A document of an older version, from minFormatVersion on, is read as
	// this version (readableVersion says how), and so rewritten at this
	// version on the next write: the document is taken over, and a
	// replica still of the older version, during a rolling upgrade, then
	// finds it newer and leaves it alone. A document of a newer version (a
	// newer console, during a rolling upgrade or after a rollback) is
	// foreign while a console of another version may still be running:
	// neither merged nor rewritten, logged once and counted in
	// pgtoolbox_console_shared_state_foreign_documents. Once none has
	// written for takeOverAfter (a rollback, writers.go), a newer document
	// below takeOverBelow is taken over: read in this version's layout,
	// and rewritten at this version. grants.json is reset instead of read.
	//
	// The compatibility contract every later version keeps, since this
	// version reads its documents so: a document of a version from 3 to
	// takeOverBelow-1 holds what this version reads in this version's
	// fields, with their meaning (sessions.json: "signedOut", keyed by the
	// session hash, each entry's "e", "r" and "s"; since.json: "facts";
	// digests.json: "sent"; drills.json: "since" and "results"), adds
	// fields rather than renaming or re-keying them, and holds at most
	// maxEntries entries in each. A version that cannot keep it is
	// numbered takeOverBelow or more: this version then never takes that
	// document over (it stays foreign until its key is deleted), rather
	// than read it wrong and overwrite it with less
	// (TestTheFormatContract).
	formatVersion = 2
	// takeOverBelow bounds the newer versions a take-over reads as this
	// one (the contract above).
	takeOverBelow = 100
	// minFormatVersion is the oldest version this console reads.
	minFormatVersion = 1
)

// Bounds.
const (
	// DefaultBudget is the bytes the documents may take together:
	// three quarters of the API server's 1 MiB object limit, which also
	// counts the metadata.
	DefaultBudget = 768 << 10
	// maxEntries bounds what one document may hold when read back.
	maxEntries = 50_000
	// maxKey bounds a fact key: a kind, a UID, a finding ID.
	maxKey = 320
	// offKeep is how long a cleared finding is remembered as cleared, so
	// a replica that has not run yet does not bring back its old "since".
	offKeep = 24 * time.Hour
	// pruneEvery is how often the facts of objects gone from the cache
	// are pruned.
	pruneEvery = time.Minute
	// writeTimeout bounds one read-merge-write.
	writeTimeout = 10 * time.Second
	// maxAttempts bounds the conflict retries of one write.
	maxAttempts = 5
	// rewoundAfter is how many Gets in a row older than what a Get or
	// this replica's own write showed before make the older one
	// authoritative (mergeReadLocked).
	rewoundAfter = 2
	// DefaultDigestRetention is how long a delivery mark is kept when
	// Options.DigestRetention is not set: the longest DIGEST_RETENTION.
	DefaultDigestRetention = 366 * 24 * time.Hour
	// sentSlack keeps a mark a little longer than its digest: a digest is
	// delivered after its window closes, so its mark is younger than it.
	sentSlack = 48 * time.Hour
	// maxDigestID and maxChannel bound the two parts of a mark's key.
	maxDigestID = 64
	maxChannel  = 16
	// The sessions document is bounded three ways. A person at their own
	// bound replaces their own entry that ends first with the new one
	// (the newest sign-out is the one a fresh cookie theft needs). The
	// sessions naming nobody refuse what arrives past their bound. The
	// document, once full, is shared fairly (fairShares): a newcomer's
	// entry takes the place of the last entry, in signOutBefore (the one
	// recorded longest ago), of the person holding the most, when that
	// person holds more than the newcomer (among several holding as many,
	// the one whose entry was recorded longest ago): everyone keeps their latest
	// sign-outs, so filling it with many identities cannot lock everyone
	// else out; a newcomer holding as much as the most refuses or
	// replaces their own instead. A sign-out the document does not keep
	// is still refused by the replica that recorded it, is counted in
	// pgtoolbox_console_shared_state_signouts_refused_total (reason
	// replaced for a person's own entry given up, unnamed or budget for a
	// new one refused, evicted for another person's taken for a newcomer,
	// merge for one a merge dropped) and, for the budget, an eviction and
	// a merge, logged.
	//
	// MaxSignOutsPerSubject bounds one person's entries (two a sign-out:
	// the session and its token as a raw bearer), so one person signing
	// in and out in a loop fills only their own share. The person is the
	// one the session was signed in as (sealed in the cookie), not what
	// the API server says at sign-out, so a token revoked before signing
	// out still counts against its owner.
	MaxSignOutsPerSubject = 32
	// MaxUnnamedSignOuts bounds the entries of sessions that name nobody
	// (a session sealed without its person's tag), all together.
	MaxUnnamedSignOuts = 4 * MaxSignOutsPerSubject
	// MaxSignOuts bounds the sessions document (151 to 163 bytes an
	// entry encoded, so about 232 to 250 KB), rather than growing the
	// ConfigMap past what the API server accepts and failing every write;
	// MaxData bounds the ConfigMap as a whole.
	MaxSignOuts = 1536
	// MaxData bounds the bytes of the ConfigMap's data, every document
	// counted (keys and values, another version's included): the API
	// server refuses a ConfigMap whose values pass 1 MiB, and every write
	// would then fail. The margin leaves room for the keys and for what
	// the API server adds. Past it, the oldest drill results are dropped
	// (budgetLocked).
	MaxData = 1<<20 - 16<<10
)

// Fact kinds, the first segment of a key.
const (
	kindInstance = "i"
	kindPhase    = "p"
	kindFinding  = "f"
	findingOn    = "on"
	findingOff   = "off"
)

// ConfigMapClient is the slice of the ConfigMap client the store needs:
// get, watch and update, matching the name-scoped Role.
type ConfigMapClient interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ConfigMap, error)
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
	Update(ctx context.Context, cm *corev1.ConfigMap, opts metav1.UpdateOptions) (*corev1.ConfigMap, error)
}

// Options configure a Store.
type Options struct {
	Client ConfigMapClient
	// Name is the ConfigMap in the console's namespace; empty disables
	// sharing (each replica keeps its own state, as before).
	Name string
	// Live lists the UIDs of the objects the cache holds (Clusters and
	// their pods); false when the list is not complete (cache not
	// synced, a namespace unreadable), and nothing is pruned then.
	Live func(ctx context.Context) (map[string]bool, bool)
	// Budget bounds the documents' size; DefaultBudget when 0.
	Budget int
	Now    func() time.Time
	Logger *slog.Logger
	// Debounce and UrgentDebounce batch writes (10 s, 1 s by default).
	Debounce, UrgentDebounce time.Duration
	// Backoff is the wait before the n-th retry of a failed watch or
	// write.
	Backoff func(attempt int) time.Duration
	// DigestRetention is how long digests are kept (DIGEST_RETENTION);
	// a delivery mark goes some time after its digest.
	// DefaultDigestRetention when 0.
	DigestRetention time.Duration
}

// fact is one "since" entry: the hash of the state and when it began.
type fact struct {
	State string `json:"s"`
	Since int64  `json:"t"`
}

// signOut is one entry of the sessions document: its expiry (Unix
// seconds), its slot, and the keyed tag of the person it belongs to
// (SubjectTag), empty when the session named nobody; the tag only groups
// a person's entries for MaxSignOutsPerSubject.
//
// The slot is the entry's place in the one order every bound keeps by
// (signOutBefore): when it was first recorded (Recorded, the latest
// first) and a hash (Slot; empty for the entry's own session hash). A person at their
// bound gives the new entry the slot of their own entry it replaces,
// one generation (Gen) later: the slots the document holds, and so where
// everyone else's entries stand, do not change, and a replica that still
// holds the replaced entry drops it on merging (the later generation of
// a slot wins).
type signOut struct {
	Expiry   int64  `json:"e"`
	Recorded int64  `json:"r,omitempty"`
	Subject  string `json:"u,omitempty"`
	Slot     string `json:"s,omitempty"`
	Gen      int64  `json:"g,omitempty"`
}

// slotOf is the hash part of the slot of h's entry e.
func slotOf(h string, e signOut) string {
	if e.Slot != "" {
		return e.Slot
	}
	return h
}

// slotBefore orders two (hash, entry) pairs by slot, the latest recorded
// first, and within a slot the later generation first, then by hash: a
// total order, of the entries as they were recorded, which the merge
// keeps by (mergeSignOut) and which no replica's clock changes. The bounds
// keep by signOutBefore, the same order with the recording times this
// replica takes for the future read as none.
func slotBefore(ah string, a signOut, bh string, b signOut) bool {
	if a.Recorded != b.Recorded {
		return a.Recorded > b.Recorded
	}
	if as, bs := slotOf(ah, a), slotOf(bh, b); as != bs {
		return as < bs
	}
	if a.Gen != b.Gen {
		return a.Gen > b.Gen
	}
	return ah < bh
}

// maxSignOutGen bounds a slot's generation read back.
const maxSignOutGen = 1 << 40

// maxRecordedSkew is how far ahead of this replica's clock a sign-out's
// recording time read back may be (another replica's clock running
// ahead); past it, the bounds order the entry as unrecorded
// (signOutBefore). The time itself is kept as it was read: rewriting it
// would make the replicas whose clocks disagree rewrite each other's
// copy for as long as they disagree. What each replica's clock still
// decides alone (an entry's end, and which entries a full document keeps
// when one's time lies between two replicas' bounds) can make them
// rewrite each other's copy of that entry for at most the length of their
// disagreement.
const maxRecordedSkew = 5 * time.Minute

// latestRecordedLocked is the latest recording time this replica's bounds
// take as given (signOutBefore).
func (s *Store) latestRecordedLocked() int64 {
	return s.opts.Now().Add(maxRecordedSkew).Unix()
}

// orderedRecorded is the recording time r as the bounds order it: none
// (0, last) past latest.
func orderedRecorded(r, latest int64) int64 {
	if r > latest {
		return 0
	}
	return r
}

// validSignOut checks the slot of h's entry read from the ConfigMap: its
// own (generation 0), or another's at a later generation.
func validSignOut(h string, e signOut) bool {
	if !validSubjectTag(e.Subject) || e.Gen < 0 || e.Gen > maxSignOutGen {
		return false
	}
	if e.Slot == "" {
		return e.Gen == 0
	}
	return e.Gen > 0 && e.Slot != h && validSessionHash(e.Slot)
}

// mergeSignOut folds two observations of session h's sign-out: the
// latest expiry, the slot that comes first in slotBefore (the latest
// recorded; an entry without a recording time comes last), and a tag when
// either has one (the smaller when they differ). It is commutative,
// associative and idempotent, so replicas folding the same observations
// in any order hold the same entry.
func mergeSignOut(h string, a, b signOut) signOut {
	out := a
	if slotBefore(h, b, h, a) {
		out.Recorded, out.Slot, out.Gen = b.Recorded, b.Slot, b.Gen
	}
	out.Expiry = max(a.Expiry, b.Expiry)
	out.Subject = a.Subject
	if out.Subject == "" || (b.Subject != "" && b.Subject < out.Subject) {
		out.Subject = b.Subject
	}
	return out
}

type sessionsDoc struct {
	Version int `json:"version"`
	// SignedOut maps the hex SHA-256 of a session ID to the session's
	// absolute expiry (Unix seconds).
	SignedOut map[string]signOut `json:"signedOut"`
}

type sinceDoc struct {
	Version int             `json:"version"`
	Facts   map[string]fact `json:"facts"`
}

type digestsDoc struct {
	Version int `json:"version"`
	// Sent maps "<digest ID>/<channel>" to when that channel delivered
	// that digest (Unix seconds).
	Sent map[string]int64 `json:"sent"`
}

// mode is the store's connection to its ConfigMap.
type mode string

const (
	modeDisabled  mode = "disabled"
	modePending   mode = "pending"
	modeLive      mode = "live"
	modeMissing   mode = "missing"
	modeForbidden mode = "forbidden"
	modeFailing   mode = "failing"
)

// Store is the shared state. A nil *Store is valid and shares nothing.
type Store struct {
	opts Options

	// writes serializes the writes of the ConfigMap: a batch (flush) and
	// a write-through (SpendCode, MarkCode, WriteRevocations) never run at
	// once. Taken before mu, never under it.
	writes    sync.Mutex
	mu        sync.Mutex
	signedOut map[string]signOut
	facts     map[string]fact
	// sent holds the digest delivery marks, keyed "<digest ID>/<channel>".
	sent map[string]int64
	// grants and codes are grants.json (grants.go).
	grants map[string]grantEntry
	codes  map[string]codeEntry
	// drills and drillsSince are drills.json (drills.go).
	drills      map[string]DrillResult
	drillsSince int64
	// byObject indexes the fact keys by object UID, for pruning and for
	// a Cluster's findings.
	byObject map[string]map[string]struct{}
	// base is the ConfigMap as last read or written; nil until read.
	base *corev1.ConfigMap
	// rewound counts the Gets in a row older than the base, and
	// rewoundLogged is set once such an episode is logged
	// (mergeReadLocked).
	rewound       int
	rewoundLogged bool
	// uid is the UID of the ConfigMap last merged: another one is the
	// ConfigMap deleted and recreated (recreatedLocked); seenRV the
	// resourceVersion of the last base, kept when the ConfigMap is
	// deleted (olderLocked).
	uid    types.UID
	seenRV string
	// foreign names the keys left untouched: of a format version this
	// console does not read, or (grants.json) unreadable.
	foreign map[string]bool
	// foreignLogged is the version of each foreign key last logged, so
	// that each is logged once.
	foreignLogged map[string]int
	// The replicas of other versions (writers.go): newer is set while
	// the ConfigMap holds a document of a newer version, takeOver once
	// those are taken over; othersFP is what the ConfigMap shows of the
	// other versions' writes and othersSince when (this replica's clock)
	// that last changed; ownBeat is this version's annotation and
	// ownSince when it last changed; takeOverLogged names the documents
	// whose take-over was logged.
	newer, takeOver       bool
	othersFP, ownBeat     string
	othersSince, ownSince time.Time
	takeOverLogged        map[string]bool
	// othersBeats is, per annotation of another version, its value and
	// when (this replica's clock) it last changed: one unchanged for
	// takeOverAfter is dropped on this replica's next write.
	othersBeats map[string]beat
	// grantsReset is the SHA-256 of the newer grants document whose
	// take-over reset the grants: the reset is made once a document, not
	// again on each merge of it until the take-over is written.
	grantsReset string
	// recorded holds the grants this replica recorded (AddGrant, a
	// revocation placeholder) that no document read has held yet, and when
	// (this replica's clock); unwritten the revocations made here that no
	// document read has held yet (mergeGrantsLocked).
	recorded  map[string]time.Time
	unwritten map[string]bool
	// unversionedLogged names the documents logged as carrying no format
	// version (versionedLocked).
	unversionedLogged map[string]bool
	// gen counts local changes; written is the gen last written.
	gen, written uint64
	urgent       bool
	live         map[string]bool
	mode         mode
	reason       string
	since        time.Time
	// overBudget is set while the budget forces entries out (logged once
	// per episode).
	overBudget bool
	// signOutsFull is set while MaxSignOuts refuses or evicts sign-outs
	// (logged once per episode).
	signOutsFull bool
	// overCeiling is set while MaxData forces drill results out (logged
	// once per episode).
	overCeiling bool
	// mergeDropLogged is set once a merge dropped sign-outs (logged once).
	mergeDropLogged bool
	// grantsUnreadable is set while the grants document does not parse
	// (logged once per episode).
	grantsUnreadable bool
	wake             chan struct{}
}

// New builds a store; nothing is read until Run.
func New(opts Options) *Store {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Budget <= 0 {
		opts.Budget = DefaultBudget
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 10 * time.Second
	}
	if opts.UrgentDebounce <= 0 {
		opts.UrgentDebounce = time.Second
	}
	if opts.Backoff == nil {
		opts.Backoff = func(attempt int) time.Duration { return min(time.Second<<min(attempt, 6), time.Minute) }
	}
	if opts.DigestRetention <= 0 {
		opts.DigestRetention = DefaultDigestRetention
	}
	s := &Store{
		opts: opts, signedOut: map[string]signOut{}, facts: map[string]fact{}, sent: map[string]int64{}, grants: map[string]grantEntry{}, codes: map[string]codeEntry{}, drills: map[string]DrillResult{}, byObject: map[string]map[string]struct{}{},
		foreign: map[string]bool{}, foreignLogged: map[string]int{}, takeOverLogged: map[string]bool{}, othersBeats: map[string]beat{},
		recorded: map[string]time.Time{}, unwritten: map[string]bool{}, unversionedLogged: map[string]bool{}, mode: modePending, since: opts.Now().UTC(), wake: make(chan struct{}, 1),
	}
	if opts.Name == "" {
		s.mode = modeDisabled
	}
	return s
}

// Enabled reports whether a ConfigMap is configured.
func (s *Store) Enabled() bool { return s != nil && s.opts.Name != "" }

// hashState shortens a state to a fixed-size, text-free token: equality
// is all a merge needs, and a status reason never reaches the ConfigMap.
func hashState(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:8])
}

// sessionHash is what is stored of a session ID: 128 bits of its hash
// (a session ID is 128 random bits; the hash need only tell sessions
// apart, and short keys keep MaxSignOuts small in bytes).
func sessionHash(sid string) string {
	sum := sha256.Sum256([]byte(sid))
	return hex.EncodeToString(sum[:16])
}

// ---------------------------------------------------------------- sessions

// SubjectTagLen is the length of a person's tag in the sessions
// document: the 32-hex keyed hash the sign-in layer derives from the
// person's name and the session secret (auth.Codec.KeyedHashes), so
// that reading the ConfigMap cannot test a guessed name.
const SubjectTagLen = 32

func validSubjectTag(tag string) bool {
	if tag == "" {
		return true
	}
	if len(tag) != SubjectTagLen {
		return false
	}
	_, err := hex.DecodeString(tag)
	return err == nil
}

// Reasons a sign-out is not kept (signOutsRefused).
const (
	refusedSubject  = "subject"
	refusedUnnamed  = "unnamed"
	refusedBudget   = "budget"
	refusedReplaced = "replaced"
	refusedEvicted  = "evicted"
	refusedMerge    = "merge"
	// superseded is a merge's drop of an entry whose slot a later
	// generation holds: a replacement reaching this replica, not counted
	// again.
	superseded = "superseded"
	// collided is a merge's drop of one of two entries holding the same
	// generation of one slot: two replicas each replaced the same entry
	// of a person at their bound before syncing, so one sign-out more
	// than the person's share reached the document. Each replica had
	// accepted its own, so the drop is counted (under merge).
	collided = "collided"
)

// RecordSignOut shares a signed-out session until its absolute expiry.
// tag is the person's keyed tag sealed in the session (SubjectTagLen hex;
// anything else, empty included, files it with the sessions that name
// nobody). It reports false when a bound of the sessions document
// refuses it (it is not shared): the caller still refuses the session
// itself. A person at their own bound, or holding as much as the most
// once the document is full, gives up their own entry that ends first
// for it; with the document full, a person holding less takes the place
// of an entry of the person holding the most (fairShares).
func (s *Store) RecordSignOut(sid, tag string, until time.Time) bool {
	if s == nil || sid == "" {
		return true
	}
	if !validSubjectTag(tag) {
		tag = ""
	}
	h := sessionHash(sid)
	s.mu.Lock()
	defer s.mu.Unlock()
	pruned := s.pruneSignOutsLocked()
	if prev, ok := s.signedOut[h]; ok {
		if until.Unix() > prev.Expiry {
			prev.Expiry = until.Unix()
			s.signedOut[h] = prev
			s.changedLocked(true)
		} else if pruned {
			s.changedLocked(false)
		}
		return true
	}
	e := signOut{Expiry: until.Unix(), Recorded: s.opts.Now().Unix(), Subject: tag}
	first, held := s.ownEntriesLocked(tag)
	limit, reason := MaxSignOutsPerSubject, refusedSubject
	if tag == "" {
		limit, reason = MaxUnnamedSignOuts, refusedUnnamed
	}
	victim, replace := "", false
	switch {
	case held >= limit:
		replace = tag != ""
	case len(s.signedOut) >= MaxSignOuts:
		reason = refusedBudget
		if v, ok := s.evictionLocked(h, e); ok {
			victim, reason = v, ""
		} else {
			replace = tag != "" && held > 0
		}
	default:
		reason = ""
	}
	switch {
	case replace:
		// The new entry takes the slot of the person's entry that ends
		// first, so the document's slots, and everyone else's place in
		// them, stay as they were.
		v := s.signedOut[first]
		e.Recorded, e.Slot, e.Gen = v.Recorded, slotOf(first, v), v.Gen+1
		delete(s.signedOut, first)
		s.signedOut[h] = e
		signOutsRefused.WithLabelValues(refusedReplaced).Inc()
		s.changedLocked(true)
		return true
	case reason != "":
		signOutsRefused.WithLabelValues(reason).Inc()
		if reason == refusedBudget {
			s.fullLocked()
		}
		if pruned {
			s.changedLocked(false)
		}
		return false
	}
	if victim != "" {
		delete(s.signedOut, victim)
		signOutsRefused.WithLabelValues(refusedEvicted).Inc()
		s.fullLocked()
	}
	s.signedOut[h] = e
	s.changedLocked(true)
	return true
}

// fullLocked logs, once per episode, that the sessions document is full.
func (s *Store) fullLocked() {
	if s.signOutsFull {
		return
	}
	s.signOutsFull = true
	s.opts.Logger.Warn("shared state: the sessions document is full; a new sign-out takes the place of an entry of the person "+
		"holding the most, or, from a person holding as much, is refused (or replaces their own) on this replica only: "+
		"a copy of a cookie whose sign-out is not shared is accepted by the other replicas until it expires",
		slog.String("configmap", s.opts.Name), slog.Int("max", MaxSignOuts))
}

// ownEntriesLocked returns how many entries tag holds and, for a person
// (a non-empty tag), their entry that ends first (then the first in slot
// order): the one a new sign-out of theirs replaces. The sessions naming
// nobody are not one person, and are never replaced.
func (s *Store) ownEntriesLocked(tag string) (string, int) {
	held, first, latest := 0, "", s.latestRecordedLocked()
	for h, e := range s.signedOut {
		if e.Subject != tag {
			continue
		}
		held++
		if tag == "" {
			continue
		}
		if f, ok := s.signedOut[first]; first == "" || !ok || e.Expiry < f.Expiry || (e.Expiry == f.Expiry && signOutBefore(h, e, first, f, latest)) {
			first = h
		}
	}
	return first, held
}

// evictionLocked returns, for a new entry e of session h in a full
// document, the entry that leaves for it: what planSignOutCaps drops from
// the document with the new entry added. ok is false when that is the new
// entry itself, or another entry of its own person (who then replaces
// their own instead). The document is always capped, so exactly one entry
// leaves.
func (s *Store) evictionLocked(h string, e signOut) (string, bool) {
	all := append(signOutEntries(s.signedOut, 1), signOutEntry{h, e})
	for victim, reason := range planSignOutEntries(all, s.latestRecordedLocked()) {
		if victim == h || reason != refusedBudget || s.signedOut[victim].Subject == e.Subject {
			return "", false
		}
		return victim, true
	}
	return "", false
}

// fairShares divides n places among tags holding counts entries (each
// already within its own bound): when they do not fit, every tag keeps
// min(count, L) for the largest level L that fits, and the places left
// go one each to the tags holding more, first in before(a, b, L) (the
// sessions naming nobody last): planSignOutCaps orders them by the entry
// each would keep with the extra place, the latest recorded first, so the
// one left out is the entry recorded longest ago among the tied people,
// never by who they are. It is a function of the counts and of before
// alone, so every replica computes the same shares, and it takes from the
// tags holding the most first: a tag never loses an entry while another
// holding fewer keeps one more than that tag's share.
func fairShares(counts map[string]int, n int, before func(a, b string, level int) bool) map[string]int {
	total, most := 0, 0
	for _, c := range counts {
		total += c
		most = max(most, c)
	}
	out := make(map[string]int, len(counts))
	if total <= n {
		for t, c := range counts {
			out[t] = c
		}
		return out
	}
	at := func(level int) int {
		sum := 0
		for _, c := range counts {
			sum += min(c, level)
		}
		return sum
	}
	// The largest level that fits: at(0) = 0 <= n < at(most).
	lo, hi := 0, most
	for hi-lo > 1 {
		if mid := (lo + hi) / 2; at(mid) <= n {
			lo = mid
		} else {
			hi = mid
		}
	}
	above := make([]string, 0, len(counts))
	for t, c := range counts {
		out[t] = min(c, lo)
		if c > lo {
			above = append(above, t)
		}
	}
	sort.Slice(above, func(i, j int) bool {
		if (above[i] == "") != (above[j] == "") {
			return above[j] == ""
		}
		return before(above[i], above[j], lo)
	})
	for _, t := range above[:n-at(lo)] {
		out[t]++
	}
	return out
}

// pruneSignOutsLocked drops the sign-outs that ended, reporting whether
// there were any.
func (s *Store) pruneSignOutsLocked() bool {
	now := s.opts.Now().Unix()
	pruned := false
	for h, e := range s.signedOut {
		if e.Expiry <= now {
			delete(s.signedOut, h)
			pruned = true
		}
	}
	return pruned
}

// signOutBefore is the one order every bound of the sessions document
// keeps by: by slot, the latest recorded first, then by hash
// (slotBefore), a recording time past latest (a clock far ahead, or an
// entry written by hand) read as none, so that such an entry comes last
// in every bound instead of first in all of them for as long as its clock
// says; it still revokes its session. The
// per-person bounds keep each person's entries that come first in it,
// and the document's fair share keeps each person's first shares (a
// function of how many entries each person holds, nothing else): so,
// the order being total and the same for every bound, replicas that
// merge the same entries in any order keep the same ones (a union of
// capped documents caps to what the union of the uncapped ones does). A
// slot holds one entry, its latest generation: keeping the first of each
// slot in a fixed order is the same kind of rule, and the bounds see
// slots only, so a person's replacement (a later generation of their own
// slot) moves nobody.
func signOutBefore(ah string, a signOut, bh string, b signOut, latest int64) bool {
	if ar, br := orderedRecorded(a.Recorded, latest), orderedRecorded(b.Recorded, latest); ar != br {
		return ar > br
	}
	return slotBefore(ah, a, bh, b)
}

// planSignOutCaps returns the entries of m the bounds drop, by hash, with
// the bound that drops each: an earlier generation of a slot (superseded)
// or a second entry of its generation (collided), per person
// MaxSignOutsPerSubject, the entries naming nobody MaxUnnamedSignOuts
// together, each keeping what comes first in signOutBefore, then the
// document MaxSignOuts, each person keeping their first fairShares.
func planSignOutCaps(m map[string]signOut, latest int64) map[string]string {
	return planSignOutEntries(signOutEntries(m, len(m)), latest)
}

// signOutEntry is one entry of the sessions document with its hash.
type signOutEntry struct {
	hash string
	signOut
}

// signOutEntries lists m, with room for extra more.
func signOutEntries(m map[string]signOut, extra int) []signOutEntry {
	all := make([]signOutEntry, 0, len(m)+extra)
	for h, e := range m {
		all = append(all, signOutEntry{h, e})
	}
	return all
}

// planSignOutEntries is planSignOutCaps of the entries all (distinct
// hashes), which it sorts. The entries of one slot (one recording time,
// one slot hash) stay next to each other in signOutBefore: it orders by
// the recording time read as given or as none, then as recorded.
func planSignOutEntries(all []signOutEntry, latest int64) map[string]string {
	sort.Slice(all, func(i, j int) bool {
		return signOutBefore(all[i].hash, all[i].signOut, all[j].hash, all[j].signOut, latest)
	})
	dropped := map[string]string{}
	// kept holds, per tag, the positions in all of the entries it keeps,
	// so in signOutBefore.
	kept := map[string][]int{}
	head := 0
	for i, e := range all {
		if i > 0 && e.Recorded == all[head].Recorded && slotOf(e.hash, e.signOut) == slotOf(all[head].hash, all[head].signOut) {
			if e.Gen < all[head].Gen {
				dropped[e.hash] = superseded
			} else {
				dropped[e.hash] = collided
			}
			continue
		}
		head = i
		limit, reason := MaxSignOutsPerSubject, refusedSubject
		if e.Subject == "" {
			limit, reason = MaxUnnamedSignOuts, refusedUnnamed
		}
		if len(kept[e.Subject]) >= limit {
			dropped[e.hash] = reason
			continue
		}
		kept[e.Subject] = append(kept[e.Subject], i)
	}
	counts := make(map[string]int, len(kept))
	for t, at := range kept {
		counts[t] = len(at)
	}
	// Ties at the level go to the person whose next entry, the one the
	// extra place keeps, comes first in signOutBefore (the latest
	// recorded): every tag in the contest holds more than level entries.
	newestNext := func(a, b string, level int) bool { return kept[a][level] < kept[b][level] }
	for t, share := range fairShares(counts, MaxSignOuts, newestNext) {
		for _, i := range kept[t][share:] {
			dropped[all[i].hash] = refusedBudget
		}
	}
	return dropped
}

// capSignOutsLocked applies the bounds of the sessions document to what
// a merge brought in, and returns how many entries they dropped (not
// counting the earlier generations of a slot: a replacement reaching
// this replica).
func (s *Store) capSignOutsLocked() int {
	n := 0
	for h, reason := range planSignOutCaps(s.signedOut, s.latestRecordedLocked()) {
		delete(s.signedOut, h)
		if reason != superseded {
			n++
		}
	}
	return n
}

// gaugeSignOutsLocked publishes how full each bound of the sessions
// document is.
func (s *Store) gaugeSignOutsLocked() {
	held, most := map[string]int{}, 0
	for _, e := range s.signedOut {
		held[e.Subject]++
		if e.Subject != "" {
			most = max(most, held[e.Subject])
		}
	}
	signOutsHeld.WithLabelValues(refusedSubject).Set(float64(most))
	signOutsHeld.WithLabelValues(refusedUnnamed).Set(float64(held[""]))
	signOutsHeld.WithLabelValues("total").Set(float64(len(s.signedOut)))
}

// SignedOut reports whether a replica signed the session out.
func (s *Store) SignedOut(sid string) bool {
	if s == nil || sid == "" {
		return false
	}
	h := sessionHash(sid)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.signedOut[h]
	return ok && s.opts.Now().Unix() < e.Expiry
}

// ------------------------------------------------------------------- since

func factKey(kind, uid string, rest ...string) string {
	return strings.Join(append([]string{kind, uid}, rest...), "/")
}

// objectOf is the UID segment of a fact key.
func objectOf(key string) string {
	parts := strings.SplitN(key, "/", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

func (s *Store) setFactLocked(key string, f fact) {
	s.facts[key] = f
	uid := objectOf(key)
	if s.byObject[uid] == nil {
		s.byObject[uid] = map[string]struct{}{}
	}
	s.byObject[uid][key] = struct{}{}
}

func (s *Store) deleteFactLocked(key string) {
	delete(s.facts, key)
	uid := objectOf(key)
	if keys := s.byObject[uid]; keys != nil {
		delete(keys, key)
		if len(keys) == 0 {
			delete(s.byObject, uid)
		}
	}
}

// sinceLocked merges one local observation: the state has held since
// local on this replica. It returns the time to show.
func (s *Store) sinceLocked(key, state string, local time.Time) time.Time {
	h, at := hashState(state), local.UnixNano()
	f, ok := s.facts[key]
	switch {
	case !ok, f.State != h && at > f.Since:
		// First seen anywhere, or a transition newer than the shared one.
		s.setFactLocked(key, fact{State: h, Since: at})
		s.changedLocked(false)
		return local
	case f.State != h:
		// Another replica saw a later transition this one has not: each
		// keeps its own clock until they agree again.
		return local
	case f.Since < at:
		return time.Unix(0, f.Since).UTC()
	case f.Since > at:
		s.setFactLocked(key, fact{State: h, Since: at})
		s.changedLocked(false)
	}
	return local
}

// InstanceSince returns when a pod was first seen in a status-read
// state, on any replica or before a restart (sweep.SharedClocks).
func (s *Store) InstanceSince(podUID, state string, local time.Time) time.Time {
	if s == nil || podUID == "" {
		return local
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sinceLocked(factKey(kindInstance, podUID), state, local)
}

// PhaseSince returns when a Cluster was first seen in its phase.
func (s *Store) PhaseSince(clusterUID, phase string, local time.Time) time.Time {
	if s == nil || clusterUID == "" {
		return local
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sinceLocked(factKey(kindPhase, clusterUID), phase, local)
}

// FindingsSince merges one diagnostics run of a Cluster: present maps
// each finding the run reported to when this replica first saw it, and
// the result maps it to the earliest time any replica did. A finding the
// shared state holds and this run did not report is recorded as cleared
// at at, so it cannot lend its old "since" to a later recurrence.
func (s *Store) FindingsSince(clusterUID string, present map[string]time.Time, at time.Time) map[string]time.Time {
	if s == nil || clusterUID == "" {
		return present
	}
	out := make(map[string]time.Time, len(present))
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := factKey(kindFinding, clusterUID) + "/"
	for id, local := range present {
		out[id] = s.sinceLocked(prefix+id, findingOn, local)
	}
	on := hashState(findingOn)
	for key := range s.byObject[clusterUID] {
		id, isFinding := strings.CutPrefix(key, prefix)
		if !isFinding {
			continue
		}
		if _, still := present[id]; !still && s.facts[key].State == on && at.UnixNano() > s.facts[key].Since {
			s.setFactLocked(key, fact{State: hashState(findingOff), Since: at.UnixNano()})
			s.changedLocked(false)
		}
	}
	return out
}

// ---------------------------------------------------------------- digests

// sentKey is a delivery mark's key, or "" when id or channel is not one
// this console could have written.
func sentKey(id, channel string) string {
	if !validDigestID(id) || !validChannel(channel) {
		return ""
	}
	return id + "/" + channel
}

// validDigestID accepts what a digest ID is made of (digest.IDLayout:
// digits and the letters T and Z), within a bound.
func validDigestID(id string) bool {
	if id == "" || len(id) > maxDigestID {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// validChannel accepts a channel name: lowercase letters, within a bound.
func validChannel(channel string) bool {
	if channel == "" || len(channel) > maxChannel {
		return false
	}
	for _, r := range channel {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// MarkDigestSent shares that channel delivered the digest id at at. A
// mark already held keeps its earlier time: once sent stays sent.
func (s *Store) MarkDigestSent(id, channel string, at time.Time) {
	key := sentKey(id, channel)
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.sent[key]; ok && prev <= at.Unix() {
		return
	}
	s.sent[key] = at.Unix()
	// Urgent: a leader that stops right after a delivery must leave its
	// mark for the next one.
	s.changedLocked(true)
}

// DigestSent reports whether any replica marked the digest id delivered
// to channel, and when.
func (s *Store) DigestSent(id, channel string) (time.Time, bool) {
	key := sentKey(id, channel)
	if s == nil || key == "" {
		return time.Time{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.sent[key]
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(at, 0).UTC(), true
}

// MarksKnown reports whether the delivery marks reflect the ConfigMap:
// it was read once, or it cannot be (none configured, missing,
// forbidden) and the local marks are all there is. While the first read
// is pending or failing it is false, so that a new leader does not take
// "no mark yet" for "not sent".
func (s *Store) MarksKnown() bool {
	if !s.Enabled() {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.base != nil || s.mode == modeMissing || s.mode == modeForbidden
}

// ------------------------------------------------------------ merge/write

// changedLocked records a local change and wakes the writer.
func (s *Store) changedLocked(urgent bool) {
	s.gen++
	s.urgent = s.urgent || urgent
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// validFact reports whether a remote entry is one this console could
// have written.
func validFact(key string, f fact) bool {
	parts := strings.SplitN(key, "/", 3)
	if len(key) > maxKey || len(parts) < 2 || parts[1] == "" || f.Since <= 0 || len(f.State) != 16 {
		return false
	}
	switch parts[0] {
	case kindInstance, kindPhase:
		return len(parts) == 2
	case kindFinding:
		return len(parts) == 3 && parts[2] != ""
	}
	return false
}

func validSessionHash(h string) bool {
	if len(h) != 32 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

// validSent reports whether a remote delivery mark is one this console
// could have written.
func validSent(key string, at int64) bool {
	id, channel, ok := strings.Cut(key, "/")
	return ok && at > 0 && sentKey(id, channel) == key
}

// documentVersion reads a document's format version, before the rest, so
// that a newer layout is recognised even when it no longer decodes as
// this one; ok is false when the document does not parse at all.
func documentVersion(raw string) (version int, ok bool) {
	var head struct {
		Version int `json:"version"`
	}
	if json.Unmarshal([]byte(raw), &head) != nil {
		return 0, false
	}
	return head.Version, true
}

// readableVersion reports whether this console reads a document of
// version v, as a document of formatVersion: what each older version
// lacks reads as empty.
//
//   - 1: the same layout without the codes' marks (none), whose
//     sign-outs were ordered by the old fair-share tie-break; read as 2,
//     they are ordered by this one, as if recorded by a replica of 2.
func readableVersion(v int) bool {
	return v >= minFormatVersion && v <= formatVersion
}

// foreignLocked reports whether the document raw of key is foreign (of a
// version this console does not read) and, if so, marks it so, to be
// neither merged nor rewritten, and logs it once.
func (s *Store) foreignLocked(key, raw string) bool {
	v, ok := documentVersion(raw)
	if !ok || readableVersion(v) || s.takingOverLocked(key, raw) {
		return false
	}
	s.foreign[key] = true
	if logged, ok := s.foreignLogged[key]; !ok || logged != v {
		s.foreignLogged[key] = v
		until := "until it is taken over, once no console of another version has written the ConfigMap for " +
			takeOverAfter.String() + " (or at once by deleting the key: what it holds is then lost)"
		if !takeable(raw) {
			until = "until its key is deleted (what it holds is then lost): this version never takes over a document of " +
				"version " + strconv.Itoa(takeOverBelow) + " or more"
		}
		s.opts.Logger.Warn("shared state: a document of a format version this console does not read is left as it is "+
			"(another console wrote it, during a rolling upgrade or before a rollback); what it holds is not shared with "+
			"this replica "+until, slog.String("configmap", s.opts.Name),
			slog.String("document", key), slog.Int("version", v), slog.Int("reads", formatVersion))
	}
	return true
}

// olderVersion reports whether resourceVersion rv is older than than,
// two versions of one object. Kubernetes asks clients to treat them as
// opaque; every API server stores them as the integer revision of its
// store, and the two are compared only when both parse so: otherwise
// neither is called older and the ConfigMap is merged.
func olderVersion(rv, than string) bool {
	a, errA := strconv.ParseUint(rv, 10, 64)
	b, errB := strconv.ParseUint(than, 10, 64)
	return errA == nil && errB == nil && a < b
}

// versionedLocked is data without the documents that carry no format
// version (`{}`, or a version of 0 or less, which no console writes): such a
// document is not foreign (it would be left alone for good, as no version
// ever takes it over) but unreadable, so it is merged as missing and
// rewritten at this version on the next write; for grants.json a missing
// document resets the index (mergeGrantsLocked), as deleting the key
// does. Each is logged once.
func (s *Store) versionedLocked(data map[string]string) map[string]string {
	out, cloned := data, false
	for _, key := range documentKeys {
		raw := data[key]
		if v, parsed := documentVersion(raw); raw == "" || !parsed || v > 0 {
			delete(s.unversionedLogged, key)
			continue
		}
		if !cloned {
			out, cloned = maps.Clone(data), true
		}
		delete(out, key)
		if !s.unversionedLogged[key] {
			s.unversionedLogged[key] = true
			s.opts.Logger.Warn("shared state: a document carries no format version (no console writes one so); it is read as empty "+
				"and rewritten on the next write", slog.String("configmap", s.opts.Name), slog.String("document", key))
		}
	}
	return out
}

// gaugeForeignLocked publishes which documents are left untouched, and
// forgets the logging of those no longer foreign.
func (s *Store) gaugeForeignLocked() {
	for _, key := range documentKeys {
		v := 0.0
		if s.foreign[key] {
			v = 1
		} else {
			delete(s.foreignLogged, key)
		}
		sharedForeign.WithLabelValues(key).Set(v)
	}
}

// mergeLocked folds a ConfigMap read from the API server into the local
// state, then prunes, and reports whether the merged state differs from
// what the ConfigMap holds (a write is then due).
func (s *Store) mergeLocked(cm *corev1.ConfigMap) {
	if s.olderLocked(cm) {
		// Older than the ConfigMap last merged or written (a watch event
		// still queued while a conflict's re-read or this replica's own
		// write showed a newer one): everything it holds is in that newer
		// one, which is a merge of it, and what it lacks was added since.
		// Merged, it would set an older base and, under the three-way merge
		// of grants, drop a grant added since (mergeGrantsLocked).
		return
	}
	s.base, s.seenRV = cm.DeepCopy(), cm.ResourceVersion
	if s.uid != "" && cm.UID != "" && cm.UID != s.uid {
		s.recreatedLocked()
	}
	if cm.UID != "" {
		s.uid = cm.UID
	}
	s.observeWritersLocked(cm)
	data := s.versionedLocked(cm.Data)
	s.foreign = map[string]bool{}
	if raw, ok := data[KeySessions]; ok && raw != "" {
		var doc sessionsDoc
		switch {
		case s.foreignLocked(KeySessions, raw):
		case json.Unmarshal([]byte(raw), &doc) != nil:
			s.opts.Logger.Warn("shared state: the sessions document does not parse; it will be rewritten", slog.String("configmap", s.opts.Name))
		case len(doc.SignedOut) <= maxEntries:
			for h, e := range doc.SignedOut {
				if !validSessionHash(h) || !validSignOut(h, e) {
					continue
				}
				// A recording time in this replica's future is kept as
				// read: the bounds alone read it as none
				// (signOutBefore).
				if prev, ok := s.signedOut[h]; ok {
					e = mergeSignOut(h, prev, e)
				}
				s.signedOut[h] = e
			}
		}
	}
	if raw, ok := data[KeySince]; ok && raw != "" {
		var doc sinceDoc
		switch {
		case s.foreignLocked(KeySince, raw):
		case json.Unmarshal([]byte(raw), &doc) != nil:
			s.opts.Logger.Warn("shared state: the since document does not parse; it will be rewritten", slog.String("configmap", s.opts.Name))
		case len(doc.Facts) <= maxEntries:
			for key, f := range doc.Facts {
				if !validFact(key, f) {
					continue
				}
				local, ok := s.facts[key]
				switch {
				case !ok, f.State != local.State && f.Since > local.Since:
					s.setFactLocked(key, f)
				case f.State == local.State && f.Since < local.Since:
					s.setFactLocked(key, f)
				}
			}
		}
	}
	if raw, ok := data[KeyDigests]; ok && raw != "" {
		var doc digestsDoc
		switch {
		case s.foreignLocked(KeyDigests, raw):
		case json.Unmarshal([]byte(raw), &doc) != nil:
			s.opts.Logger.Warn("shared state: the digests document does not parse; it will be rewritten", slog.String("configmap", s.opts.Name))
		case len(doc.Sent) <= maxEntries:
			for key, at := range doc.Sent {
				// Union; the earliest delivery time of a mark wins.
				if prev, ok := s.sent[key]; validSent(key, at) && (!ok || at < prev) {
					s.sent[key] = at
				}
			}
		}
	}
	wasUnreadable := s.grantsUnreadable
	s.grantsUnreadable = false
	// A missing or empty grants document is merged as an empty one: the
	// key deleted resets the index (mergeGrantsLocked).
	s.mergeGrantsLocked(data[KeyGrants])
	switch {
	case s.grantsUnreadable && !wasUnreadable:
		s.opts.Logger.Error("shared state: the grants document does not parse; MCP grants are refused until it is fixed "+
			"(delete the key "+KeyGrants+" to reset it: every assistant then connects again)",
			slog.String("configmap", s.opts.Name))
	case wasUnreadable && !s.grantsUnreadable:
		s.opts.Logger.Info("shared state: the grants document is readable again", slog.String("configmap", s.opts.Name))
	}
	if raw, ok := data[KeyDrills]; ok && raw != "" {
		s.mergeDrillsLocked(raw)
	}
	s.gaugeForeignLocked()
	if !s.inSyncWithLocked(s.pruneLocked()) || s.beatDueLocked() {
		s.changedLocked(false)
	}
}

// olderLocked reports whether cm is older than the base, or, the
// ConfigMap deleted since (no base), older than the last base of the same
// object (a read answered before the delete, arriving after it): such a
// ConfigMap is not merged (mergeLocked).
func (s *Store) olderLocked(cm *corev1.ConfigMap) bool {
	if s.base != nil {
		return olderVersion(cm.ResourceVersion, s.base.ResourceVersion)
	}
	return cm.UID != "" && cm.UID == s.uid && olderVersion(cm.ResourceVersion, s.seenRV)
}

// mergeReadLocked is mergeLocked of cm as a Get answered it (a quorum
// read), not as a watch delivered it.
//
// mergeLocked skips a ConfigMap older than the base: a watch event still
// queued, or a Get whose answer was overtaken by the watch delivering a
// newer write. But when the API server's revision went back (an etcd
// restore without --bump-revision), every read is older than the base:
// each would be skipped, each write would conflict until maxAttempts, and
// the grants would stay usable without the other replicas' revocations.
// So rewoundAfter Gets in a row older than the base make the older one
// authoritative: the base is replaced by it (logged once an episode). A
// Get overtaken by the watch twice in a row is merged so too, which costs
// one more conflict: the next write, built on it, conflicts, and the
// re-read after it shows the newer ConfigMap again.
func (s *Store) mergeReadLocked(cm *corev1.ConfigMap) {
	if s.olderLocked(cm) {
		s.rewound++
		if s.rewound < rewoundAfter {
			return
		}
		if !s.rewoundLogged {
			s.rewoundLogged = true
			s.opts.Logger.Warn("shared state: the API server shows the ConfigMap at a resourceVersion older than the one this "+
				"replica merged (an etcd restore that did not bump the revision?); that version is taken as the ConfigMap",
				slog.String("configmap", s.opts.Name), slog.String("resourceVersion", cm.ResourceVersion),
				slog.String("merged", s.seenRV))
		}
		s.base, s.seenRV = nil, ""
	} else {
		s.rewoundLogged = false
	}
	s.rewound = 0
	s.mergeLocked(cm)
}

// wroteBaseLocked records updated, this replica's write as the API server
// answered it, as the base, unless the base is newer already (the watch
// delivered another replica's later write before this answer was read):
// set back, the base would drop under the three-way merge the grants that
// later write added (mergeGrantsLocked).
func (s *Store) wroteBaseLocked(updated *corev1.ConfigMap) {
	if s.base == nil || !olderVersion(updated.ResourceVersion, s.base.ResourceVersion) {
		s.base, s.seenRV = updated.DeepCopy(), updated.ResourceVersion
	}
	s.rewound = 0
	s.wroteLocked(updated)
}

// exhaustedLocked records a write that still conflicted after maxAttempts
// as a failure: the replica cannot write its state (a ConfigMap rewritten
// faster than it can merge, or one whose revision went back), so the
// failing mode's rules apply (grants usable for staleGrace only).
func (s *Store) exhaustedLocked() {
	sharedExhausted.Inc()
	s.setModeLocked(modeFailing, "every write conflicted")
}

// pruneLocked drops expired sign-outs, cleared findings past offKeep,
// the facts of objects gone from the cache (when the cache's list is
// complete), and, over the budget, the oldest facts. It returns the
// documents as encoded once pruned (encodeLocked), for the caller to
// compare and write while it holds the lock: the documents are encoded
// once a prune, the largest part of the time the lock is held.
func (s *Store) pruneLocked() map[string]string {
	now := s.opts.Now()
	s.pruneSignOutsLocked()
	if n := s.capSignOutsLocked(); n > 0 {
		// Only a merge brings in more than the bounds hold (two replicas
		// filling the last places at once, or replacing the same entry
		// of one person at once): sign-outs each of them had accepted
		// are dropped from the document, though still refused by the
		// replica that recorded them. Every replica merging counts them.
		signOutsRefused.WithLabelValues(refusedMerge).Add(float64(n))
		if !s.mergeDropLogged {
			s.mergeDropLogged = true
			s.opts.Logger.Warn("shared state: merging the sessions document dropped sign-outs past its bounds; "+
				"each is still refused by the replica that recorded it (counted under reason merge, logged once)",
				slog.String("configmap", s.opts.Name), slog.Int("dropped", n))
		}
	}
	s.gaugeSignOutsLocked()
	if s.signOutsFull && len(s.signedOut) < MaxSignOuts {
		s.signOutsFull = false
		s.opts.Logger.Info("shared state: the sessions document has room for sign-outs again", slog.String("configmap", s.opts.Name))
	}
	s.pruneGrantsLocked(now.Unix())
	keepSent := now.Add(-s.opts.DigestRetention - sentSlack).Unix()
	for key, at := range s.sent {
		if at < keepSent {
			delete(s.sent, key)
		}
	}
	off := hashState(findingOff)
	for key, f := range s.facts {
		if f.State == off && now.Sub(time.Unix(0, f.Since)) > offKeep {
			s.deleteFactLocked(key)
		}
	}
	if s.live != nil {
		for uid, keys := range s.byObject {
			if s.live[uid] {
				continue
			}
			for key := range keys {
				s.deleteFactLocked(key)
			}
		}
	}
	return s.budgetLocked(s.encodeLocked())
}

// budgetLocked drops the facts with the oldest "since" until they fit,
// then, should the ConfigMap's data still pass MaxData, the oldest drill
// results (ceilingLocked); docs are the documents as encoded, and it
// returns them as they are once fitted. The "since" times give way to
// every other document: the since document may take what the budget
// leaves the sessions and digests documents (Options.Budget bounds the
// three together), and no more than what MaxData leaves once every other
// document is counted, so that they are dropped only when the ConfigMap
// has no room left for them. Sign-outs are not dropped here (forgetting
// one would let a copied cookie back in): their own bounds
// (capSignOutsLocked) keep the sessions document within MaxSignOuts; nor
// are delivery marks, which the retention bounds (a few a day) and whose
// loss would send a digest twice; nor grants and redeemed codes, bounded
// by count (grants.go). At every bound those take some 800 KB, under
// MaxData.
func (s *Store) budgetLocked(docs map[string]string) map[string]string {
	_, data := s.measureLocked(docs)
	over := s.sinceOverLocked(docs, data)
	if over <= 0 {
		if s.overBudget {
			s.overBudget = false
			s.opts.Logger.Info("shared state fits its budget again", slog.String("configmap", s.opts.Name))
		}
		return s.ceilingLocked(docs, data)
	}
	keys := make([]string, 0, len(s.facts))
	for key := range s.facts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := s.facts[keys[i]], s.facts[keys[j]]
		if a.Since != b.Since {
			return a.Since < b.Since
		}
		return keys[i] < keys[j]
	})
	dropped := 0
	for over > 0 && dropped < len(keys) {
		// Drop by the encoded size of each entry, then measure again:
		// "key":{"s":"<state>","t":<since>}, and a comma.
		for ; over > 0 && dropped < len(keys); dropped++ {
			key := keys[dropped]
			over -= len(key) + len(s.facts[key].State) + len(strconv.FormatInt(s.facts[key].Since, 10)) + 18
			s.deleteFactLocked(key)
		}
		docs[KeySince] = s.encodeSinceLocked()
		_, data = s.measureLocked(docs)
		over = s.sinceOverLocked(docs, data)
	}
	sharedDropped.WithLabelValues(KeySince).Add(float64(dropped))
	if !s.overBudget {
		s.overBudget = true
		s.opts.Logger.Warn("shared state over its size budget: the oldest since times were dropped",
			slog.String("configmap", s.opts.Name), slog.Int("dropped", dropped), slog.Int("budget", s.opts.Budget),
			slog.Int("signedOut", len(s.signedOut)))
	}
	return s.ceilingLocked(docs, data)
}

// sinceOverLocked is by how many bytes the since document passes what it
// may take (budgetLocked), data being the ConfigMap's data with docs.
func (s *Store) sinceOverLocked(docs map[string]string, data int) int {
	since := len(docs[KeySince])
	overBudget := len(docs[KeySessions]) + len(docs[KeyDigests]) + since - s.opts.Budget
	overData := data - MaxData
	if s.foreign[KeySince] {
		// Not written: dropping facts frees nothing of the ConfigMap.
		overData = 0
	}
	return max(overBudget, overData)
}

// ceilingLocked keeps the ConfigMap's data within MaxData, every
// document counted, another version's included: past it the API server
// refuses every write. What the budget leaves past it are drill results
// (a document at every field's bound) or another version's documents
// during a rolling upgrade; the oldest drill results go, each source's
// newest pass last (counted in pgtoolbox_console_shared_state_dropped_total
// and logged once an episode). With none left to drop, it logs that the
// write will be refused. docs are the documents as encoded, data the
// ConfigMap's data with them; it returns the documents once fitted.
func (s *Store) ceilingLocked(docs map[string]string, data int) map[string]string {
	over := data - MaxData
	if over <= 0 {
		if s.overCeiling {
			s.overCeiling = false
			s.opts.Logger.Info("shared state ConfigMap fits within its size again", slog.String("configmap", s.opts.Name))
		}
		return docs
	}
	dropped := 0
	for over > 0 && len(s.drills) > 0 {
		dropped += s.shedDrillsLocked(over)
		if drills, ok := s.encodeDrillsLocked(); ok {
			docs[KeyDrills] = drills
		} else {
			delete(docs, KeyDrills)
		}
		_, data = s.measureLocked(docs)
		over = data - MaxData
	}
	sharedDropped.WithLabelValues(KeyDrills).Add(float64(dropped))
	if s.overCeiling {
		return docs
	}
	s.overCeiling = true
	if over > 0 {
		s.opts.Logger.Error("shared state ConfigMap past what the API server accepts, with nothing left to drop: its writes will be refused",
			slog.String("configmap", s.opts.Name), slog.Int("bytes", over+MaxData), slog.Int("max", MaxData))
		return docs
	}
	s.opts.Logger.Warn("shared state ConfigMap at its size ceiling: the oldest restore drill results were dropped",
		slog.String("configmap", s.opts.Name), slog.Int("dropped", dropped), slog.Int("max", MaxData))
	return docs
}

// encodeLocked renders the documents, by data key; encoding/json sorts
// map keys, so two replicas holding the same state write the same bytes.
func (s *Store) encodeLocked() map[string]string {
	sessions, _ := json.Marshal(sessionsDoc{Version: formatVersion, SignedOut: s.signedOut})
	digests, _ := json.Marshal(digestsDoc{Version: formatVersion, Sent: s.sent})
	grants, _ := json.Marshal(grantsDoc{Version: formatVersion, Grants: s.grants, Codes: s.codes})
	out := map[string]string{KeySessions: string(sessions), KeySince: s.encodeSinceLocked(), KeyDigests: string(digests), KeyGrants: string(grants)}
	if drills, ok := s.encodeDrillsLocked(); ok {
		out[KeyDrills] = drills
	}
	return out
}

func (s *Store) encodeSinceLocked() string {
	since, _ := json.Marshal(sinceDoc{Version: formatVersion, Facts: s.facts})
	return string(since)
}

// encodeDrillsLocked renders the drills document. It is written only once
// there is something to keep (ok), so a console without restore drills
// writes what it did.
func (s *Store) encodeDrillsLocked() (string, bool) {
	if len(s.drills) == 0 && s.drillsSince == 0 {
		return "", false
	}
	drills, _ := json.Marshal(drillsDoc{Version: formatVersion, Since: s.drillsSince, Results: s.drills})
	return string(drills), true
}

// documentKeys are the ConfigMap keys this console writes.
var documentKeys = []string{KeySessions, KeySince, KeyDigests, KeyGrants, KeyDrills}

// sizesLocked measures the documents this console writes (ours) and the
// data the next write sends (data): the keys and values of those
// documents and of what else the ConfigMap holds (another version's
// documents, any other key, binaryData included: the API server counts
// the raw bytes of every binaryData value toward the same 1 MiB), as the
// API server counts them.
func (s *Store) sizesLocked() (ours, data int) {
	return s.measureLocked(s.encodeLocked())
}

// measureLocked is sizesLocked of the documents docs, as encodeLocked
// renders them.
func (s *Store) measureLocked(docs map[string]string) (ours, data int) {
	for key, doc := range docs {
		ours += len(doc)
		sharedBytes.WithLabelValues(key).Set(float64(len(doc)))
		if !s.foreign[key] {
			data += len(key) + len(doc)
		}
	}
	if s.base != nil {
		for key, v := range s.base.Data {
			if _, mine := docs[key]; !mine || s.foreign[key] {
				data += len(key) + len(v)
			}
		}
		for key, v := range s.base.BinaryData {
			data += len(key) + len(v)
		}
	}
	sharedBytes.WithLabelValues("total").Set(float64(data))
	return ours, data
}

// desiredLocked is the ConfigMap to write: the base with the documents
// this console understands replaced.
func (s *Store) desiredLocked() *corev1.ConfigMap {
	return s.desiredWithLocked(s.encodeLocked())
}

// desiredWithLocked is desiredLocked of the documents docs, as
// encodeLocked renders them.
func (s *Store) desiredWithLocked(docs map[string]string) *corev1.ConfigMap {
	cm := s.base.DeepCopy()
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	s.stampLocked(cm)
	for key, doc := range docs {
		if !s.foreign[key] {
			cm.Data[key] = doc
		}
	}
	return cm
}

// inSyncLocked reports whether the ConfigMap holds the local state.
func (s *Store) inSyncLocked() bool {
	return s.inSyncWithLocked(s.encodeLocked())
}

// inSyncWithLocked is inSyncLocked of the documents docs, as encodeLocked
// renders them: what desiredWithLocked would write of each document this
// console writes is what the ConfigMap holds (a document of another
// version, or one this console does not write now, is left as it is).
func (s *Store) inSyncWithLocked(docs map[string]string) bool {
	if s.base == nil {
		return true
	}
	for _, key := range documentKeys {
		doc, ok := docs[key]
		if !ok || s.foreign[key] {
			continue
		}
		if have, held := s.base.Data[key]; !held || doc != have {
			return false
		}
	}
	return true
}

// flush writes the local state when it differs from the ConfigMap,
// re-reading and merging on a conflict.
func (s *Store) flush(ctx context.Context) error {
	s.writes.Lock()
	defer s.writes.Unlock()
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	for range maxAttempts {
		s.mu.Lock()
		if s.base == nil || s.mode == modeMissing || s.mode == modeForbidden {
			s.mu.Unlock()
			return nil
		}
		docs := s.pruneLocked()
		if s.inSyncWithLocked(docs) && !s.beatDueLocked() {
			s.written, s.urgent = s.gen, false
			s.mu.Unlock()
			return nil
		}
		cm, gen := s.desiredWithLocked(docs), s.gen
		s.mu.Unlock()
		updated, err := s.opts.Client.Update(ctx, cm, metav1.UpdateOptions{FieldManager: "pgtoolbox-console"})
		switch {
		case err == nil:
			s.mu.Lock()
			s.wroteBaseLocked(updated)
			if s.gen == gen {
				s.written, s.urgent = gen, false
			}
			s.setModeLocked(modeLive, "")
			s.mu.Unlock()
			sharedWrites.WithLabelValues("written").Inc()
			return nil
		case apierrors.IsConflict(err):
			// Another replica wrote first: read its state, merge, retry.
			sharedWrites.WithLabelValues("conflict").Inc()
			fresh, getErr := s.opts.Client.Get(ctx, s.opts.Name, metav1.GetOptions{})
			if getErr != nil {
				return s.failed(getErr)
			}
			s.mu.Lock()
			s.mergeReadLocked(fresh)
			s.mu.Unlock()
		default:
			sharedWrites.WithLabelValues("failed").Inc()
			return s.failed(err)
		}
	}
	s.mu.Lock()
	s.exhaustedLocked()
	s.mu.Unlock()
	return fmt.Errorf("write of ConfigMap %s: still conflicting after %d attempts", s.opts.Name, maxAttempts)
}

// failed records a read or write failure as the store's mode.
func (s *Store) failed(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case apierrors.IsNotFound(err):
		s.setModeLocked(modeMissing, "")
	case apierrors.IsForbidden(err):
		s.setModeLocked(modeForbidden, "")
	default:
		s.setModeLocked(modeFailing, "the API server refused or did not answer")
	}
	return err
}

// setModeLocked changes the mode, logging the transitions an operator
// must see once: the degradations and the recovery.
func (s *Store) setModeLocked(m mode, reason string) {
	if s.mode == m && s.reason == reason {
		return
	}
	previous := s.mode
	s.mode, s.reason, s.since = m, reason, s.opts.Now().UTC()
	log := s.opts.Logger.With(slog.String("configmap", s.opts.Name))
	switch m {
	// P4-F: the two an install without the chart's RBAC meets at start;
	// each says what to create, and that MCP grants wait for it.
	case modeMissing:
		log.Warn("shared state ConfigMap not found: each replica keeps its own clocks and sign-outs, and MCP OAuth grants "+
			"are refused, until it exists; create it empty in the console's namespace (the chart does when console.rbac.create is true)",
			slog.String("rbac", "verbs get, watch, update on configmaps, resourceNames ["+s.opts.Name+"], for the console's ServiceAccount"))
	case modeForbidden:
		log.Warn("shared state ConfigMap not readable or writable by the console's ServiceAccount: each replica keeps its own "+
			"clocks and sign-outs, and MCP OAuth grants are refused; bind a Role to the ServiceAccount in the console's namespace "+
			"(the chart does when console.rbac.create is true)",
			slog.String("rbac", "verbs get, watch, update on configmaps, resourceNames ["+s.opts.Name+"]"))
	case modeFailing:
		log.Warn("shared state ConfigMap unreachable; retrying", slog.String("reason", reason))
	case modeLive:
		if previous != modePending {
			log.Info("shared state ConfigMap in use again")
		}
	case modeDisabled, modePending:
	}
	sharedMode.Reset()
	sharedMode.WithLabelValues(string(m)).Set(1)
}

// SetLive records the objects the cache holds, for pruning.
func (s *Store) setLive(live map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = live
	before := s.gen
	docs := s.pruneLocked()
	if s.gen == before && !s.inSyncWithLocked(docs) {
		s.changedLocked(false)
	}
}

// Run reads and watches the ConfigMap, merges what other replicas write,
// writes local changes in batches and prunes, until ctx ends; then it
// writes what is pending once more.
func (s *Store) Run(ctx context.Context) error {
	if !s.Enabled() {
		<-ctx.Done()
		return nil
	}
	var wg sync.WaitGroup
	wg.Go(func() { s.writer(ctx) })
	wg.Go(func() { s.pruner(ctx) })
	wg.Go(func() { s.ticker(ctx) })
	attempt := 0
	for ctx.Err() == nil {
		err := s.watchOnce(ctx)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			attempt++
			s.opts.Logger.Debug("shared state watch ended", slog.String("configmap", s.opts.Name), slog.Int("attempt", attempt))
		} else {
			attempt = 0
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.opts.Backoff(attempt)):
		}
	}
	wg.Wait()
	// A sign-out made just before the shutdown must still reach the
	// other replicas.
	final, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	_ = s.flush(final)
	return nil
}

func (s *Store) watchOnce(ctx context.Context) error {
	cm, err := s.opts.Client.Get(ctx, s.opts.Name, metav1.GetOptions{})
	if err != nil {
		return s.failed(err)
	}
	s.mu.Lock()
	s.mergeReadLocked(cm)
	s.setModeLocked(modeLive, "")
	s.mu.Unlock()
	// No resourceVersion: the watch starts at the current state and first
	// sends the ConfigMap as Added (merged again, harmlessly). Starting from
	// cm.ResourceVersion fails with "too old resource version" forever once
	// the ConfigMap has been left unchanged longer than the API server keeps
	// history.
	w, err := s.opts.Client.Watch(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", s.opts.Name).String(),
	})
	if err != nil {
		return s.failed(err)
	}
	defer w.Stop()
	first := true
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				return nil
			}
			switch ev.Type {
			case watch.Added, watch.Modified:
				if cm, ok := ev.Object.(*corev1.ConfigMap); ok && cm.Name == s.opts.Name {
					s.mu.Lock()
					if first && ev.Type == watch.Added {
						// The watch's first event is the ConfigMap as it is
						// now, a read as the Get above: two such reads older
						// than the base mean the revision went back
						// (mergeReadLocked), which an idle replica would
						// otherwise never take in.
						s.mergeReadLocked(cm)
					} else {
						s.mergeLocked(cm)
					}
					s.mu.Unlock()
				}
				first = false
			case watch.Deleted:
				s.mu.Lock()
				s.base, s.rewound = nil, 0
				s.setModeLocked(modeMissing, "")
				s.mu.Unlock()
				return nil
			case watch.Error:
				return fmt.Errorf("watch error: %w", apierrors.FromObject(ev.Object))
			case watch.Bookmark:
			}
		}
	}
}

// writer batches local changes into writes.
func (s *Store) writer(ctx context.Context) {
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		if !s.debounce(ctx) {
			return
		}
		if err := s.flush(ctx); err != nil && ctx.Err() == nil {
			attempt++
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.opts.Backoff(attempt)):
			}
			s.mu.Lock()
			if s.gen != s.written {
				s.changedLocked(false)
			}
			s.mu.Unlock()
			continue
		}
		attempt = 0
	}
}

// debounce waits before a write: Debounce for routine changes,
// UrgentDebounce once an urgent one (a sign-out, a revoked grant, a spent
// code, a delivery mark) is pending, including one that arrives while the
// routine wait runs: a grant revoked just after a "since" time changed is
// written within about a second, not with the ten-second batch. It
// reports false when ctx ends.
func (s *Store) debounce(ctx context.Context) bool {
	start := time.Now()
	urgent := false
	timer := time.NewTimer(s.opts.Debounce)
	defer timer.Stop()
	for {
		s.mu.Lock()
		nowUrgent := s.urgent
		s.mu.Unlock()
		if nowUrgent && !urgent {
			urgent = true
			// The urgent deadline runs from the start of the batch: a
			// batch that has waited that long already is written now.
			timer.Reset(max(s.opts.UrgentDebounce-time.Since(start), 0))
		}
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case <-s.wake:
			// Another change while waiting: it joins this batch; only its
			// urgency matters.
		}
	}
}

// pruner refreshes the live object set once a minute.
func (s *Store) pruner(ctx context.Context) {
	if s.opts.Live == nil {
		return
	}
	t := time.NewTicker(pruneEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if live, ok := s.opts.Live(ctx); ok {
				s.setLive(live)
			}
		}
	}
}

// SourceName is the admin page's key of the shared state.
const SourceName = "sharedState"

// State is the shared state as a source of the admin page.
func (s *Store) State() viewmodel.SourceState {
	if !s.Enabled() {
		return viewmodel.SourceState{
			Source: SourceName, State: viewmodel.SourceDisabled,
			Reason: "no state ConfigMap is configured (STATE_CONFIGMAP): each replica keeps its own clocks and sign-outs",
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	since := s.since
	st := viewmodel.SourceState{Source: SourceName, Since: &since}
	switch s.mode {
	case modeLive:
		st.State = viewmodel.SourceLive
	case modeMissing:
		st.State, st.Reason = viewmodel.SourceUnavailable,
			"ConfigMap "+s.opts.Name+" not found: each replica keeps its own clocks and sign-outs"
	case modeForbidden:
		st.State, st.Reason = viewmodel.SourceForbidden,
			"the console may not read or write ConfigMap "+s.opts.Name+": each replica keeps its own clocks and sign-outs"
	case modeFailing:
		st.State, st.Reason = viewmodel.SourceUnavailable, "ConfigMap "+s.opts.Name+": "+s.reason
	default:
		st.State = viewmodel.SourcePending
	}
	return st
}
