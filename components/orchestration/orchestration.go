// Package orchestration is the registry of the agents of one AgentCore, for example the
// PM agent and the designer agent of one user. It holds the agents in insertion order,
// spawns them through a factory, and removes one when it is disposed.
//
// The factory is injected so that this package (layer 8) never imports the root
// package that owns the hubs: aec.AgentCore passes a factory that builds an
// agent.Agent over its hubs.
package orchestration

import (
	"context"
	"errors"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
)

// ErrAgentExists is matched, with errors.Is, by the error Spawn returns for an id
// that is already registered.
var ErrAgentExists = errors.New("orchestration: agent already exists")

// agentExistsError carries the text `Agent "<id>" already exists` (capitalised,
// so a plain errors.New literal would fail staticcheck ST1005; tool.hubError is the
// precedent for a typed error that carries such a text) and unwraps to ErrAgentExists.
type agentExistsError struct{ id string }

func (e *agentExistsError) Error() string { return `Agent "` + e.id + `" already exists` }

func (*agentExistsError) Unwrap() error { return ErrAgentExists }

// Orchestration is the registry of agents. It is safe for concurrent use.
type Orchestration struct {
	factory Factory

	mu     sync.Mutex
	order  []string
	agents map[string]*agent.Agent
	// building holds the ids whose agent a Spawn is building right now: reserved, but
	// not registered yet, so Get and List do not see them.
	building map[string]struct{}
}

// New returns an empty Orchestration whose agents are built by f.
func New(f Factory) *Orchestration {
	return &Orchestration{factory: f, agents: map[string]*agent.Agent{}, building: map[string]struct{}{}}
}

// Spawn builds the agent of p through the factory and registers it under the id of the
// profile (spawn an agent, or recall it from a persistent store). A duplicate id
// returns an error whose text is `Agent "<id>" already exists` and that matches
// ErrAgentExists; the first agent stays registered. A factory error registers nothing.
//
// The id is reserved under the registry lock, the agent is built outside it, and the
// result is then committed (registered) or rolled back (the reservation released), so
// an id can never be built twice by two concurrent Spawn calls, and Get, List and the
// Spawn of other ids do not wait while an agent is being built. A Spawn of an id that
// is being built fails with ErrAgentExists at once.
func (o *Orchestration) Spawn(ctx context.Context, p profile.AgentProfile) (*agent.Agent, error) {
	if o.factory == nil {
		return nil, errors.New("orchestration: no agent factory")
	}
	o.mu.Lock()
	_, taken := o.agents[p.ID]
	_, inFlight := o.building[p.ID]
	if taken || inFlight {
		o.mu.Unlock()
		return nil, &agentExistsError{id: p.ID}
	}
	o.building[p.ID] = struct{}{}
	o.mu.Unlock()

	committed := false
	defer func() {
		if !committed {
			// Roll back: the factory failed or panicked.
			o.mu.Lock()
			delete(o.building, p.ID)
			o.mu.Unlock()
		}
	}()
	a, err := o.factory(ctx, p)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	delete(o.building, p.ID)
	o.agents[p.ID] = a
	o.order = append(o.order, p.ID)
	committed = true
	o.mu.Unlock()
	return a, nil
}

// Get returns the agent registered under id.
func (o *Orchestration) Get(id string) (*agent.Agent, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	a, ok := o.agents[id]
	return a, ok
}

// List returns the registered agents in insertion order, as a fresh slice.
func (o *Orchestration) List() []*agent.Agent {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]*agent.Agent, 0, len(o.order))
	for _, id := range o.order {
		out = append(out, o.agents[id])
	}
	return out
}

// Dispose stops the agent registered under id and removes it. An unknown id is a no-op
// that returns nil. The agent is removed even when its Dispose fails (in a deferred
// call), and that error is returned.
func (o *Orchestration) Dispose(ctx context.Context, id string) error {
	return o.disposeWith(id, func(a *agent.Agent) error { return a.Dispose(ctx) })
}

// Delete is Dispose for an agent that is being deleted (its data is removed too):
// it stops the agent with Agent.Delete, which removes its own sandbox's files too,
// and removes it. An unknown id is a no-op that returns nil, and the agent is removed
// even when its Delete fails, as with Dispose.
func (o *Orchestration) Delete(ctx context.Context, id string) error {
	return o.disposeWith(id, func(a *agent.Agent) error { return a.Delete(ctx) })
}

// disposeWith is Dispose with the stopping step passed in, so a test can make it fail:
// it runs stop on the agent registered under id and removes the entry in a deferred
// call, whatever stop returns or does.
func (o *Orchestration) disposeWith(id string, stop func(*agent.Agent) error) error {
	a, ok := o.Get(id)
	if !ok {
		return nil
	}
	defer o.remove(id, a)
	return stop(a)
}

// remove deletes the entry of id when it still holds a.
func (o *Orchestration) remove(id string, a *agent.Agent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.agents[id] != a {
		return
	}
	delete(o.agents, id)
	for i, v := range o.order {
		if v == id {
			o.order = append(o.order[:i], o.order[i+1:]...)
			break
		}
	}
}
