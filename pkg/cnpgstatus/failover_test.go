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

package cnpgstatus

import (
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
)

func primaryInst(standbys ...instancestatus.Standby) RehearsalInstance {
	ready := true
	return RehearsalInstance{
		Name: "db-1", Fresh: true, PodReady: &ready, Node: "n1",
		Reading: &instancestatus.Reading{IsPrimary: true, CurrentLSN: "0/5000000", Standbys: standbys},
	}
}

func replica(name, received, replay, node string) RehearsalInstance {
	ready := true
	return RehearsalInstance{
		Name: name, Fresh: true, PodReady: &ready, Node: node,
		Reading: &instancestatus.Reading{ReceivedLSN: received, ReplayLSN: replay},
	}
}

func quorumObj(names ...string) *cnpgv1.FailoverQuorum {
	return &cnpgv1.FailoverQuorum{Status: cnpgv1.FailoverQuorumStatus{Method: "any", StandbyNumber: 1, StandbyNames: names, Primary: "db-1"}}
}

// TestRehearseFollowsTheOperatorsElection checks each rule of
// PostgresqlStatusList.Less, the fencing short-cut and the quorum check
// (R + W > N), and that data loss is claimed zero only under a
// synchronous replication the primary reported satisfied.
func TestRehearseFollowsTheOperatorsElection(t *testing.T) {
	t.Parallel()
	async := SyncReading{Mode: SyncModeNone}
	quorumSync := SyncReading{Mode: SyncModeExplicit, Method: "any", Number: 1, FailoverQuorum: true}
	unreadable := replica("db-4", "0/5000000", "0/5000000", "n4")
	unreadable.Fresh = false

	cases := []struct {
		name        string
		in          RehearsalInput
		outcome     string
		candidate   string
		loss        string
		class       Class
		certain     bool
		behind      int64 // -1: not set
		textHas     string
		quorumMet   *bool
		rankingHead []string
	}{
		{
			name: "async: the most advanced received LSN wins",
			in: RehearsalInput{Primary: "db-1", Sync: async, Instances: []RehearsalInstance{
				primaryInst(), replica("db-2", "0/4000000", "0/4000000", "n2"), replica("db-3", "0/4F00000", "0/3000000", "n3"),
			}},
			outcome: RehearsalPromote, candidate: "db-3", loss: LossUpTo, class: ClassWarning, certain: true, behind: 0x100000,
			textHas: "losing up to 1.0 MiB", rankingHead: []string{"db-3", "db-2"},
		},
		{
			name: "received tie: replay LSN, then name",
			in: RehearsalInput{Primary: "db-1", Sync: async, Instances: []RehearsalInstance{
				primaryInst(), replica("db-3", "0/5000000", "0/4000000", "n3"), replica("db-2", "0/5000000", "0/4000000", "n2"), replica("db-4", "0/5000000", "0/4800000", "n4"),
			}},
			outcome: RehearsalPromote, candidate: "db-4", loss: LossUpTo, class: ClassOK, certain: true, behind: 0,
			rankingHead: []string{"db-4", "db-2", "db-3"},
		},
		{
			name: "fenced replica sorts after the others",
			in: RehearsalInput{Primary: "db-1", Sync: async, Fencing: FencingReading{State: FencingSome, Instances: []string{"db-3"}}, Instances: []RehearsalInstance{
				primaryInst(), replica("db-2", "0/4000000", "0/4000000", "n2"), replica("db-3", "0/5000000", "0/5000000", "n3"),
			}},
			outcome: RehearsalPromote, candidate: "db-2", loss: LossUpTo, class: ClassWarning, certain: true, behind: 0x1000000,
			rankingHead: []string{"db-2", "db-3"},
		},
		{
			name: "every instance fenced",
			in: RehearsalInput{Primary: "db-1", Sync: async, Fencing: FencingReading{State: FencingAll, Instances: []string{"*"}}, Instances: []RehearsalInstance{
				primaryInst(), replica("db-2", "0/4000000", "0/4000000", "n2"),
			}},
			outcome: RehearsalBlocked, class: ClassCritical, certain: true, behind: -1, loss: LossUnknown,
		},
		{
			name: "priority sync standby: no acknowledged commit lost",
			in: RehearsalInput{Primary: "db-1", Sync: SyncReading{Mode: SyncModeExplicit, Method: "first", Number: 1}, Instances: []RehearsalInstance{
				primaryInst(instancestatus.Standby{ApplicationName: "db-2", SyncState: "sync"}, instancestatus.Standby{ApplicationName: "db-3", SyncState: "potential"}),
				replica("db-2", "0/4FFF000", "0/4FFF000", "n2"), replica("db-3", "0/4000000", "0/4000000", "n3"),
			}},
			outcome: RehearsalPromote, candidate: "db-2", loss: LossNone, class: ClassOK, certain: true, behind: 0x1000,
			textHas: "no acknowledged commit lost",
		},
		{
			name: "candidate potential under sync: loss possible",
			in: RehearsalInput{Primary: "db-1", Sync: SyncReading{Mode: SyncModeExplicit, Method: "first", Number: 1}, Instances: []RehearsalInstance{
				primaryInst(instancestatus.Standby{ApplicationName: "db-2", SyncState: "sync"}, instancestatus.Standby{ApplicationName: "db-3", SyncState: "potential"}),
				replica("db-2", "0/4000000", "0/4000000", "n2"), replica("db-3", "0/4FFF000", "0/4000000", "n3"),
			}},
			outcome: RehearsalPromote, candidate: "db-3", loss: LossUpTo, class: ClassWarning, certain: true, behind: 0x1000,
		},
		{
			name: "failover quorum met",
			in: RehearsalInput{Primary: "db-1", Sync: quorumSync, QuorumReadable: true, Quorum: quorumObj("db-2", "db-3"), Instances: []RehearsalInstance{
				primaryInst(instancestatus.Standby{ApplicationName: "db-2", SyncState: "quorum"}, instancestatus.Standby{ApplicationName: "db-3", SyncState: "quorum"}),
				replica("db-2", "0/5000000", "0/5000000", "n2"), replica("db-3", "0/4000000", "0/4000000", "n3"),
			}},
			outcome: RehearsalPromote, candidate: "db-2", loss: LossNone, class: ClassOK, certain: true, behind: 0, quorumMet: new(true),
		},
		{
			name: "failover quorum not met: the operator refuses",
			in: RehearsalInput{Primary: "db-1", Sync: quorumSync, QuorumReadable: true, Quorum: quorumObj("db-2", "db-3", "db-4"), Instances: []RehearsalInstance{
				primaryInst(), replica("db-2", "0/5000000", "0/5000000", "n2"), unreadable,
			}},
			outcome: RehearsalBlocked, candidate: "db-2", class: ClassCritical, certain: false, behind: 0, quorumMet: new(false),
			textHas: "1 promotable of 3, 1 required",
		},
		{
			name: "failover quorum with an unread member: cannot tell",
			in: RehearsalInput{Primary: "db-1", Sync: quorumSync, QuorumReadable: true, Quorum: quorumObj("db-2", "db-4"), Instances: []RehearsalInstance{
				primaryInst(), replica("db-2", "0/5000000", "0/5000000", "n2"), unreadable,
			}},
			outcome: RehearsalPromote, candidate: "db-2", loss: LossUpTo, class: ClassWarning, certain: false, behind: 0,
		},
		{
			name: "failover quorum requested, no object",
			in: RehearsalInput{Primary: "db-1", Sync: quorumSync, QuorumReadable: true, Instances: []RehearsalInstance{
				primaryInst(), replica("db-2", "0/5000000", "0/5000000", "n2"),
			}},
			outcome: RehearsalBlocked, candidate: "db-2", class: ClassCritical, certain: true, behind: 0, quorumMet: new(false),
		},
		{
			name: "failover quorum not readable: uncertain",
			in: RehearsalInput{Primary: "db-1", Sync: quorumSync, Instances: []RehearsalInstance{
				primaryInst(), replica("db-2", "0/5000000", "0/5000000", "n2"),
			}},
			outcome: RehearsalPromote, candidate: "db-2", loss: LossUpTo, class: ClassWarning, certain: false, behind: 0,
		},
		{
			name:    "single instance",
			in:      RehearsalInput{Primary: "db-1", Sync: async, Instances: []RehearsalInstance{primaryInst()}},
			outcome: RehearsalNoCandidate, class: ClassWarning, certain: true, behind: -1, loss: LossUnknown,
		},
		{
			name: "an unread replica makes it uncertain",
			in: RehearsalInput{Primary: "db-1", Sync: async, Instances: []RehearsalInstance{
				primaryInst(), replica("db-2", "0/5000000", "0/5000000", "n2"), unreadable,
			}},
			outcome: RehearsalPromote, candidate: "db-2", loss: LossUpTo, class: ClassWarning, certain: false, behind: 0,
			rankingHead: []string{"db-2", "db-4"},
		},
		{
			name:    "no replica read",
			in:      RehearsalInput{Primary: "db-1", Sync: async, Instances: []RehearsalInstance{primaryInst(), unreadable}},
			outcome: RehearsalUnknown, class: ClassUnknown, certain: false, behind: -1, loss: LossUnknown,
		},
		{
			name: "same node as the primary",
			in: RehearsalInput{Primary: "db-1", Sync: async, Instances: []RehearsalInstance{
				primaryInst(), replica("db-2", "0/5000000", "0/5000000", "n1"),
			}},
			outcome: RehearsalPromote, candidate: "db-2", loss: LossUpTo, class: ClassWarning, certain: true, behind: 0,
			textHas: "primary's node",
		},
		{
			name: "primary not read: loss unknown",
			in: RehearsalInput{Primary: "db-1", Sync: async, Instances: []RehearsalInstance{
				func() RehearsalInstance { p := primaryInst(); p.Fresh = false; return p }(), replica("db-2", "0/5000000", "0/5000000", "n2"),
			}},
			outcome: RehearsalPromote, candidate: "db-2", loss: LossUnknown, class: ClassUnknown, certain: true, behind: -1,
		},
		{
			name:    "hibernated",
			in:      RehearsalInput{Primary: "db-1", Hibernated: true},
			outcome: RehearsalNotApplied, class: ClassInfo, certain: true, behind: -1, loss: LossUnknown,
		},
	}
	for _, c := range cases {
		got := Rehearse(c.in)
		if got.Outcome != c.outcome || got.Candidate != c.candidate || got.Class != c.class || got.Certain != c.certain {
			t.Errorf("%s: outcome %s candidate %q class %s certain %v; want %s %q %s %v (%s)",
				c.name, got.Outcome, got.Candidate, got.Class, got.Certain, c.outcome, c.candidate, c.class, c.certain, got.Text)
			continue
		}
		if c.loss != "" && got.DataLoss != c.loss {
			t.Errorf("%s: loss %s, want %s (%s)", c.name, got.DataLoss, c.loss, got.DataLossText)
		}
		switch {
		case c.behind < 0 && got.BehindBytes != nil:
			t.Errorf("%s: behind %d, want unset", c.name, *got.BehindBytes)
		case c.behind >= 0 && (got.BehindBytes == nil || *got.BehindBytes != c.behind):
			t.Errorf("%s: behind %v, want %d", c.name, got.BehindBytes, c.behind)
		}
		if c.textHas != "" && !strings.Contains(got.Text, c.textHas) {
			t.Errorf("%s: text %q lacks %q", c.name, got.Text, c.textHas)
		}
		if c.quorumMet != nil && (got.Quorum == nil || got.Quorum.Met == nil || *got.Quorum.Met != *c.quorumMet) {
			t.Errorf("%s: quorum %+v, want met=%v", c.name, got.Quorum, *c.quorumMet)
		}
		for i, n := range c.rankingHead {
			if i >= len(got.Ranking) || got.Ranking[i].Name != n {
				t.Errorf("%s: ranking %+v, want head %v", c.name, got.Ranking, c.rankingHead)
				break
			}
		}
	}
}

func TestParseLSN(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]uint64{"0/0": 0, "0/3000000": 0x3000000, "1/0": 1 << 32, "FFFFFFFF/FFFFFFFF": 1<<64 - 1} {
		if got, ok := ParseLSN(s); !ok || got != want {
			t.Errorf("ParseLSN(%q) = %x, %v", s, got, ok)
		}
	}
	for _, s := range []string{"", "0", "/1", "1/", "G/0", "100000000/0", "0/0/0"} {
		if _, ok := ParseLSN(s); ok {
			t.Errorf("ParseLSN(%q) accepted", s)
		}
	}
}

// TestRehearsalHonesty covers the cases where a reading looks
// conclusive but is not: synchronous_commit lowering what sync_state
// promises, an unread replica behind a fenced one, a replica reporting
// itself primary, a quorum whose set the primary undercounts, a legacy
// quorum of unknown size, and a replica cluster the quorum does not gate.
func TestRehearsalHonesty(t *testing.T) {
	t.Parallel()
	prioritySync := SyncReading{Mode: SyncModeExplicit, Method: "first", Number: 1}
	syncPair := func(commit string) RehearsalInput {
		return RehearsalInput{Primary: "db-1", Sync: prioritySync, SynchronousCommit: commit, Instances: []RehearsalInstance{
			primaryInst(instancestatus.Standby{ApplicationName: "db-2", SyncState: "sync"}),
			replica("db-2", "0/5000000", "0/5000000", "n2"),
		}}
	}
	unread := func(name string) RehearsalInstance {
		r := replica(name, "", "", "n9")
		r.Fresh, r.Reading = false, nil
		return r
	}
	selfPrimary := replica("db-3", "0/5000000", "0/5000000", "n3")
	selfPrimary.Reading.IsPrimary = true
	quorumSync := SyncReading{Mode: SyncModeExplicit, Method: "any", Number: 1, FailoverQuorum: true}
	// db-3 is in the FailoverQuorum but disconnected: missing from the
	// primary's pg_stat_replication, and not read by the console.
	undercounted := RehearsalInput{
		Primary: "db-1", Sync: quorumSync, Quorum: quorumObj("db-2", "db-3"), QuorumReadable: true,
		Instances: []RehearsalInstance{
			primaryInst(instancestatus.Standby{ApplicationName: "db-2", SyncState: "quorum"}),
			replica("db-2", "0/5000000", "0/5000000", "n2"), unread("db-3"),
		},
	}
	legacy := RehearsalInput{
		Primary: "db-1", Sync: SyncReading{Mode: SyncModeLegacy, Method: "any", MinSync: 1, MaxSync: 1},
		Instances: []RehearsalInstance{
			primaryInst(instancestatus.Standby{ApplicationName: "db-2", SyncState: "quorum"}),
			replica("db-2", "0/5000000", "0/5000000", "n2"),
		},
	}
	replicaCluster := RehearsalInput{
		Primary: "db-1", Sync: quorumSync, ReplicaCluster: true, QuorumReadable: true, // no FailoverQuorum object
		Instances: []RehearsalInstance{primaryInst(), replica("db-2", "0/5000000", "0/5000000", "n2")},
	}

	cases := []struct {
		name     string
		in       RehearsalInput
		outcome  string
		loss     string
		textHas  string
		lossHas  string
		noQuorum bool
	}{
		{"sync, default synchronous_commit", syncPair(""), RehearsalPromote, LossNone, "for transactions using the cluster default", "a role, a database, a session or a transaction", false},
		{"sync, synchronous_commit off", syncPair("off"), RehearsalPromote, LossUpTo, "not proven synchronous", "synchronous_commit is off", false},
		{"sync, synchronous_commit local", syncPair("Local"), RehearsalPromote, LossUpTo, "", "synchronous_commit is local", false},
		{"sync, synchronous_commit false (a boolean spelling)", syncPair("false"), RehearsalPromote, LossUpTo, "", "synchronous_commit is off", false},
		{"sync, remote_write: written, not flushed", syncPair("remote_write"), RehearsalPromote, LossNone, "", "not flushed", false},
		// "None" is a fact about the sweep: an old sweep cannot carry it.
		{"sync, but the sweep is stale", func() RehearsalInput {
			in := syncPair("")
			in.SweepStale = "the sweep has not completed for three intervals"
			return in
		}(), RehearsalPromote, LossUnknown, "unknown data loss", "whether that still holds is not known", false},
		{"sync, synchronous_commit padded: PostgreSQL refuses it", syncPair(" on"), RehearsalPromote, LossUnknown, "unknown data loss", "does not accept", false},
		{
			"read replica fenced, an unread one open: cannot tell",
			RehearsalInput{Primary: "db-1", Fencing: FencingReading{State: FencingSome, Instances: []string{"db-2"}}, Instances: []RehearsalInstance{
				primaryInst(), replica("db-2", "0/5000000", "0/5000000", "n2"), unread("db-3"),
			}},
			RehearsalUnknown, LossUnknown, "db-3 could not be read", "", false,
		},
		{
			"read replica fenced, the unread one fenced too: blocked",
			RehearsalInput{Primary: "db-1", Fencing: FencingReading{State: FencingSome, Instances: []string{"db-2", "db-3"}}, Instances: []RehearsalInstance{
				primaryInst(), replica("db-2", "0/5000000", "0/5000000", "n2"), unread("db-3"),
			}},
			RehearsalBlocked, LossUnknown, "fenced or cannot be promoted", "", false,
		},
		{
			"a replica reporting itself primary is named, not called unread",
			RehearsalInput{Primary: "db-1", Instances: []RehearsalInstance{primaryInst(), selfPrimary}},
			RehearsalUnknown, LossUnknown, "db-3 reports itself primary", "", false,
		},
		{"quorum N from the FailoverQuorum, not the primary's walsenders", undercounted, RehearsalPromote, LossUpTo, "", "1 + 1 > 2", false},
		{"legacy quorum: W not published, cannot tell", legacy, RehearsalPromote, LossUpTo, "", "cannot tell", false},
		{"replica cluster: the failover quorum does not gate", replicaCluster, RehearsalPromote, LossUpTo, "", "", true},
	}
	for _, c := range cases {
		got := Rehearse(c.in)
		if got.Outcome != c.outcome || got.DataLoss != c.loss {
			t.Errorf("%s: outcome %s loss %s, want %s %s (%s | %s)", c.name, got.Outcome, got.DataLoss, c.outcome, c.loss, got.Text, got.DataLossText)
			continue
		}
		if !strings.Contains(got.Text, c.textHas) {
			t.Errorf("%s: text %q lacks %q", c.name, got.Text, c.textHas)
		}
		if !strings.Contains(got.DataLossText, c.lossHas) {
			t.Errorf("%s: data loss text %q lacks %q", c.name, got.DataLossText, c.lossHas)
		}
		if c.noQuorum && got.Quorum != nil {
			t.Errorf("%s: quorum gate %+v on a replica cluster", c.name, got.Quorum)
		}
		if strings.Contains(got.DataLossText, "too few members") && c.in.Sync.Mode == SyncModeLegacy {
			t.Errorf("%s: legacy mode claims too few members: %s", c.name, got.DataLossText)
		}
	}
}

func TestAsyncRehearsalOnAStaleSweepIsUnknown(t *testing.T) {
	t.Parallel()
	caughtUp := func(stale string) RehearsalInput {
		return RehearsalInput{Primary: "db-1", SweepStale: stale, Instances: []RehearsalInstance{
			primaryInst(instancestatus.Standby{ApplicationName: "db-2"}),
			replica("db-2", "0/5000000", "0/5000000", "n2"),
		}}
	}
	fresh := Rehearse(caughtUp(""))
	if fresh.Class != ClassOK {
		t.Fatalf("fresh caught-up async replica: class %s, want ok (%s)", fresh.Class, fresh.DataLossText)
	}
	stale := Rehearse(caughtUp("the sweep has not completed for three intervals"))
	if stale.Class != ClassUnknown {
		t.Errorf("stale caught-up async replica: class %s, want unknown (%s)", stale.Class, stale.DataLossText)
	}
	if !strings.Contains(stale.DataLossText, "whether it still has is not known") {
		t.Errorf("stale data loss text %q does not say the distance is unknown", stale.DataLossText)
	}
}

func TestSynchronousCommitLevel(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]string{"": "on", "ON": "on", "yes": "on", "1": "on", "0": "off", "No": "off", "remote_apply": "remote_apply", "REMOTE_WRITE": "remote_write", "local": "local"} {
		if got, ok := SynchronousCommitLevel(v); !ok || got != want {
			t.Errorf("SynchronousCommitLevel(%q) = %q, %v; want %q", v, got, ok, want)
		}
	}
	for _, v := range []string{" on", "of", "remote", "t"} {
		if _, ok := SynchronousCommitLevel(v); ok {
			t.Errorf("SynchronousCommitLevel(%q) accepted", v)
		}
	}
}
