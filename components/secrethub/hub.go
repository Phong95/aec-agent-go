// Package secrethub is the global hub for secrets (A-101) over persistence.SecretStore -
// the SecretHub of core/ts, with its envelope encryption (envelope.go, the Sealer) and SecretHub
// providers (secret_hub_provider.go, LocalSecretHubProvider; hosts plug in KMS adapters through
// AgentCore.UseSecretHubProvider).
//
// Agents use secrets (API keys, tokens) without the model ever seeing a value. The model
// works with secret names: it writes {{secret:NAME}} in an http_request, or lists names in
// secrets for terminal / run_code, and the SDK puts the value in at the moment the tool
// runs. The contract (types, rules, the Error) is package secret; the package lives apart
// from it because it imports persistence, and persistence imports secret.
package secrethub

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// errNoStore is returned when getStore yields no store.
var errNoStore = errors.New("secrethub: the persistence secret store is not configured")

// CreateInput is what creating a secret takes: its definition, its value and who owns it.
type CreateInput struct {
	secret.Definition
	Value string
	// OwnerAgentID is the owning agent; "" = a global secret.
	OwnerAgentID string
	// CreatedBy defaults to secret.CreatorUser.
	CreatedBy secret.Creator
}

// RotationFailure is a secret a rotation could not re-seal.
type RotationFailure struct {
	OwnerAgentID string `json:"ownerAgentId,omitempty"`
	Name         string `json:"name"`
	Error        string `json:"error"`
}

// RotationResult is the outcome of a rotation: secrets re-sealed under the current master
// key, those left as they were, and those that failed.
type RotationResult struct {
	Resealed  int               `json:"resealed"`
	Unchanged int               `json:"unchanged"`
	Failed    []RotationFailure `json:"failed"`
}

// RevealContext is who a Reveal is for: the agent whose tool uses the value and the turn's
// session (both for the secret:used event).
type RevealContext struct {
	AgentID   string
	SessionID string
}

// SecretHub is the global hub for secrets (A-101) - like the SkillHub, one per AgentCore.
//   - It seals values with envelope encryption under the current SecretHubProvider
//     (UseSecretHubProvider); hosts also use Seal / Open for their own credential columns, so
//     every secret at rest goes through one place and one rotation.
//   - It keeps the owner rules: a global secret ("" owner) or an agent's own secret.
//   - Reveal hands a value to a tool after checking the use (allowed tool and host); only
//     the SDK calls it - the model never gets a value.
//
// It publishes secret:created / updated / deleted (awaited) and emits secret:used - names
// only (WithEvents).
//
// Concurrency: Create, Update, SetValue and Delete are serialised by one mutex, so a
// check-then-save cannot interleave within the process. Every value the hub returns is a
// copy. It is safe for concurrent use.
type SecretHub struct {
	getStore   func() persistence.SecretStore
	events     events.Bus // set by WithEvents; nil means no events
	logger     *slog.Logger
	now        func() int64
	sealerOpts SealerOptions
	sealer     *Sealer
	// redactor holds every value revealed in this process, so tool output that echoes one
	// (an API that returns the key, `echo $KEY`) is redacted before the model, a store or
	// the UI sees it. Process-wide: a value one agent used is redacted in every agent's
	// output.
	redactor secret.Redactor

	providerMu sync.RWMutex
	provider   SecretHubProvider

	// opMu serialises the writes (check, then save).
	opMu sync.Mutex
}

// NewSecretHub returns a hub over the store getStore returns, resolved on every call, so
// AgentCore.UsePersistence can swap the store.
func NewSecretHub(getStore func() persistence.SecretStore, opts ...Option) *SecretHub {
	h := &SecretHub{getStore: getStore, logger: slog.Default(), now: kernel.NowMillis}
	for _, o := range opts {
		if o != nil {
			o(h)
		}
	}
	so := h.sealerOpts
	if so.Logger == nil {
		so.Logger = h.logger
	}
	h.sealer = NewSealer(h.Provider, so)
	return h
}

// UseSecretHubProvider sets the SecretHub provider new values are sealed with (and stored ones opened
// with); nil removes it. The data-key cache is cleared.
func (h *SecretHub) UseSecretHubProvider(p SecretHubProvider) {
	if nilcheck.IsNil(p) {
		p = nil
	}
	h.providerMu.Lock()
	h.provider = p
	h.providerMu.Unlock()
	h.sealer.ClearCache()
}

// Provider returns the SecretHub provider in use, or nil.
func (h *SecretHub) Provider() SecretHubProvider {
	h.providerMu.RLock()
	defer h.providerMu.RUnlock()
	return h.provider
}

// Sealer returns the hub's sealer.
func (h *SecretHub) Sealer() *Sealer { return h.sealer }

// Redactor returns the process-wide redactor of every value revealed so far.
func (h *SecretHub) Redactor() *secret.Redactor { return &h.redactor }

// ── Sealing (also for hosts' own columns) ──

// Seal seals a value under the current master key; aad binds it to where it is stored.
func (h *SecretHub) Seal(ctx context.Context, plain, aad string) (string, error) {
	return h.sealer.Seal(ctx, plain, aad)
}

// Open opens a value sealed by Seal (or enc:v1, or plain text). It returns an
// *secret.Error ("decrypt_failed").
func (h *SecretHub) Open(ctx context.Context, stored, aad string) (string, error) {
	return h.sealer.Open(ctx, stored, aad)
}

// ── Secrets ──

func (h *SecretHub) store() (persistence.SecretStore, error) {
	if h.getStore == nil {
		return nil, errNoStore
	}
	s := h.getStore()
	if nilcheck.IsNil(s) {
		return nil, errNoStore
	}
	return s, nil
}

// Get returns one secret's metadata; the bool is false when there is none.
func (h *SecretHub) Get(ctx context.Context, ownerAgentID, name string) (secret.Info, bool, error) {
	st, err := h.store()
	if err != nil {
		return secret.Info{}, false, err
	}
	s, err := st.Get(ctx, ownerAgentID, name)
	if err != nil || s == nil {
		return secret.Info{}, false, err
	}
	return s.Info.Clone(), true, nil
}

// List returns the secrets of one owner ("" = global), sorted by name.
func (h *SecretHub) List(ctx context.Context, ownerAgentID string) ([]secret.Info, error) {
	st, err := h.store()
	if err != nil {
		return nil, err
	}
	all, err := st.List(ctx, ownerAgentID)
	if err != nil {
		return nil, err
	}
	return infos(all), nil
}

// ListAll returns every secret, globals first (for status and rotation views).
func (h *SecretHub) ListAll(ctx context.Context) ([]secret.Info, error) {
	st, err := h.store()
	if err != nil {
		return nil, err
	}
	all, err := st.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	return infos(all), nil
}

// Create stores a new secret. It returns an *secret.Error ("invalid_argument",
// "already_exists").
func (h *SecretHub) Create(ctx context.Context, in CreateInput) (secret.Info, error) {
	name, err := secret.CheckName(in.Name)
	if err != nil {
		return secret.Info{}, err
	}
	if err := secret.CheckValue(in.Value); err != nil {
		return secret.Info{}, err
	}
	owner := in.OwnerAgentID
	meta, err := secret.NormalizeDefinition(in.Definition, nil)
	if err != nil {
		return secret.Info{}, err
	}
	st, err := h.store()
	if err != nil {
		return secret.Info{}, err
	}
	createdBy := in.CreatedBy
	if createdBy == "" {
		createdBy = secret.CreatorUser
	}
	s, err := h.createLocked(ctx, st, in.Value, secret.Info{
		Name: name, OwnerAgentID: owner, Description: meta.Description, AllowedHosts: meta.AllowedHosts, AllowedTools: meta.AllowedTools, CreatedBy: createdBy,
	})
	if err != nil {
		return secret.Info{}, err
	}
	// Published outside the write lock, so a subscriber may call the hub.
	h.publish(ctx, events.SecretCreated, s.Info, nil)
	return s.Info.Clone(), nil
}

// createLocked checks that the secret is new and that its owner has room, then seals and
// saves it. info carries everything but the key id and the timestamps.
func (h *SecretHub) createLocked(ctx context.Context, st persistence.SecretStore, value string, info secret.Info) (secret.Stored, error) {
	owner, name := info.OwnerAgentID, info.Name
	forAgent := ""
	if owner != "" {
		forAgent = " for this agent"
	}
	h.opMu.Lock()
	defer h.opMu.Unlock()
	existing, err := st.Get(ctx, owner, name)
	if err != nil {
		return secret.Stored{}, err
	}
	if existing != nil {
		return secret.Stored{}, secret.NewError("a secret named "+name+" already exists"+forAgent, secret.CodeAlreadyExists)
	}
	current, err := st.List(ctx, owner)
	if err != nil {
		return secret.Stored{}, err
	}
	if len(current) >= secret.MaxPerOwner {
		perAgent := ""
		if owner != "" {
			perAgent = " per agent"
		}
		return secret.Stored{}, secret.NewError("at most "+strconv.Itoa(secret.MaxPerOwner)+" secrets"+perAgent, secret.CodeInvalidArgument)
	}
	now := kernel.MillisToISO(h.now())
	sealed, err := h.Seal(ctx, value, AAD(owner, name))
	if err != nil {
		return secret.Stored{}, err
	}
	info.KeyID, info.CreatedAt, info.UpdatedAt = keyIDOf(sealed), now, now
	s := secret.Stored{Info: info, Value: sealed}
	if err := st.Save(ctx, s); err != nil {
		return secret.Stored{}, err
	}
	return s, nil
}

// Update changes a secret's description, hosts or tools (its value stays; patch.Name is
// ignored). It returns an *secret.Error ("not_found", "invalid_argument").
func (h *SecretHub) Update(ctx context.Context, ownerAgentID, name string, patch secret.Definition) (secret.Info, error) {
	s, err := h.change(ctx, ownerAgentID, name, func(s *secret.Stored) error {
		base := s.Meta()
		meta, err := secret.NormalizeDefinition(patch, &base)
		if err != nil {
			return err
		}
		s.Description, s.AllowedHosts, s.AllowedTools = meta.Description, meta.AllowedHosts, meta.AllowedTools
		return nil
	})
	if err != nil {
		return secret.Info{}, err
	}
	changed := false
	h.publish(ctx, events.SecretUpdated, s.Info, &changed)
	return s.Info.Clone(), nil
}

// SetValue gives a secret a new value (the key was rotated at its issuer). It returns an
// *secret.Error ("not_found", "invalid_argument").
func (h *SecretHub) SetValue(ctx context.Context, ownerAgentID, name, value string) (secret.Info, error) {
	s, err := h.change(ctx, ownerAgentID, name, func(s *secret.Stored) error {
		if err := secret.CheckValue(value); err != nil {
			return err
		}
		sealed, err := h.Seal(ctx, value, AAD(s.OwnerAgentID, name))
		if err != nil {
			return err
		}
		s.Value, s.KeyID = sealed, keyIDOf(sealed)
		return nil
	})
	if err != nil {
		return secret.Info{}, err
	}
	changed := true
	h.publish(ctx, events.SecretUpdated, s.Info, &changed)
	return s.Info.Clone(), nil
}

// change applies edit to a copy of an existing secret under the write lock, stamps
// UpdatedAt and saves it.
func (h *SecretHub) change(ctx context.Context, ownerAgentID, name string, edit func(*secret.Stored) error) (secret.Stored, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	st, existing, err := h.require(ctx, ownerAgentID, name)
	if err != nil {
		return secret.Stored{}, err
	}
	s := existing.Clone()
	if err := edit(&s); err != nil {
		return secret.Stored{}, err
	}
	s.UpdatedAt = kernel.MillisToISO(h.now())
	if err := st.Save(ctx, s); err != nil {
		return secret.Stored{}, err
	}
	return s, nil
}

// Delete deletes a secret, then publishes secret:deleted and returns once its subscribers
// returned - so a global secret is already dropped from every agent that used it. It
// returns an *secret.Error ("not_found").
func (h *SecretHub) Delete(ctx context.Context, ownerAgentID, name string) error {
	info, err := h.deleteLocked(ctx, ownerAgentID, name)
	if err != nil {
		return err
	}
	h.publish(ctx, events.SecretDeleted, info, nil)
	return nil
}

func (h *SecretHub) deleteLocked(ctx context.Context, ownerAgentID, name string) (secret.Info, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	st, existing, err := h.require(ctx, ownerAgentID, name)
	if err != nil {
		return secret.Info{}, err
	}
	if err := st.Delete(ctx, existing.OwnerAgentID, name); err != nil {
		return secret.Info{}, err
	}
	return existing.Info, nil
}

// DeleteByOwner deletes every secret an agent owns (the agent is being deleted). An empty
// id deletes nothing.
func (h *SecretHub) DeleteByOwner(ctx context.Context, agentID string) error {
	if agentID == "" {
		return nil
	}
	st, err := h.store()
	if err != nil {
		return err
	}
	return st.DeleteByOwner(ctx, agentID)
}

// Reveal returns a secret's value for a tool - after checking that use.Tool may use it
// and (when set) that use.Host is allowed. It records the use and emits secret:used. For
// the SDK's own use (tool execution); never hand the result to a model. It returns an
// *secret.Error ("not_found", "not_allowed", "decrypt_failed").
func (h *SecretHub) Reveal(ctx context.Context, ownerAgentID, name string, use secret.Use, rc RevealContext) (string, error) {
	st, s, err := h.require(ctx, ownerAgentID, name)
	if err != nil {
		return "", err
	}
	if err := secret.CheckUse(s.Info, use); err != nil {
		return "", err
	}
	value, err := h.Open(ctx, s.Value, AAD(s.OwnerAgentID, name))
	if err != nil {
		return "", err
	}
	h.redactor.Add(name, value)
	if err := st.MarkUsed(ctx, s.OwnerAgentID, name, kernel.MillisToISO(h.now())); err != nil {
		h.logger.Warn("secrets: could not record a use", "secret", name, "error", err)
	}
	if h.events != nil {
		events.Emit(h.events, events.SecretUsed, rc.SessionID, events.SecretUsedPayload{
			Name: name, OwnerAgentID: s.OwnerAgentID, AgentID: rc.AgentID, Tool: use.Tool, Host: use.Host,
		})
	}
	return value, nil
}

// ── Keys ──

// Status counts how the stored secrets are sealed: per master key, legacy enc:v1, plain.
func (h *SecretHub) Status(ctx context.Context) (secret.Status, error) {
	status := secret.Status{Provider: "none", ByKeyID: map[string]int{}}
	if p := h.Provider(); p != nil {
		status.Provider, status.CurrentKeyID = p.ID(), p.CurrentKeyID()
	}
	st, err := h.store()
	if err != nil {
		return secret.Status{}, err
	}
	all, err := st.ListAll(ctx)
	if err != nil {
		return secret.Status{}, err
	}
	for _, s := range all {
		switch f := SealedFormatOf(s.Value); f.Format {
		case FormatV2:
			status.ByKeyID[f.KeyID]++
		case FormatV1:
			status.Legacy++
		default:
			status.Plain++
		}
	}
	return status, nil
}

// Rotate re-seals every secret that isn't sealed with the current master key (an older
// key, enc:v1 or plain text). It is safe to run while serving and to run again; a secret
// that can't be opened is reported and left as it is. Hosts re-seal their own columns
// too. Without a SecretHub provider it returns an *secret.Error ("unavailable").
func (h *SecretHub) Rotate(ctx context.Context) (RotationResult, error) {
	result := RotationResult{Failed: []RotationFailure{}}
	if h.Provider() == nil {
		return result, secret.NewError("no SecretHub provider is set "+emDash+" there is no key to rotate to", secret.CodeUnavailable)
	}
	st, err := h.store()
	if err != nil {
		return result, err
	}
	all, err := st.ListAll(ctx)
	if err != nil {
		return result, err
	}
	for _, s := range all {
		if !h.sealer.NeedsReseal(s.Value) {
			result.Unchanged++
			continue
		}
		resealed, err := h.reseal(ctx, st, s)
		switch {
		case err != nil:
			result.Failed = append(result.Failed, RotationFailure{OwnerAgentID: s.OwnerAgentID, Name: s.Name, Error: err.Error()})
		case resealed:
			result.Resealed++
		default:
			result.Unchanged++
		}
	}
	return result, nil
}

// reseal re-seals one secret under the current key; false when it changed meanwhile.
func (h *SecretHub) reseal(ctx context.Context, st persistence.SecretStore, s secret.Stored) (bool, error) {
	aad := AAD(s.OwnerAgentID, s.Name)
	openAAD := ""
	// enc:v1 and plain values were stored without the AAD.
	if SealedFormatOf(s.Value).Format == FormatV2 {
		openAAD = aad
	}
	plain, err := h.Open(ctx, s.Value, openAAD)
	if err != nil {
		return false, err
	}
	sealed, err := h.Seal(ctx, plain, aad)
	if err != nil {
		return false, err
	}
	h.opMu.Lock()
	defer h.opMu.Unlock()
	// Re-read so a value changed meanwhile is not overwritten with the old one.
	current, err := st.Get(ctx, s.OwnerAgentID, s.Name)
	if err != nil {
		return false, err
	}
	if current == nil || current.Value != s.Value {
		return false, nil
	}
	next := current.Clone()
	next.Value, next.KeyID = sealed, keyIDOf(sealed)
	if err := st.Save(ctx, next); err != nil {
		return false, err
	}
	return true, nil
}

// SelfCheck is the startup check: a seal/open round trip with the current key, and one
// open per master key the stored secrets use. It returns the problems found (empty = all
// good); without a SecretHub provider there is nothing to check.
func (h *SecretHub) SelfCheck(ctx context.Context) ([]string, error) {
	problems := []string{}
	if h.Provider() == nil {
		return problems, nil
	}
	const probe = "aec-key-check"
	sealed, err := h.Seal(ctx, probe, "check")
	if err == nil {
		var opened string
		if opened, err = h.Open(ctx, sealed, "check"); err == nil && opened != probe {
			problems = append(problems, "the SecretHub provider round trip returned a different value")
		}
	}
	if err != nil {
		problems = append(problems, err.Error())
	}
	st, err := h.store()
	if err != nil {
		return problems, err
	}
	all, err := st.ListAll(ctx)
	if err != nil {
		return problems, err
	}
	seen := map[string]bool{}
	for _, s := range all {
		f := SealedFormatOf(s.Value)
		id, aad := "", ""
		switch f.Format {
		case FormatV2:
			id, aad = f.KeyID, AAD(s.OwnerAgentID, s.Name)
		case FormatV1:
			id = "enc:v1"
		default:
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, err := h.Open(ctx, s.Value, aad); err != nil {
			problems = append(problems, "secrets sealed with "+id+" can't be opened: "+err.Error())
		}
	}
	return problems, nil
}

func (h *SecretHub) require(ctx context.Context, ownerAgentID, name string) (persistence.SecretStore, secret.Stored, error) {
	st, err := h.store()
	if err != nil {
		return nil, secret.Stored{}, err
	}
	found, err := st.Get(ctx, ownerAgentID, name)
	if err != nil {
		return nil, secret.Stored{}, err
	}
	if found == nil {
		forAgent := ""
		if ownerAgentID != "" {
			forAgent = " for this agent"
		}
		return nil, secret.Stored{}, secret.NewError("no secret named "+name+forAgent, secret.CodeNotFound)
	}
	return st, *found, nil
}

// publish publishes a secret:created / updated / deleted event with ctx (awaited).
func (h *SecretHub) publish(ctx context.Context, k events.Kind[events.SecretChangedPayload], s secret.Info, valueChanged *bool) {
	if h.events == nil {
		return
	}
	events.Publish(ctx, h.events, k, "", events.SecretChangedPayload{Name: s.Name, OwnerAgentID: s.OwnerAgentID, ValueChanged: valueChanged})
}

// AAD is the AAD a secret's value is sealed with - it binds the value to its owner and
// name: "secret:<owner|global>:<NAME>" (aadFor of core/ts, exported there as secretAad).
func AAD(owner, name string) string {
	if owner == "" {
		owner = "global"
	}
	return "secret:" + owner + ":" + name
}

// keyIDOf is the master key id of a sealed value ("" when it is not enc:v2).
func keyIDOf(sealed string) string {
	if f := SealedFormatOf(sealed); f.Format == FormatV2 {
		return f.KeyID
	}
	return ""
}

// infos returns the metadata of stored secrets, without their values.
func infos(all []secret.Stored) []secret.Info {
	out := make([]secret.Info, 0, len(all))
	for _, s := range all {
		out = append(out, s.Info.Clone())
	}
	return out
}
