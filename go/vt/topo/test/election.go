/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package test

import (
	"testing"
	"time"

	"context"

	"vitess.io/vitess/go/vt/topo"
)

func waitForMasterID(t *testing.T, mp topo.MasterParticipation, expected string) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		master, err := mp.GetCurrentMasterID(context.Background())
		if err != nil {
			t.Fatalf("GetCurrentMasterID failed: %v", err)
		}

		if master == expected {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("GetCurrentMasterID timed out with %v, expected %v", master, expected)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// checkElection runs the tests on the MasterParticipation part of the
// topo.Conn API.
func checkElection(t *testing.T, ts *topo.Server) {
	conn, err := ts.ConnForCell(context.Background(), topo.GlobalCell)
	if err != nil {
		t.Fatalf("ConnForCell(global) failed: %v", err)
	}
	name := "testmp"

	// create a new MasterParticipation
	id1 := "id1"
	mp1, err := conn.NewMasterParticipation(name, id1)
	if err != nil {
		t.Fatalf("cannot create mp1: %v", err)
	}

	// no primary yet, check name
	waitForMasterID(t, mp1, "")

	// wait for id1 to be the primary
	ctx1, err := mp1.WaitForMastership()
	if err != nil {
		t.Fatalf("mp1 cannot become master: %v", err)
	}

	// A lot of implementations use a toplevel directory for their elections.
	// Make sure it is marked as 'Ephemeral'.
	entries, err := conn.ListDir(context.Background(), "/", true /*full*/)
	if err != nil {
		t.Fatalf("ListDir(/) failed: %v", err)
	}
	for _, e := range entries {
		if e.Name != topo.CellsPath {
			if !e.Ephemeral {
				t.Errorf("toplevel directory that is not ephemeral: %v", e)
			}
		}
	}

	// get the current primary name, better be id1
	waitForMasterID(t, mp1, id1)

	// create a second MasterParticipation on same name
	id2 := "id2"
	mp2, err := conn.NewMasterParticipation(name, id2)
	if err != nil {
		t.Fatalf("cannot create mp2: %v", err)
	}

	// wait until mp2 gets to be the primary in the background
	mp2IsMaster := make(chan error)
	var mp2Context context.Context
	go func() {
		var err error
		mp2Context, err = mp2.WaitForMastership()
		mp2IsMaster <- err
	}()

	// ask mp2 for primary name, should get id1
	waitForMasterID(t, mp2, id1)

	// stop mp1
	mp1.Stop()

	// this should have closed ctx1 as soon as possible,
	// so 5s should be enough in tests. This will be used during lameduck
	// when the server exits, so we can't wait too long anyway.
	select {
	case <-ctx1.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("shutting down mp1 didn't close ctx1 in time")
	}

	// now mp2 should be primary
	err = <-mp2IsMaster
	if err != nil {
		t.Fatalf("mp2 awoke with error: %v", err)
	}

	// ask mp2 for primary name, should get id2
	waitForMasterID(t, mp2, id2)

	// stop mp2, we're done
	mp2.Stop()

	// mp2Context should then close.
	select {
	case <-mp2Context.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("shutting down mp2 didn't close mp2Context in time")
	}

	// At this point, we should be able to call WaitForMastership
	// again, and it should return topo.ErrInterrupted.  Testing
	// this here as this is what the vtctld workflow manager loop
	// does, for instance. There is a go routine that runs
	// WaitForMastership and needs to exit cleanly at the end.
	_, err = mp2.WaitForMastership()
	if !topo.IsErrType(err, topo.Interrupted) {
		t.Errorf("wrong error returned by WaitForMastership, got %v expected %v", err, topo.NewError(topo.Interrupted, ""))
	}
}
