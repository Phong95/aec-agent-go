// Package variablehub is the global hub for variables (A-215) over persistence.VariableStore
// - the VariableHub of core/ts. Variables are settings that are not secret (a base URL, a
// region, a project id), like GitHub Actions' vars next to its secrets: global (owner "")
// or an agent's own. The contract (types, rules, the Error) is package variable; the package
// lives apart from it because it imports persistence, and persistence imports variable.
package variablehub

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/variablehub/variable"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// errNoStore is returned when getStore yields no store.
var errNoStore = errors.New("variablehub: the persistence variable store is not configured")

// SecretValueCheck reports whether value is the value of a secret the owner can use (its
// own, or a global one) - such a value is refused, so a secret can't be copied into a
// variable the model sees. AgentCore wires the SecretHub's IsSecretValue here.
type SecretValueCheck func(ctx context.Context, value, ownerAgentID string) (bool, error)

// CreateInput is what creating a variable takes: its definition and who owns it.
type CreateInput struct {
	variable.Definition
	// OwnerAgentID is the owning agent; "" = a global variable.
	OwnerAgentID string
	// CreatedBy defaults to variable.CreatorUser.
	CreatedBy variable.Creator
}

// VariableHub is the global hub for variables (A-215) - like the SecretHub without the
// sealing, one per AgentCore. It publishes variable:created / updated / deleted (awaited,
// WithEvents); a deleted global variable is dropped from every agent's Variables.Use by
// AgentCore.
//
// Concurrency: Create, Update and Delete are serialised by one mutex, so a check-then-save
// cannot interleave within the process. It is safe for concurrent use.
type VariableHub struct {
	getStore    func() persistence.VariableStore
	events      events.Bus
	logger      *slog.Logger
	now         func() int64
	secretCheck SecretValueCheck

	opMu sync.Mutex
}

// NewVariableHub returns a hub over the store getStore returns, resolved on every call, so
// AgentCore.UsePersistence can swap the store.
func NewVariableHub(getStore func() persistence.VariableStore, opts ...Option) *VariableHub {
	h := &VariableHub{getStore: getStore, logger: slog.Default(), now: kernel.NowMillis}
	for _, o := range opts {
		if o != nil {
			o(h)
		}
	}
	return h
}

func (h *VariableHub) store() (persistence.VariableStore, error) {
	if h.getStore == nil {
		return nil, errNoStore
	}
	s := h.getStore()
	if nilcheck.IsNil(s) {
		return nil, errNoStore
	}
	return s, nil
}

// Get returns one variable; the bool is false when there is none.
func (h *VariableHub) Get(ctx context.Context, ownerAgentID, name string) (variable.Info, bool, error) {
	st, err := h.store()
	if err != nil {
		return variable.Info{}, false, err
	}
	v, err := st.Get(ctx, ownerAgentID, name)
	if err != nil || v == nil {
		return variable.Info{}, false, err
	}
	return *v, true, nil
}

// List returns the variables of one owner ("" = global), sorted by name.
func (h *VariableHub) List(ctx context.Context, ownerAgentID string) ([]variable.Info, error) {
	st, err := h.store()
	if err != nil {
		return nil, err
	}
	return st.List(ctx, ownerAgentID)
}

// ListAll returns every variable, globals first.
func (h *VariableHub) ListAll(ctx context.Context) ([]variable.Info, error) {
	st, err := h.store()
	if err != nil {
		return nil, err
	}
	return st.ListAll(ctx)
}

// Create stores a new variable. It returns an *variable.Error ("invalid_argument",
// "already_exists").
func (h *VariableHub) Create(ctx context.Context, in CreateInput) (variable.Info, error) {
	name, err := variable.CheckName(in.Name)
	if err != nil {
		return variable.Info{}, err
	}
	if err := variable.CheckValue(in.Value); err != nil {
		return variable.Info{}, err
	}
	description, err := variable.CheckDescription(in.Description)
	if err != nil {
		return variable.Info{}, err
	}
	st, err := h.store()
	if err != nil {
		return variable.Info{}, err
	}
	createdBy := in.CreatedBy
	if createdBy == "" {
		createdBy = variable.CreatorUser
	}
	v, err := h.createLocked(ctx, st, variable.Info{
		Name: name, Value: in.Value, OwnerAgentID: in.OwnerAgentID, Description: description, CreatedBy: createdBy,
	})
	if err != nil {
		return variable.Info{}, err
	}
	// Published outside the write lock, so a subscriber may call the hub.
	h.publish(ctx, events.VariableCreated, v, nil)
	return v, nil
}

func (h *VariableHub) createLocked(ctx context.Context, st persistence.VariableStore, v variable.Info) (variable.Info, error) {
	owner, name := v.OwnerAgentID, v.Name
	h.opMu.Lock()
	defer h.opMu.Unlock()
	existing, err := st.Get(ctx, owner, name)
	if err != nil {
		return variable.Info{}, err
	}
	if existing != nil {
		forAgent := ""
		if owner != "" {
			forAgent = " for this agent"
		}
		return variable.Info{}, variable.NewError("a variable named "+name+" already exists"+forAgent, variable.CodeAlreadyExists)
	}
	current, err := st.List(ctx, owner)
	if err != nil {
		return variable.Info{}, err
	}
	if len(current) >= variable.MaxPerOwner {
		perAgent := ""
		if owner != "" {
			perAgent = " per agent"
		}
		return variable.Info{}, variable.NewError("at most "+strconv.Itoa(variable.MaxPerOwner)+" variables"+perAgent, variable.CodeInvalidArgument)
	}
	if err := h.refuseSecret(ctx, v.Value, owner); err != nil {
		return variable.Info{}, err
	}
	now := kernel.MillisToISO(h.now())
	v.CreatedAt, v.UpdatedAt = now, now
	if err := st.Save(ctx, v); err != nil {
		return variable.Info{}, err
	}
	return v, nil
}

// Update changes a variable's value and / or description (a nil field keeps it; a
// Description pointing to "" removes it). It returns an *variable.Error ("not_found",
// "invalid_argument").
func (h *VariableHub) Update(ctx context.Context, ownerAgentID, name string, patch variable.Patch) (variable.Info, error) {
	v, changed, err := h.updateLocked(ctx, ownerAgentID, name, patch)
	if err != nil {
		return variable.Info{}, err
	}
	h.publish(ctx, events.VariableUpdated, v, &changed)
	return v, nil
}

func (h *VariableHub) updateLocked(ctx context.Context, ownerAgentID, name string, patch variable.Patch) (variable.Info, bool, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	st, existing, err := h.require(ctx, ownerAgentID, name)
	if err != nil {
		return variable.Info{}, false, err
	}
	v := existing
	if patch.Value != nil {
		if err := variable.CheckValue(*patch.Value); err != nil {
			return variable.Info{}, false, err
		}
		v.Value = *patch.Value
		if v.Value != existing.Value {
			if err := h.refuseSecret(ctx, v.Value, existing.OwnerAgentID); err != nil {
				return variable.Info{}, false, err
			}
		}
	}
	if patch.Description != nil {
		d, err := variable.CheckDescription(*patch.Description)
		if err != nil {
			return variable.Info{}, false, err
		}
		v.Description = d
	}
	v.UpdatedAt = kernel.MillisToISO(h.now())
	if err := st.Save(ctx, v); err != nil {
		return variable.Info{}, false, err
	}
	return v, v.Value != existing.Value, nil
}

// Delete deletes a variable, then publishes variable:deleted and returns once its
// subscribers returned - so a global variable is already dropped from every agent that
// used it. It returns an *variable.Error ("not_found").
func (h *VariableHub) Delete(ctx context.Context, ownerAgentID, name string) error {
	v, err := h.deleteLocked(ctx, ownerAgentID, name)
	if err != nil {
		return err
	}
	h.publish(ctx, events.VariableDeleted, v, nil)
	return nil
}

func (h *VariableHub) deleteLocked(ctx context.Context, ownerAgentID, name string) (variable.Info, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	st, existing, err := h.require(ctx, ownerAgentID, name)
	if err != nil {
		return variable.Info{}, err
	}
	if err := st.Delete(ctx, existing.OwnerAgentID, existing.Name); err != nil {
		return variable.Info{}, err
	}
	return existing, nil
}

// DeleteByOwner deletes every variable an agent owns (the agent is being deleted). An
// empty id deletes nothing.
func (h *VariableHub) DeleteByOwner(ctx context.Context, agentID string) error {
	if agentID == "" {
		return nil
	}
	st, err := h.store()
	if err != nil {
		return err
	}
	return st.DeleteByOwner(ctx, agentID)
}

func (h *VariableHub) require(ctx context.Context, ownerAgentID, name string) (persistence.VariableStore, variable.Info, error) {
	n, err := variable.CheckName(name)
	if err != nil {
		return nil, variable.Info{}, err
	}
	st, err := h.store()
	if err != nil {
		return nil, variable.Info{}, err
	}
	found, err := st.Get(ctx, ownerAgentID, n)
	if err != nil {
		return nil, variable.Info{}, err
	}
	if found == nil {
		forAgent := ""
		if ownerAgentID != "" {
			forAgent = " for this agent"
		}
		return nil, variable.Info{}, variable.NewError("no variable named "+n+forAgent, variable.CodeNotFound)
	}
	return st, *found, nil
}

func (h *VariableHub) refuseSecret(ctx context.Context, value, ownerAgentID string) error {
	if value == "" || h.secretCheck == nil {
		return nil
	}
	is, err := h.secretCheck(ctx, value, ownerAgentID)
	if err != nil {
		return err
	}
	if is {
		return variable.NewError("that value is a secret's value — keep it a secret and use {{secret:NAME}}, not a variable", variable.CodeInvalidArgument)
	}
	return nil
}

// publish publishes a variable:created / updated / deleted event with ctx (awaited).
func (h *VariableHub) publish(ctx context.Context, k events.Kind[events.VariableChangedPayload], v variable.Info, valueChanged *bool) {
	if h.events == nil {
		return
	}
	events.Publish(ctx, h.events, k, "", events.VariableChangedPayload{Name: v.Name, OwnerAgentID: v.OwnerAgentID, ValueChanged: valueChanged})
}
