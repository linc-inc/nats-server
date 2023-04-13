// Copyright 2021-2023 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

func TestNRGSimple(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R3S", 3)
	defer c.shutdown()

	rg := c.createRaftGroup("TEST", 3, newStateAdder)
	rg.waitOnLeader()
	// Do several state transitions.
	rg.randomMember().(*stateAdder).proposeDelta(11)
	rg.randomMember().(*stateAdder).proposeDelta(11)
	rg.randomMember().(*stateAdder).proposeDelta(-22)
	// Wait for all members to have the correct state.
	rg.waitOnTotal(t, 0)
}

func TestNRGPIndex(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R3S", 3)
	defer c.shutdown()

	rg := c.createRaftGroup("TEST", 3, newStateAdder)
	rg.waitOnLeader()

	rafts := make([]*raft, 0, len(rg))
	for _, n := range rg {
		rafts = append(rafts, n.node().(*raft))
	}

	for _, r := range rafts {
		switch {
		case r.pterm != 1:
			t.Fatalf("initial state should be pterm 1, got %d", r.pterm)
		case r.pindex != 1:
			t.Fatalf("initial state should be pindex 1, got %d", r.pindex)
		case r.commit != 1:
			t.Fatalf("initial state should be commit 1, got %d", r.commit)
		case r.applied != 1:
			t.Fatalf("initial state should be applied 1, got %d", r.applied)
		}
	}

	randomAdder := func() *stateAdder {
		return rg.randomMember().(*stateAdder)
	}

	type step struct {
		total  int64
		term   uint64
		pterm  uint64
		pindex uint64
		commit uint64
		action func()
	}

	for i, s := range []step{
		{1, 1, 1, 2, 2, func() { randomAdder().proposeDelta(1) }},
		{2, 1, 1, 3, 3, func() { randomAdder().proposeDelta(1) }},
		{3, 1, 1, 4, 4, func() { randomAdder().proposeDelta(1) }},
		{4, 1, 1, 5, 5, func() { randomAdder().proposeDelta(1) }},
		{4, 2, 2, 5, 5, func() { rg.leader().node().StepDown(); rg.waitOnLeader() }},
		{4, 2, 2, 6, 6, func() { randomAdder().proposeDelta(1) }},
	} {
		s.action()
		rg.waitOnTotal(t, s.total)

		for _, r := range rafts {
			switch {
			case r.term != s.term:
				t.Fatalf("loop %d state should be term %d, got %d", i, s.term, r.term)
			//case r.pterm != s.pterm:
			//	t.Fatalf("loop %d state should be pterm %d, got %d", i, s.pterm, r.pterm)
			case r.pindex != s.pindex:
				t.Fatalf("loop %d state should be pindex %d, got %d", i, s.pindex, r.pindex)
			case r.commit != s.commit:
				t.Fatalf("loop %d state should be commit %d, got %d", i, s.commit, r.commit)
			case r.commit != r.applied:
				t.Fatalf("loop %d state should have applied %d commits but has only applied %d", i, r.commit, r.applied)
			}
		}
	}
}

func TestNRGSnapshotAndRestart(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R3S", 3)
	defer c.shutdown()

	rg := c.createRaftGroup("TEST", 3, newStateAdder)
	rg.waitOnLeader()

	var expectedTotal int64

	leader := rg.leader().(*stateAdder)
	sm := rg.nonLeader().(*stateAdder)

	for i := 0; i < 1000; i++ {
		delta := rand.Int63n(222)
		expectedTotal += delta
		leader.proposeDelta(delta)

		if i == 250 {
			// Let some things catchup.
			time.Sleep(50 * time.Millisecond)
			// Snapshot leader and stop and snapshot a member.
			leader.snapshot(t)
			sm.snapshot(t)
			sm.stop()
		}
	}
	// Restart.
	sm.restart()
	// Wait for all members to have the correct state.
	rg.waitOnTotal(t, expectedTotal)
}

func TestNRGAppendEntryEncode(t *testing.T) {
	ae := &appendEntry{
		term:   1,
		pindex: 0,
	}

	// Test leader should be _EMPTY_ or exactly idLen long
	ae.leader = "foo_bar_baz"
	_, err := ae.encode(nil)
	require_Error(t, err, errLeaderLen)

	// Empty ok (noLeader)
	ae.leader = noLeader // _EMPTY_
	_, err = ae.encode(nil)
	require_NoError(t, err)

	ae.leader = "DEREK123"
	_, err = ae.encode(nil)
	require_NoError(t, err)

	// Buffer reuse
	var rawSmall [32]byte
	var rawBigger [64]byte

	b := rawSmall[:]
	ae.encode(b)
	if b[0] != 0 {
		t.Fatalf("Expected arg buffer to not be used")
	}
	b = rawBigger[:]
	ae.encode(b)
	if b[0] == 0 {
		t.Fatalf("Expected arg buffer to be used")
	}

	// Test max number of entries.
	for i := 0; i < math.MaxUint16+1; i++ {
		ae.entries = append(ae.entries, &Entry{EntryNormal, nil})
	}
	_, err = ae.encode(b)
	require_Error(t, err, errTooManyEntries)
}

func TestNRGAppendEntryDecode(t *testing.T) {
	ae := &appendEntry{
		leader: "12345678",
		term:   1,
		pindex: 0,
	}
	for i := 0; i < math.MaxUint16; i++ {
		ae.entries = append(ae.entries, &Entry{EntryNormal, nil})
	}
	buf, err := ae.encode(nil)
	require_NoError(t, err)

	// Truncate buffer first.
	var node *raft
	short := buf[0 : len(buf)-1024]
	_, err = node.decodeAppendEntry(short, nil, _EMPTY_)
	require_Error(t, err, errBadAppendEntry)

	for i := 0; i < 100; i++ {
		b := copyBytes(buf)
		// modifying the header (idx < 42) will not result in an error by decodeAppendEntry
		bi := 42 + rand.Intn(len(b)-42)
		if b[bi] != 0 && bi != 40 {
			b[bi] = 0
			_, err = node.decodeAppendEntry(b, nil, _EMPTY_)
			require_Error(t, err, errBadAppendEntry)
		}
	}
}
