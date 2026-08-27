// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"sync"
	"time"

	"github.com/sourcegateway/sourcegateway/api/v1alpha1"
	"github.com/sourcegateway/sourcegateway/internal/objectstore"
	"github.com/sourcegateway/sourcegateway/internal/policy"
)

// GitRefreshMarker requests an immediate mirror refresh when present.
const GitRefreshMarker = ".sourcegateway-refresh-required"

// GitSource is the serving configuration for a Git mirror.
type GitSource struct {
	Namespace    string
	Name         string
	Path         string
	MirrorPath   string
	UpstreamURL  string
	AuthHeader   string
	Policy       *policy.Policy
	Rules        []v1alpha1.GitAuthorizationRule
	RefreshedAt  time.Time
	MaxStaleness time.Duration
	Requests     *RequestGate
	State        *GitState
}

// GitState coordinates mirror replacement with active Git operations.
type GitState struct {
	Gate   sync.RWMutex
	Update sync.Mutex
}

// ObjectSource is the serving configuration for an object bucket.
type ObjectSource struct {
	Namespace          string
	Name               string
	Bucket             string
	Backend            v1alpha1.ObjectBackend
	Policy             *policy.Policy
	Provider           objectstore.ObjectProvider
	Capacity           int64
	TTL                time.Duration
	Requests           *RequestGate
	CredentialRevision string
}

// RequestGate drains active requests before a routed resource is retired.
type RequestGate struct {
	key     string
	mu      sync.Mutex
	active  int
	closing bool
	drained chan struct{}
}

// newRequestGate creates an open request gate owned under key.
func newRequestGate(key string) *RequestGate {
	return &RequestGate{key: key, drained: make(chan struct{})}
}

// Acquire registers an active user and returns its idempotent release function.
func (g *RequestGate) Acquire() (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closing {
		return nil, false
	}
	g.active++
	released := false
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if released {
			return
		}
		released = true
		g.active--
		if g.closing && g.active == 0 {
			close(g.drained)
		}
	}, true
}

// CloseAndWait rejects new users and waits for active users to drain.
func (g *RequestGate) CloseAndWait(ctx context.Context) error {
	g.mu.Lock()
	if !g.closing {
		g.closing = true
		if g.active == 0 {
			close(g.drained)
		}
	}
	drained := g.drained
	g.mu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Snapshot is an immutable view of all currently served resources.
type Snapshot struct {
	Git     map[string]*GitSource
	Objects map[string]*ObjectSource
}

// Registry publishes routing snapshots and owns resource request gates.
type Registry struct {
	mu           sync.RWMutex
	snapshot     *Snapshot
	published    bool
	requestGates map[string]*RequestGate
	gitStates    map[string]*GitState
	refreshes    chan struct{}
}

// NewRegistry creates an empty unpublished registry.
func NewRegistry() *Registry {
	return &Registry{
		snapshot:     &Snapshot{Git: map[string]*GitSource{}, Objects: map[string]*ObjectSource{}},
		requestGates: make(map[string]*RequestGate), gitStates: make(map[string]*GitState), refreshes: make(chan struct{}, 64),
	}
}

// Load returns the current routing snapshot.
func (r *Registry) Load() *Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshot
}

// Publish replaces the routing snapshot when snapshot is non-nil.
func (r *Registry) Publish(snapshot *Snapshot) {
	if snapshot == nil {
		return
	}
	r.mu.Lock()
	r.snapshot = snapshot
	r.published = true
	r.mu.Unlock()
}

// Published reports whether a snapshot has been explicitly published.
func (r *Registry) Published() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.published
}

// RequestGate returns the stable request gate associated with key.
func (r *Registry) RequestGate(key string) *RequestGate {
	r.mu.Lock()
	defer r.mu.Unlock()
	gate := r.requestGates[key]
	if gate == nil {
		gate = newRequestGate(key)
		r.requestGates[key] = gate
	}
	return gate
}

// GitState returns the stable mirror state associated with key.
func (r *Registry) GitState(key string) *GitState {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.gitStates[key]
	if state == nil {
		state = &GitState{}
		r.gitStates[key] = state
	}
	return state
}

// RequestGitRefresh queues a refresh without blocking the caller.
func (r *Registry) RequestGitRefresh() {
	select {
	case r.refreshes <- struct{}{}:
	default:
	}
}

// GitRefreshRequests returns the refresh request stream.
func (r *Registry) GitRefreshRequests() <-chan struct{} {
	return r.refreshes
}

// Retire drains and forgets a request gate and its associated Git state.
func (r *Registry) Retire(ctx context.Context, gate *RequestGate) error {
	if err := gate.CloseAndWait(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	if r.requestGates[gate.key] == gate {
		delete(r.requestGates, gate.key)
		delete(r.gitStates, gate.key)
	}
	r.mu.Unlock()
	return nil
}
