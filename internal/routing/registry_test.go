// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"testing"
	"time"
)

// TestRequestGateDrainsAndRejectsNewRequests verifies orderly shutdown.
func TestRequestGateDrainsAndRejectsNewRequests(t *testing.T) {
	gate := newRequestGate("test")
	release, ok := gate.Acquire()
	if !ok {
		t.Fatal("initial Acquire() failed")
	}
	drained := make(chan error, 1)
	go func() { drained <- gate.CloseAndWait(context.Background()) }()
	select {
	case <-drained:
		t.Fatal("CloseAndWait() returned before active request drained")
	case <-time.After(20 * time.Millisecond):
	}
	if _, ok := gate.Acquire(); ok {
		t.Fatal("Acquire() succeeded while closing")
	}
	release()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("CloseAndWait() did not return after release")
	}
}

// TestRegistryPublishesAndRetiresRequestGate verifies registry ownership.
func TestRegistryPublishesAndRetiresRequestGate(t *testing.T) {
	registry := NewRegistry()
	if registry.Published() {
		t.Fatal("new registry is published")
	}
	registry.Publish(&Snapshot{Git: map[string]*GitSource{}, Objects: map[string]*ObjectSource{}})
	if !registry.Published() {
		t.Fatal("registry did not record publication")
	}
	first := registry.RequestGate("git/agents/app")
	if first != registry.RequestGate("git/agents/app") {
		t.Fatal("request gate was not stable")
	}
	if err := registry.Retire(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if first == registry.RequestGate("git/agents/app") {
		t.Fatal("retired request gate was reused")
	}
}
