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
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KeyGrants holds the MCP clients' console grants and the
// single-use values already spent (authorization codes, consent answers),
// in grants.json:
//
//	{"version":2,
//	 "grants":{"grant-<32 hex>":{"u":<user key>,"c":<client name, as the
//	   client declared it>,"s":<scope>,"t":<created>,"l":<last used>,
//	   "e":<expiry>,"g":<refresh generation>,"r":<revoked at, 0 when not>}},
//	 "codes":{<32-hex SHA-256 prefix of the value>:{"e":<expiry>,"u":<user key>,
//	   "m":[<marks: the later steps of the same work, MarkCode>]}}}
//
// Times are Unix seconds. The user key is a 32-hex keyed hash of the
// username (the console's secret is needed to tell whose it is). No token,
// no username: a grant ID is a reference (it names the grant in the audit
// log and the revocation list) and opens nothing; a grant's tokens are
// sealed and held by its client.
//
// The index is authoritative: a grant it does not know is refused once
// the replication grace of its newest token has passed (mcpoauth), so an
// entry may be dropped once nothing could be refused by it any more: at
// its expiry, or RevokedKeep after its revocation.
//
// Merge rule, the same on every replica: for one ID the earliest
// creation, the latest use, the highest generation and the earliest
// revocation win (a revocation is never undone); a grant the document no
// longer holds is dropped, unless this replica recorded it moments ago or
// holds a revocation of it not written yet (mergeGrantsLocked says why).
// Codes are a union, latest expiry, and the union of their marks. Every valid entry another replica wrote is merged,
// however many there are: none is dropped for being one too many. A
// document this replica cannot read (another format version, or not
// JSON) is never overwritten, and grants are refused while it is so.
const KeyGrants = "grants.json"

// Bounds of the grant index: a person may connect a few assistants on a
// few machines; the console refuses new work (a grant, a consent answer,
// a code) beyond them rather than growing the ConfigMap. Every entry
// counts, revoked ones and revocation placeholders included. The totals
// are sized for a hundred people each at their own bound, but no table
// fills that way: once a table holds fairFromPercent of its total, new
// work is refused to a person holding lateBound entries or more, or at
// least the mean share of the people the table holds (overFairShare); a
// newcomer is admitted while the table has room. Whatever the order of
// arrival, N identities hold at most 80 % of the total plus lateBound for
// each identity past the 80 % that took their own bound to get there:
// 1,280 + 4 x (N - 80) of 1,600, plus in the grants table one revocation
// placeholder each (below): a hundred identities hold at most 1,360 codes
// and 1,460 grants, filling the codes table takes 160 identities and the
// grants table 128, so the last places go to newcomers.
// Each replica decides from what it holds and the merge stays a union, so
// replicas converge as before (concurrent admissions on several replicas,
// within the second a write takes to arrive, can each take a place).
// Identities minted without bound can still fill a table four entries
// each: that is what the per-person bounds cannot tell apart. Two writes
// that are not new work are exempt from the fair share (only the hard
// bounds apply): re-recording a grant another replica admitted (a refresh
// of a grant this replica has not seen rotate), and a person's first
// standing revocation placeholder (a replayed code's). Encoded, a grant
// entry is at most about 300 bytes (a client name costs at most 128 bytes
// once JSON-escaped, the scope 32) and a code about 90, so a full index
// takes about 605 KiB of the 768 KiB budget, which the other documents
// give way to (TestAFullIndexFitsTheBudget measures it).
const (
	MaxGrantsPerUser = 16
	MaxGrants        = 100 * MaxGrantsPerUser
	MaxCodesPerUser  = 16
	maxCodes         = 100 * MaxCodesPerUser
	// fairFromPercent is how full a table is, in percent of its total,
	// when the fair share starts to refuse the people holding the most.
	fairFromPercent = 80
	// lateDivisor sets lateBound, the per-person bound past
	// fairFromPercent: a quarter of the per-person bound.
	lateDivisor   = 4
	maxClientName = 128
	maxScope      = 32
	// RevokedKeep is how long a revoked entry outlives its revocation: far
	// longer than the replication grace during which a replica accepts a
	// grant it does not know yet (mcpoauth), so that no replica can accept
	// a revoked grant's token for want of the entry.
	RevokedKeep = 10 * time.Minute
	// localKeep is how long a grant this replica recorded is kept while
	// no document read from the ConfigMap holds it (mergeGrantsLocked):
	// RevokedKeep less a margin for clock skew between replicas (the
	// console assumes at most 30 s) and the write and watch delays.
	localKeep = RevokedKeep - 2*time.Minute
	// staleGrace is how long grants stay usable while the ConfigMap cannot
	// be written or read: a moment's API server trouble does not
	// disconnect every assistant, a lasting one refuses grants (a
	// revocation taken elsewhere might not be seen).
	staleGrace = 30 * time.Second
)

// Errors of the grant index.
var (
	// ErrTooManyGrants refuses a new grant over MaxGrantsPerUser or
	// MaxGrants.
	ErrTooManyGrants = errors.New("too many connected assistants: disconnect one first")
	// ErrSpent: the single-use value was used before.
	ErrSpent = errors.New("already used")
	// ErrBusy: the table of spent values is full (for this person or for
	// the console); nothing was recorded, retry later.
	ErrBusy = errors.New("too many sign-ins in progress; retry in a minute")
	// ErrNotSpent: MarkCode was asked to mark a value the person has not
	// spent (or that expired); nothing was recorded.
	ErrNotSpent = errors.New("not a spent value of this person")
)

// Grant is one grant as the index keeps it.
type Grant struct {
	ID string
	// UserHash is the user key (mcpoauth computes it: a keyed hash).
	UserHash string
	Client   string
	Scope    string
	Created  time.Time
	LastUsed time.Time
	Expiry   time.Time
	// Generation counts refresh-token rotations: only the refresh token of
	// the current generation is accepted.
	Generation int
	// Revoked is zero while the grant stands.
	Revoked time.Time
	// Rotated (AddGrant only, never stored) marks a grant another replica
	// admitted and rotated, which this one has not seen yet: the refresh
	// token is the proof it was admitted, the union already holds it, so
	// recording it here is not new work and only the hard bounds apply.
	Rotated bool
}

// grantEntry is a Grant on the wire.
type grantEntry struct {
	User    string `json:"u"`
	Client  string `json:"c"`
	Scope   string `json:"s"`
	Created int64  `json:"t"`
	Used    int64  `json:"l,omitempty"`
	Expiry  int64  `json:"e"`
	Gen     int    `json:"g,omitempty"`
	Revoked int64  `json:"r,omitempty"`
}

// codeEntry is a spent single-use value on the wire. Marks are the later
// steps of the same piece of work, recorded on the value rather than as
// entries of their own (MarkCode): a confirmation is one entry, however
// many steps it records, and so one unit of the fair share.
type codeEntry struct {
	Expiry int64    `json:"e"`
	User   string   `json:"u"`
	Marks  []string `json:"m,omitempty"`
}

// maxMarks bounds the marks of one value, maxMark the length of a mark.
const (
	maxMarks = 4
	maxMark  = 16
)

func validMark(m string) bool {
	if m == "" || len(m) > maxMark {
		return false
	}
	for _, r := range m {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// unionMarks is the sorted union of two entries' valid marks, at most
// maxMarks of them.
func unionMarks(a, b []string) []string {
	var out []string
	for _, m := range append(slices.Clone(a), b...) {
		if validMark(m) && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	slices.Sort(out)
	if len(out) > maxMarks {
		out = out[:maxMarks]
	}
	return out
}

type grantsDoc struct {
	Version int                   `json:"version"`
	Grants  map[string]grantEntry `json:"grants"`
	Codes   map[string]codeEntry  `json:"codes"`
}

func validGrantID(id string) bool {
	ref, ok := strings.CutPrefix(id, "grant-")
	return ok && isHex32(ref)
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

func validGrantEntry(id string, e grantEntry) bool {
	return validGrantID(id) && isHex32(e.User) && e.Created > 0 && e.Expiry > 0 && e.Gen >= 0 && e.Revoked >= 0 &&
		validClientName(e.Client) && len(e.Scope) <= maxScope
}

// nameCost is the size of r in a JSON string as encoding/json writes it:
// the characters it escapes cost six bytes (\u00XX). -1 for a character a
// client name must not hold (a control character, an invalid byte).
func nameCost(r rune) int {
	switch {
	case r == utf8.RuneError, unicode.IsControl(r):
		return -1
	case r == '<', r == '>', r == '&', r == '"', r == '\\', r == '\u2028', r == '\u2029':
		return 6
	}
	return utf8.RuneLen(r)
}

// validClientName: valid UTF-8 without control characters, at most
// maxClientName bytes once JSON-encoded.
func validClientName(name string) bool {
	if !utf8.ValidString(name) {
		return false
	}
	n := 0
	for _, r := range name {
		c := nameCost(r)
		if c < 0 {
			return false
		}
		n += c
	}
	return n <= maxClientName
}

func (e grantEntry) grant(id string) Grant {
	g := Grant{
		ID: id, UserHash: e.User, Client: e.Client, Scope: e.Scope, Generation: e.Gen,
		Created: time.Unix(e.Created, 0).UTC(), Expiry: time.Unix(e.Expiry, 0).UTC(),
	}
	if e.Used > 0 {
		g.LastUsed = time.Unix(e.Used, 0).UTC()
	}
	if e.Revoked > 0 {
		g.Revoked = time.Unix(e.Revoked, 0).UTC()
	}
	return g
}

// gone reports whether an entry can be dropped at now: expired, or
// revoked RevokedKeep ago.
func (e grantEntry) gone(now int64) bool {
	return e.Expiry <= now || (e.Revoked > 0 && e.Revoked+int64(RevokedKeep/time.Second) <= now)
}

// mergeGrant folds b into a by the merge rule.
func mergeGrant(a, b grantEntry) grantEntry {
	if b.Created < a.Created {
		a.Created = b.Created
	}
	a.Used = max(a.Used, b.Used)
	a.Expiry = max(a.Expiry, b.Expiry)
	a.Gen = max(a.Gen, b.Gen)
	if b.Revoked > 0 && (a.Revoked == 0 || b.Revoked < a.Revoked) {
		a.Revoked = b.Revoked
	}
	return a
}

// TruncateClientName makes a client's declared name valid for the index:
// valid UTF-8 without control characters, cut on a rune boundary to at
// most 128 bytes once JSON-encoded.
func TruncateClientName(name string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.ToValidUTF8(name, "") {
		c := nameCost(r)
		if c < 0 {
			continue
		}
		if n+c > maxClientName {
			break
		}
		n += c
		b.WriteRune(r)
	}
	return b.String()
}

// GrantsUsable reports whether the index can be trusted to refuse what was
// revoked: a store of this replica's memory always (it is the only
// authority), a shared one while it reads and writes the ConfigMap (or
// failed to for less than staleGrace) and can read its grants document.
// Live with no watch open (one being reopened) counts as failing from the
// instant the last watch ended (unwatchedAt): a read that hangs while it
// is reopened does not keep revocations made elsewhere unseen past
// staleGrace.
func (s *Store) GrantsUsable() bool {
	if s == nil {
		return false
	}
	if !s.Enabled() {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.foreign[KeyGrants] {
		return false
	}
	switch s.mode {
	case modeLive:
		return s.watching || !s.unwatchedAt.IsZero() && s.opts.Now().Sub(s.unwatchedAt) < staleGrace
	case modeFailing:
		return s.opts.Now().Sub(s.since) < staleGrace
	case modeDisabled, modePending, modeMissing, modeForbidden:
	}
	return false
}

// GrantsShared reports whether the index is shared with other replicas
// (a grant created elsewhere reaches this one about a second later).
func (s *Store) GrantsShared() bool { return s.Enabled() }

// countLocked counts the entries of user, in all, and the people holding
// any, whatever their state.
func (s *Store) countLocked(user string) (mine, total, people int) {
	seen := make(map[string]struct{}, len(s.grants))
	for _, x := range s.grants {
		if x.User == user {
			mine++
		}
		seen[x.User] = struct{}{}
	}
	return mine, len(s.grants), len(seen)
}

// placeholdersLocked counts the revocation placeholders user holds: the
// entries revoked and never used (a recorded grant is used at its
// creation, and the merge keeps the latest use).
func (s *Store) placeholdersLocked(user string) int {
	n := 0
	for _, x := range s.grants {
		if x.User == user && x.Revoked > 0 && x.Used == 0 {
			n++
		}
	}
	return n
}

// tableBound is the bounds of a table of the index: per person and in
// all.
type tableBound struct{ perUser, total int }

var (
	grantsBound = tableBound{perUser: MaxGrantsPerUser, total: MaxGrants}
	codesBound  = tableBound{perUser: MaxCodesPerUser, total: maxCodes}
)

// full reports whether a person holding mine of a table's total entries
// may add none whatever the fair share: at the per-person bound or at the
// table's bound.
func (b tableBound) full(mine, total int) bool {
	return mine >= b.perUser || total >= b.total
}

// lateBound is the per-person bound once the table holds fairFromPercent
// of its total.
func (b tableBound) lateBound() int { return max(1, b.perUser/lateDivisor) }

// overFairShare reports whether a person holding mine of a table's total
// entries, held by people people, may add none: when full, or, once the
// table holds fairFromPercent of its bound, holding lateBound or more, or
// at least the mean share (total/people). A newcomer may always add while
// the table has room. The lateBound refusal is what makes the bound
// independent of the order of arrival: the mean alone stops people
// arriving together, not people arriving one after another (each of the
// last ones holds less than the mean until their own bound).
func (b tableBound) overFairShare(mine, total, people int) bool {
	switch {
	case b.full(mine, total):
		return true
	case total*100 < b.total*fairFromPercent:
		return false
	}
	return mine >= b.lateBound() || (mine > 0 && mine*people >= total)
}

// AddGrant records a new grant, or merges one the index has. A new one is
// refused over the per-user or the total bound, or past the fair share of
// a nearly full index (overFairShare) (ErrTooManyGrants), counting every
// entry. A Rotated one (another replica admitted it) is refused only over
// the hard bounds.
func (s *Store) AddGrant(g Grant) error {
	if s == nil || !validGrantID(g.ID) {
		return errors.New("not a grant")
	}
	e := grantEntry{
		User: g.UserHash, Client: TruncateClientName(g.Client), Scope: g.Scope, Created: g.Created.Unix(),
		Used: g.LastUsed.Unix(), Expiry: g.Expiry.Unix(), Gen: g.Generation,
	}
	if g.LastUsed.IsZero() {
		e.Used = 0
	}
	if !validGrantEntry(g.ID, e) {
		return errors.New("not a grant")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.grants[g.ID]; ok {
		s.grants[g.ID] = mergeGrant(prev, e)
		s.changedLocked(true)
		return nil
	}
	s.pruneGrantsLocked(s.opts.Now().Unix())
	mine, total, people := s.countLocked(e.User)
	if grantsBound.full(mine, total) || (!g.Rotated && grantsBound.overFairShare(mine, total, people)) {
		return ErrTooManyGrants
	}
	s.grants[g.ID] = e
	s.recordedLocked(g.ID)
	s.changedLocked(true)
	return nil
}

// Grant returns one grant of the index.
func (s *Store) Grant(id string) (Grant, bool) {
	if s == nil {
		return Grant{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.grants[id]
	if !ok {
		return Grant{}, false
	}
	return e.grant(id), true
}

// GrantsOf lists the unexpired, unrevoked grants of a user, newest first.
func (s *Store) GrantsOf(userHash string) []Grant {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now().Unix()
	out := []Grant{}
	for id, e := range s.grants {
		if e.User == userHash && e.Revoked == 0 && e.Expiry > now {
			out = append(out, e.grant(id))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// RevokeGrant marks a grant revoked; every replica refuses it from the
// next write (about a second). It reports whether the grant was standing.
// A grant the index does not know (a code replayed before the grant it
// bought reached this replica) is recorded revoked under userHash until
// until. A placeholder is refused at the hard bounds; past the fair share
// it is admitted while the person holds no other standing placeholder (a
// revocation must be recorded, and a person cannot climb past the fair
// share by replaying their own codes: one placeholder each, at most, past
// it).
func (s *Store) RevokeGrant(id, userHash string, at, until time.Time) bool {
	if s == nil || !validGrantID(id) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.grants[id]
	if !ok {
		e = grantEntry{User: userHash, Created: at.Unix(), Expiry: until.Unix()}
		if !validGrantEntry(id, e) {
			return false
		}
		s.pruneGrantsLocked(s.opts.Now().Unix())
		mine, total, people := s.countLocked(userHash)
		if grantsBound.full(mine, total) || (grantsBound.overFairShare(mine, total, people) && s.placeholdersLocked(userHash) > 0) {
			return false
		}
	}
	if e.Revoked > 0 {
		return false
	}
	e.Revoked = at.Unix()
	s.grants[id] = e
	if !ok {
		s.recordedLocked(id)
	}
	s.revokedLocked(id)
	s.changedLocked(true)
	return ok
}

// RevokeGrantsOf revokes every standing grant of a user and returns their
// IDs ("sign out and disconnect my assistants").
func (s *Store) RevokeGrantsOf(userHash string, at time.Time) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now().Unix()
	var ids []string
	for id, e := range s.grants {
		if e.User == userHash && e.Revoked == 0 && e.Expiry > now {
			e.Revoked = at.Unix()
			s.grants[id] = e
			s.revokedLocked(id)
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		s.changedLocked(true)
	}
	sort.Strings(ids)
	return ids
}

// WriteRevocations writes the revocations of ids, made on this replica
// (RevokeGrant, RevokeGrantsOf), through to the ConfigMap, as SpendCode
// writes a spend: nil once a document the API server holds records each
// of them revoked (this write, or another replica's that holds them
// already), ErrNotRecorded when that cannot be done within writeTimeout.
// The local revocations stay either way: they only refuse more, and the
// next batch writes them. Without a ConfigMap (one replica) there is
// nothing to write.
//
// No ids is nil only on a replica that has read the grants document, or
// knows the ConfigMap missing or forbidden (every grant is then refused,
// on every replica: they share the ServiceAccount): one that has not read
// it yet knows none of the grants recorded elsewhere, and its "nothing to
// revoke" is no answer (ErrNotRecorded), not a disconnection done.
func (s *Store) WriteRevocations(ctx context.Context, ids ...string) error {
	if s == nil {
		return ErrNotRecorded
	}
	if !s.Enabled() {
		return nil
	}
	if len(ids) == 0 {
		var read bool
		s.locked(func() {
			read = s.mode == modeMissing || s.mode == modeForbidden || s.base != nil && !s.foreign[KeyGrants]
		})
		if !read {
			return ErrNotRecorded
		}
		return nil
	}
	revokedIn := func(cm *corev1.ConfigMap) bool {
		var doc grantsDoc
		if cm == nil || json.Unmarshal([]byte(cm.Data[KeyGrants]), &doc) != nil {
			return false
		}
		for _, id := range ids {
			if doc.Grants[id].Revoked == 0 {
				return false
			}
		}
		return true
	}
	if s.lockWrites(ctx) != nil {
		return ErrNotRecorded
	}
	defer s.unlockWrites()
	var desired *corev1.ConfigMap
	var gen uint64
	var early error
	done := false
	s.locked(func() {
		switch {
		case s.mode == modeMissing:
			done, early = true, ErrNoConfigMap
		case s.base == nil || s.mode == modeForbidden || s.foreign[KeyGrants]:
			done, early = true, ErrNotRecorded
		case revokedIn(s.base):
			done = true
		default:
			desired, gen = s.desiredLocked(), s.gen
		}
	})
	if done {
		return early
	}
	held := func() bool {
		for _, id := range ids {
			if s.grants[id].Revoked == 0 {
				return false
			}
		}
		return true
	}
	switch err := s.writeThrough(ctx, revokedIn, held, desired, gen); {
	case errors.Is(err, ErrSpent):
		return nil // the ConfigMap holds them revoked already
	case err != nil:
		return err
	}
	return nil
}

// UnwrittenRevocationsOf returns the IDs of userHash's grants revoked on
// this replica that no document read or written holds revoked yet: what a
// retried disconnection writes again.
func (s *Store) UnwrittenRevocationsOf(userHash string) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id := range s.unwritten {
		if e, ok := s.grants[id]; ok && e.User == userHash && e.Revoked > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// GrantRevoked reports whether the grant is revoked on any replica.
func (s *Store) GrantRevoked(id string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.grants[id].Revoked > 0
}

// TouchGrant records a use; written with the next batch (not urgent).
func (s *Store) TouchGrant(id string, at time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.grants[id]
	if !ok || at.Unix() <= e.Used {
		return
	}
	e.Used = at.Unix()
	s.grants[id] = e
	s.changedLocked(false)
}

// Advance is the outcome of AdvanceGrant.
type Advance int

// Outcomes of AdvanceGrant.
const (
	// Advanced: the generation moved on.
	Advanced Advance = iota
	// Replayed: the index is past that generation: the refresh token was
	// used before (the caller revokes the grant).
	Replayed
	// Unknown: the index does not hold the grant.
	Unknown
	// Revoked: the grant is revoked.
	Revoked
)

// AdvanceGrant moves a grant's refresh generation from from to from+1. An
// index behind from (another replica rotated it, and its write has not
// arrived) moves to from+1 too: the sealed refresh token is the proof
// that generation was issued. An index past from is a replay.
func (s *Store) AdvanceGrant(id string, from int, at time.Time) Advance {
	if s == nil {
		return Unknown
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.grants[id]
	switch {
	case !ok:
		return Unknown
	case e.Revoked > 0:
		return Revoked
	case e.Gen > from:
		return Replayed
	}
	e.Gen = from + 1
	e.Used = max(e.Used, at.Unix())
	s.grants[id] = e
	s.changedLocked(true)
	return Advanced
}

// UseCode spends a single-use value (an authorization code's or a consent
// answer's nonce, by a hash of it) of user until it expires: ErrSpent
// when it was spent before, on this replica or on one whose write this
// replica has read; ErrBusy, recording nothing, when user or the console
// holds too many, or user holds the mean share of a nearly full table
// (overFairShare). The spend reaches the other replicas with the next
// batch (about a second): SpendCode is for the values that window matters
// for.
func (s *Store) UseCode(code, userHash string, until time.Time) error {
	if s == nil {
		return ErrBusy
	}
	if !isHex32(userHash) {
		return errors.New("not a user key")
	}
	h := codeHash(code)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reserveLocked(h, userHash, until); err != nil {
		return err
	}
	s.changedLocked(true)
	return nil
}

// ErrNotRecorded is SpendCode unable to write the spend to the ConfigMap:
// nothing was recorded, and the caller must not act on the value.
var ErrNotRecorded = errors.New("the spend could not be recorded in the shared state")

// ErrNoConfigMap is WriteRevocations while the ConfigMap does not exist
// (it was deleted, or never created): there is nothing to write to, and
// every replica refuses every grant meanwhile (GrantsUsable, each
// replica's watch delivering the deletion). The revocations stay on this
// replica and are written at its next read of the ConfigMap, which a
// missing one gets every one to two seconds (Run, missingPoll), so within
// about two seconds of its return (recreatedLocked); meanwhile another
// replica that read it first may serve a grant it recorded itself. A
// restart of this replica before then loses them.
var ErrNoConfigMap = errors.New("the shared state ConfigMap does not exist: every replica refuses every grant until it does")

// SpendCode is UseCode written through: the spend is in the ConfigMap,
// under the API server's optimistic concurrency, before it returns nil.
// A conflict re-reads the ConfigMap; a spend found there (another
// replica's, whose write this replica's watch has not delivered yet) is
// ErrSpent. A write that cannot be made is ErrNotRecorded and leaves
// nothing behind on this replica. Without a ConfigMap (one replica) it is
// UseCode. It blocks on the API server for at most writeTimeout.
func (s *Store) SpendCode(ctx context.Context, code, userHash string, until time.Time) error {
	if s == nil {
		return ErrBusy
	}
	if !s.Enabled() {
		return s.UseCode(code, userHash, until)
	}
	if !isHex32(userHash) {
		return errors.New("not a user key")
	}
	h := codeHash(code)
	// No batch is written while the spend is (writes): one would carry
	// the reservation, and the write-through, conflicting with it, would
	// find the spend in the ConfigMap and call its own spend another's.
	if s.lockWrites(ctx) != nil {
		return ErrNotRecorded
	}
	defer s.unlockWrites()
	var desired *corev1.ConfigMap
	var gen uint64
	var refused error
	s.locked(func() {
		if s.base == nil || s.mode == modeMissing || s.mode == modeForbidden || s.foreign[KeyGrants] {
			refused = ErrNotRecorded
			return
		}
		if refused = s.reserveLocked(h, userHash, until); refused != nil {
			return
		}
		desired, gen = s.desiredLocked(), s.gen
	})
	if refused != nil {
		return refused
	}
	held := func() bool { _, ok := s.codes[h]; return ok }
	err := s.writeThrough(ctx, func(cm *corev1.ConfigMap) bool { return codeIn(cm, h) }, held, desired, gen)
	if err != nil && !errors.Is(err, ErrSpent) {
		// Nothing was recorded that this replica knows of: forget the
		// reservation, so that the value is not refused here as spent. A
		// write that landed although its answer was lost is found by the
		// next attempt's conflict. A spend the ConfigMap holds already
		// (another replica's, which this one's watch merged while its
		// write was in flight, or its own write delivered so) stays: it
		// is spent, and forgetting it here would erase it from the
		// ConfigMap with this replica's next write. An expired copy there
		// (pruned here, the prune not written yet) is no spend: kept, the
		// reservation would refuse the retry as spent, and the next write
		// would put the code back.
		s.locked(func() {
			if !liveCodeIn(s.base, h, s.opts.Now().Unix()) {
				delete(s.codes, h)
			}
		})
	}
	return err
}

// writeThrough writes desired (the local state with a spend reserved),
// re-reading and merging on a conflict, until it lands or the ConfigMap
// shows the spend made already (spentIn: ErrSpent). held reports, under
// s.mu, whether the local state still holds the reservation once a
// conflict's re-read is merged: one the merge removed (the value expired,
// or anything else that empties the local state) would not be in the
// next write, which would then land without it, so it is ErrNotRecorded.
//
// Once started, the write is not cancelled with the caller's request
// (it is bounded by writeTimeout): a caller that gives up midway is no
// failure of the API server, and must not put the replica in the
// failing mode, nor leave a write whose outcome nobody reads.
func (s *Store) writeThrough(ctx context.Context, spentIn func(*corev1.ConfigMap) bool, held func() bool, desired *corev1.ConfigMap, gen uint64) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	for range maxAttempts {
		updated, err := s.opts.Client.Update(ctx, desired, metav1.UpdateOptions{FieldManager: "pgtoolbox-console"})
		switch {
		case err == nil:
			// Recorded; live again only while the watch is open.
			s.locked(func() { s.wroteLandedLocked(updated, gen) })
			sharedWrites.WithLabelValues("written").Inc()
			return nil
		case apierrors.IsConflict(err):
			sharedWrites.WithLabelValues("conflict").Inc()
			fresh, getErr := s.opts.Client.Get(ctx, s.opts.Name, metav1.GetOptions{})
			if getErr != nil {
				_ = s.failed(getErr)
				return ErrNotRecorded
			}
			var spent, unreadable, kept bool
			s.locked(func() {
				s.mergeReadLocked(fresh)
				// The spend is looked for in what was merged, not only
				// in the re-read: a re-read older than the base (the
				// watch delivered a newer write while its answer was on
				// its way) is not merged, and the next write is built on
				// that newer base, which may hold another replica's
				// spend already.
				spent = spentIn(fresh) || spentIn(s.base)
				unreadable, kept = s.foreign[KeyGrants], held()
				desired, gen = s.desiredLocked(), s.gen
			})
			switch {
			case spent:
				return ErrSpent
			case unreadable, !kept:
				return ErrNotRecorded
			}
		default:
			sharedWrites.WithLabelValues("failed").Inc()
			_ = s.failed(err)
			return ErrNotRecorded
		}
	}
	s.locked(s.exhaustedLocked)
	return ErrNotRecorded
}

// codeIn reports whether the grants document of cm holds code hash h.
func codeIn(cm *corev1.ConfigMap, h string) bool {
	_, ok := codeOf(cm, h)
	return ok
}

// markIn reports whether the grants document of cm holds code hash h
// marked mark.
func markIn(cm *corev1.ConfigMap, h, mark string) bool {
	c, ok := codeOf(cm, h)
	return ok && slices.Contains(c.Marks, mark)
}

// liveCodeIn is codeIn of a code that has not expired at now: what the
// rollback of a failed write-through keeps.
func liveCodeIn(cm *corev1.ConfigMap, h string, now int64) bool {
	c, ok := codeOf(cm, h)
	return ok && c.Expiry > now
}

// liveMarkIn is markIn of a code that has not expired at now.
func liveMarkIn(cm *corev1.ConfigMap, h, mark string, now int64) bool {
	c, ok := codeOf(cm, h)
	return ok && c.Expiry > now && slices.Contains(c.Marks, mark)
}

func codeOf(cm *corev1.ConfigMap, h string) (codeEntry, bool) {
	var doc grantsDoc
	if cm == nil || json.Unmarshal([]byte(cm.Data[KeyGrants]), &doc) != nil {
		return codeEntry{}, false
	}
	c, ok := doc.Codes[h]
	return c, ok
}

// MarkCode records mark (a later step of the same piece of work: a
// confirmation answered, then approved, then run) on code, a value
// userHash spent (UseCode, SpendCode) that has not expired. A mark is no
// new entry: it is refused at no bound, so the step after one the fair
// share admitted cannot be refused for want of room. ErrSpent when the
// value carries the mark already, here or in the ConfigMap; ErrNotSpent
// when it is not userHash's spent value (nothing recorded); and, as
// SpendCode, written through to the ConfigMap before it returns nil
// (ErrNotRecorded when it cannot be, leaving nothing behind).
func (s *Store) MarkCode(ctx context.Context, code, mark, userHash string) error {
	if s == nil {
		return ErrNotRecorded
	}
	if !validMark(mark) {
		return errors.New("not a mark")
	}
	h := codeHash(code)
	if s.Enabled() {
		// As SpendCode: no batch carries the mark meanwhile.
		if s.lockWrites(ctx) != nil {
			return ErrNotRecorded
		}
		defer s.unlockWrites()
	}
	desired, gen, err := s.recordMark(h, mark, userHash)
	if err != nil || desired == nil {
		return err
	}
	held := func() bool { c, ok := s.codes[h]; return ok && slices.Contains(c.Marks, mark) }
	err = s.writeThrough(ctx, func(cm *corev1.ConfigMap) bool { return markIn(cm, h, mark) }, held, desired, gen)
	if err != nil && !errors.Is(err, ErrSpent) {
		// As SpendCode: forget the mark unless the ConfigMap holds it
		// already (another replica's, merged while this write was in
		// flight), which this replica's next write would otherwise erase.
		s.locked(func() {
			if c, ok := s.codes[h]; ok && !liveMarkIn(s.base, h, mark, s.opts.Now().Unix()) {
				c.Marks = slices.DeleteFunc(slices.Clone(c.Marks), func(m string) bool { return m == mark })
				s.codes[h] = c
			}
		})
	}
	return err
}

// recordMark takes mu and records mark on code hash h (MarkCode): the
// ConfigMap to write it through to and its gen, or nil when there is
// nothing to write (no ConfigMap configured) or err refuses it.
func (s *Store) recordMark(h, mark, userHash string) (*corev1.ConfigMap, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Enabled() && (s.base == nil || s.mode == modeMissing || s.mode == modeForbidden || s.foreign[KeyGrants]) {
		return nil, 0, ErrNotRecorded
	}
	c, ok := s.codes[h]
	switch {
	case !ok || c.User != userHash || c.Expiry <= s.opts.Now().Unix():
		return nil, 0, ErrNotSpent
	case slices.Contains(c.Marks, mark):
		return nil, 0, ErrSpent
	case len(c.Marks) >= maxMarks:
		return nil, 0, ErrBusy
	}
	c.Marks = unionMarks(c.Marks, []string{mark})
	s.codes[h] = c
	s.changedLocked(true)
	if !s.Enabled() {
		return nil, 0, nil
	}
	return s.desiredLocked(), s.gen, nil
}

// CodeMarked reports whether code was spent, has not expired and carries
// mark (MarkCode), on this replica or on one whose write it has read.
func (s *Store) CodeMarked(code, mark string) bool {
	if s == nil {
		return false
	}
	h := codeHash(code)
	s.mu.Lock()
	defer s.mu.Unlock()
	c, used := s.codes[h]
	return used && c.Expiry > s.opts.Now().Unix() && slices.Contains(c.Marks, mark)
}

// codeHash is how a single-use value is kept: a hash, never the value.
func codeHash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:16])
}

// reserveLocked records code hash h as spent by userHash until until:
// ErrSpent when it is recorded already, ErrBusy when the table is full
// for userHash (overFairShare).
func (s *Store) reserveLocked(h, userHash string, until time.Time) error {
	if _, used := s.codes[h]; used {
		return ErrSpent
	}
	s.pruneGrantsLocked(s.opts.Now().Unix())
	mine := 0
	people := make(map[string]struct{}, len(s.codes))
	for _, c := range s.codes {
		if c.User == userHash {
			mine++
		}
		people[c.User] = struct{}{}
	}
	if codesBound.overFairShare(mine, len(s.codes), len(people)) {
		return ErrBusy
	}
	s.codes[h] = codeEntry{Expiry: until.Unix(), User: userHash}
	return nil
}

// CodeUsed reports whether code was spent (UseCode) and has not expired,
// on this replica or on one whose write this replica has read: an MCP
// confirmation page marks an approval so, which the tool call then reads.
func (s *Store) CodeUsed(code string) bool {
	if s == nil {
		return false
	}
	h := codeHash(code)
	s.mu.Lock()
	defer s.mu.Unlock()
	c, used := s.codes[h]
	return used && c.Expiry > s.opts.Now().Unix()
}

// mergeGrantsLocked folds the grants document raw (empty when the key is
// missing) into the local index.
//
// Grants are merged three ways, not as a union: a grant the document does
// not hold is dropped here, unless this replica recorded it itself less
// than localKeep ago and has read no document holding it since (it is
// waiting for its own write). Codes stay a union.
//
// Why. Every write is an update at the resourceVersion of the document it
// merged, so an entry leaves the document only when a replica drops it on
// purpose: pruned (expired, or RevokedKeep after its revocation), reset by
// a take-over, or the key deleted. A union lets a replica that missed the
// drop write the entry back, and a revoked grant pruned while a replica
// was cut off from the API server came back unrevoked from it. Under
// this rule:
//   - a grant this replica read in a document and that the document no
//     longer holds was dropped elsewhere: dropped here too;
//   - a grant recorded here that no document has held yet is known
//     elsewhere only through a write of this replica that landed after it
//     was recorded, or (a revocation placeholder of a replayed code)
//     through the code's spend, written through within the same request:
//     a revocation of it elsewhere is no older than its recording here,
//     and its prune comes RevokedKeep later on the pruning replica's
//     clock. localKeep is RevokedKeep less a margin for the replicas'
//     clocks and the write delays, so a grant kept by this exception
//     never outlives a pruned revocation of it; past localKeep it is
//     dropped, and its assistant connects again (only a replica cut off
//     from the ConfigMap that long gets there, and it refuses grants
//     after staleGrace anyway);
//   - a replica that never read the document (its first read, the
//     ConfigMap recreated, a take-over it missed) holds only grants it
//     read in an older document (dropped) or recorded (the exception);
//   - a revocation made here is neither dropped by this rule nor pruned
//     before a document read or written holds it (unwritten), so a
//     revocation made while cut off is not lost when its RevokedKeep ends
//     before it reached the ConfigMap. Keeping a revoked entry longer only
//     refuses more.
//
// The replication grace of mcpoauth (a grant not known yet is accepted
// within 30 s of its newest token) is unchanged: a grant dropped here was
// revoked, expired or reset long before, or recorded localKeep ago by a
// replica that has refused grants since.
func (s *Store) mergeGrantsLocked(raw string) {
	var doc grantsDoc
	switch {
	case raw != "" && s.foreignLocked(KeyGrants, raw):
		// A newer grants document may record revocations of grants this
		// replica holds, in a layout it does not read: they are dropped
		// (refused meanwhile anyway). Spent codes are kept: a spend is
		// never undone, and keeping it only refuses more.
		s.dropGrantsLocked()
	case raw != "" && s.takingOverLocked(KeyGrants, raw):
		// Taken over (writers.go): reset rather than read, so that a
		// revocation recorded in the newer layout cannot be lost. Once a
		// document: a merge of the same document again (a conflict of the
		// take-over's own write with a replica of this version that left
		// it alone) must not erase what was recorded since, a spend being
		// written through among it. Every replica of this version drops
		// the grants it holds on finding the reset document (the rule
		// below), including one that never saw the newer one.
		if sum := sha256.Sum256([]byte(raw)); hex.EncodeToString(sum[:]) != s.grantsReset {
			s.grantsReset = hex.EncodeToString(sum[:])
			s.dropGrantsLocked()
		}
	case raw != "" && json.Unmarshal([]byte(raw), &doc) != nil:
		// Never overwritten: what it holds (revocations among it) is not
		// known, and replacing it would forget them. Grants are refused
		// until it is readable again.
		// Logged by mergeLocked once an episode.
		s.foreign[KeyGrants] = true
		s.grantsUnreadable = true
	default:
		now := s.opts.Now()
		held := make(map[string]bool, len(doc.Grants))
		for id, e := range doc.Grants {
			if !validGrantEntry(id, e) {
				continue
			}
			held[id] = true
			s.heldLocked(id, e)
			if prev, ok := s.grants[id]; ok {
				e = mergeGrant(prev, e)
			}
			s.grants[id] = e
		}
		for id := range s.grants {
			if at, mine := s.recorded[id]; held[id] || s.unwritten[id] || (mine && now.Sub(at) < localKeep) {
				continue
			}
			s.dropGrantLocked(id)
		}
		for h, c := range doc.Codes {
			if !isHex32(h) || !isHex32(c.User) {
				continue
			}
			prev, ok := s.codes[h]
			marks := unionMarks(prev.Marks, c.Marks)
			if ok && prev.Expiry >= c.Expiry {
				c = prev
			}
			c.Marks = marks
			s.codes[h] = c
		}
	}
}

// landedLocked records that the grants document raw, this replica's own
// write, landed: what it holds of the grants and revocations recorded
// here is now in the ConfigMap, under the merge rule's first case
// (mergeGrantsLocked). Parsed only while something recorded here waits.
func (s *Store) landedLocked(raw string) {
	if len(s.recorded) == 0 && len(s.unwritten) == 0 {
		return
	}
	var doc grantsDoc
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return
	}
	for id, e := range doc.Grants {
		s.heldLocked(id, e)
	}
}

// heldLocked records that a document read or written holds entry e of
// grant id.
func (s *Store) heldLocked(id string, e grantEntry) {
	delete(s.recorded, id)
	if e.Revoked > 0 {
		delete(s.unwritten, id)
	}
}

// recreatedLocked handles a ConfigMap deleted and recreated: the empty
// document resets the index (mergeGrantsLocked), but a grant recorded
// here within localKeep, or on another replica, survives the reset, and a
// revocation of it that an older document held would not. Every
// revocation this replica knows is made unwritten again, so it is kept
// and written to the new ConfigMap: a reset of the index never undoes a
// revocation (keeping one longer only refuses more).
func (s *Store) recreatedLocked() {
	for id, e := range s.grants {
		if e.Revoked > 0 {
			s.revokedLocked(id)
		}
	}
}

// dropGrantsLocked forgets every grant of the index.
func (s *Store) dropGrantsLocked() {
	s.grants, s.recorded, s.unwritten = map[string]grantEntry{}, map[string]time.Time{}, map[string]bool{}
}

// dropGrantLocked forgets one grant.
func (s *Store) dropGrantLocked(id string) {
	delete(s.grants, id)
	delete(s.recorded, id)
	delete(s.unwritten, id)
}

// recordedLocked notes a grant recorded on this replica, for the merge
// rule (mergeGrantsLocked).
func (s *Store) recordedLocked(id string) {
	if s.Enabled() {
		s.recorded[id] = s.opts.Now()
	}
}

// revokedLocked notes a revocation made on this replica: not pruned
// before a document read holds it.
func (s *Store) revokedLocked(id string) {
	if s.Enabled() {
		s.unwritten[id] = true
	}
}

func (s *Store) pruneGrantsLocked(now int64) {
	for id, e := range s.grants {
		// A revocation no document read or written holds yet is kept,
		// past its expiry and its RevokedKeep too, until one does:
		// pruned, it would be lost, and its grant written back, or left
		// standing, by a replica that never saw it (a code-replay
		// placeholder recorded on a replica cut off from the ConfigMap
		// expires RevokedKeep after the replay). It is an entry of the
		// table, under its bounds, and the first write that lands
		// releases it (landedLocked).
		if e.gone(now) && !s.unwritten[id] {
			s.dropGrantLocked(id)
		}
	}
	for h, c := range s.codes {
		if c.Expiry <= now {
			delete(s.codes, h)
		}
	}
}
