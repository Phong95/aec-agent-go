package sandboxhub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// waitTimeout bounds every blocking wait of the hub tests, so a bug fails a test
// instead of hanging it.
const waitTimeout = 5 * time.Second

// --- fakes ------------------------------------------------------------------------

type removeCall struct {
	ID      string
	AgentID string
}

// fakeProvider mimics the in-memory sandbox provider as far as the hub can see it:
// ids, traits, the ForAgent layout, failCreates, and counters. gate holds Create
// until it is closed (or, unless ignoreCtx is set, until ctx ends); entered gets
// one value per Create call; created gets each sandbox it returns.
type fakeProvider struct {
	kind      sandboxhub.Kind
	failFirst int
	gate      chan struct{}
	ignoreCtx bool
	entered   chan struct{}
	created   chan *fakeSandbox
	returnNil bool
	panicWith any
	// disposeErr is the Dispose error of every sandbox the provider creates.
	disposeErr error

	mu        sync.Mutex
	creates   int
	sandboxes map[string]*fakeSandbox
	removed   []removeCall
	lastCC    sandboxhub.CreateContext
}

func newFakeProvider(kind sandboxhub.Kind) *fakeProvider {
	return &fakeProvider{
		kind:      kind,
		entered:   make(chan struct{}, 64),
		created:   make(chan *fakeSandbox, 64),
		sandboxes: map[string]*fakeSandbox{},
	}
}

func (p *fakeProvider) Name() string          { return "sandbox:fake" }
func (p *fakeProvider) Version() string       { return "1.0.0" }
func (p *fakeProvider) Kind() sandboxhub.Kind { return p.kind }
func (p *fakeProvider) createCount() int      { p.mu.Lock(); defer p.mu.Unlock(); return p.creates }
func (p *fakeProvider) sandboxFor(id string) *fakeSandbox {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sandboxes[id]
}

func (p *fakeProvider) Create(ctx context.Context, spec sandboxhub.Spec, cc sandboxhub.CreateContext) (sandboxhub.Sandbox, error) {
	p.mu.Lock()
	p.creates++
	n := p.creates
	p.lastCC = cc
	p.mu.Unlock()
	p.entered <- struct{}{}

	if p.panicWith != nil {
		panic(p.panicWith)
	}
	if p.gate != nil {
		if p.ignoreCtx {
			<-p.gate
		} else {
			select {
			case <-p.gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	if n <= p.failFirst {
		return nil, errors.New("simulated provider failure")
	}
	if p.returnNil {
		return nil, nil
	}
	network := spec.Network
	if network == "" {
		network = sandboxhub.NetworkNone
	}
	sb := &fakeSandbox{
		id: cc.ID,
		traits: sandboxhub.Traits{
			Kind: p.kind, Isolated: p.kind != sandboxhub.KindHost, Network: network,
			Tenancy: cc.Tenancy, OS: sandboxhub.OSLinux,
		},
		disposed:   make(chan struct{}),
		disposeErr: p.disposeErr,
	}
	p.mu.Lock()
	p.sandboxes[cc.ID] = sb
	p.mu.Unlock()
	p.created <- sb
	return sb, nil
}

// RemoveData makes fakeProvider a sandboxhub.ProviderDataRemover.
func (p *fakeProvider) RemoveData(_ context.Context, _ sandboxhub.Spec, cc sandboxhub.CreateContext, agentID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removed = append(p.removed, removeCall{ID: cc.ID, AgentID: agentID})
	return nil
}

func (p *fakeProvider) removedCalls() []removeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.removed)
}

type fakeSandbox struct {
	id     string
	traits sandboxhub.Traits

	forAgentErr error
	disposeErr  error

	mu               sync.Mutex
	disposeCalls     []bool
	removedAgentData []string
	disposed         chan struct{}
	disposeOnce      sync.Once
	// disposeDeadline records whether the ctx of the last Dispose had a deadline.
	disposeDeadline bool
}

func (s *fakeSandbox) ID() string                { return s.id }
func (s *fakeSandbox) Traits() sandboxhub.Traits { return s.traits }

func (s *fakeSandbox) ForAgent(_ context.Context, agentID string) (sandboxhub.View, error) {
	if s.forAgentErr != nil {
		return nil, s.forAgentErr
	}
	layout, err := sandboxhub.AgentLayout(s.traits.Tenancy, agentID)
	if err != nil {
		return nil, err
	}
	return &fakeView{id: s.id, traits: s.traits, layout: layout}, nil
}

func (s *fakeSandbox) Health(context.Context) sandboxhub.Health { return sandboxhub.HealthReady }

func (s *fakeSandbox) Dispose(ctx context.Context, removeData bool) error {
	s.mu.Lock()
	s.disposeCalls = append(s.disposeCalls, removeData)
	_, s.disposeDeadline = ctx.Deadline()
	s.mu.Unlock()
	s.disposeOnce.Do(func() { close(s.disposed) })
	return s.disposeErr
}

// RemoveAgentData makes fakeSandbox a sandboxhub.AgentDataRemover.
func (s *fakeSandbox) RemoveAgentData(_ context.Context, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removedAgentData = append(s.removedAgentData, agentID)
	return nil
}

func (s *fakeSandbox) disposes() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.disposeCalls)
}

func (s *fakeSandbox) removedAgents() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.removedAgentData)
}

// fakeView is the only part of a View the hub tests read; the embedded nil
// interface makes any other call fail loudly.
type fakeView struct {
	sandboxhub.View
	id     string
	traits sandboxhub.Traits
	layout sandboxhub.Layout
}

func (v *fakeView) ID() string                { return v.id }
func (v *fakeView) Traits() sandboxhub.Traits { return v.traits }
func (v *fakeView) WorkingDir() string        { return v.layout.WorkingDir }
func (v *fakeView) Roots() []string           { return slices.Clone(v.layout.Roots) }

// --- helpers ----------------------------------------------------------------------

const sharedImage = "aec-sandbox:latest"

// hubWith is a hub with a docker provider and, unless
// shared is false, a configured shared sandbox.
func hubWith(t *testing.T, shared bool, failCreates int) (*sandboxhub.SandboxHub, *fakeProvider) {
	t.Helper()
	hub := sandboxhub.NewSandboxHub()
	p := newFakeProvider(sandboxhub.KindDocker)
	p.failFirst = failCreates
	if err := hub.Register(p); err != nil {
		t.Fatal(err)
	}
	if shared {
		if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker, Image: sharedImage}); err != nil {
			t.Fatal(err)
		}
	}
	return hub, p
}

func expectCode(t *testing.T, err error, code kernel.SandboxErrorCode) {
	t.Helper()
	var se *kernel.SandboxError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v (%T), want a *kernel.SandboxError %s", err, err, code)
	}
	if se.Code != code {
		t.Fatalf("code = %s (%s), want %s", se.Code, se.Message, code)
	}
}

func ptr[T any](v T) *T { return &v }

// --- golden fixtures --------------------------------------------------------------

type approvalRow struct {
	Isolated  bool   `json:"isolated"`
	Operation string `json:"operation"`
	Approval  bool   `json:"approval"`
}

func TestApprovalPolicyMatchesGolden(t *testing.T) {
	var rows []approvalRow
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "sandbox-approval.json"), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 14 {
		t.Fatalf("the matrix has %d rows, want 14", len(rows))
	}
	// The "sandbox approval allow-list" rule: on a sandbox that is not isolated, the
	// recorded fixture lets "exec.shell", an empty and an unknown operation through;
	// the policy asks for approval for everything but fs.read and fs.search (fail
	// closed).
	goDivergent := map[string]bool{"exec.shell": true, "": true, "x": true}
	hub := sandboxhub.NewSandboxHub()
	for _, row := range rows {
		traits := sandboxhub.Traits{Isolated: row.Isolated}
		op := sandboxhub.Operation(row.Operation)
		want := row.Approval
		if !row.Isolated && goDivergent[row.Operation] {
			if want {
				t.Errorf("row (isolated=false, %q): TS asks now; drop it from goDivergent", row.Operation)
			}
			want = true
		}
		if got := sandboxhub.DefaultApprovalPolicy(traits, op); got != want {
			t.Errorf("DefaultApprovalPolicy(isolated=%v, %q) = %v, want %v", row.Isolated, row.Operation, got, want)
		}
		// The hub uses the default until the policy is replaced.
		if got := hub.RequiresApproval(traits, op); got != want {
			t.Errorf("Hub.RequiresApproval(isolated=%v, %q) = %v, want %v", row.Isolated, row.Operation, got, want)
		}
	}
}

// TestDefaultApprovalPolicyAllowList: on a host (not isolated) sandbox only the
// read-only operations skip approval; any other operation, including one the SDK
// does not know, needs it. An isolated sandbox never needs approval.
func TestDefaultApprovalPolicyAllowList(t *testing.T) {
	host := sandboxhub.Traits{Isolated: false}
	isolated := sandboxhub.Traits{Isolated: true}
	for op, want := range map[sandboxhub.Operation]bool{
		sandboxhub.OperationFSRead: false, sandboxhub.OperationFSSearch: false,
		sandboxhub.OperationFSWrite: true, sandboxhub.OperationExec: true,
		"exec.shell": true, "fs.delete": true, "net.fetch": true, "": true,
	} {
		if got := sandboxhub.DefaultApprovalPolicy(host, op); got != want {
			t.Errorf("host %q: approval %v, want %v", op, got, want)
		}
		if sandboxhub.DefaultApprovalPolicy(isolated, op) {
			t.Errorf("isolated %q must not need approval", op)
		}
	}
}

type viewSummary struct {
	ID         string   `json:"id"`
	WorkingDir string   `json:"workingDir"`
	Roots      []string `json:"roots"`
	Tenancy    string   `json:"tenancy"`
}

func (v viewSummary) String() string {
	b, _ := jsonx.Marshal(v)
	return string(b)
}

type selectionForm struct {
	Form string                `json:"form"`
	Own  *sandboxhub.OwnConfig `json:"own"`
}

func (f selectionForm) selection() sandboxhub.Selection {
	switch f.Form {
	case "omitted":
		return sandboxhub.Selection{}
	case "none":
		return sandboxhub.Selection{Mode: sandboxhub.SelectionNone}
	case "shared":
		return sandboxhub.Selection{Mode: sandboxhub.SelectionShared}
	case "bogus":
		return sandboxhub.Selection{Mode: "bogus"}
	case "ownNil":
		return sandboxhub.Selection{Mode: sandboxhub.SelectionOwn}
	case "own":
		return sandboxhub.Selection{Mode: sandboxhub.SelectionOwn, Own: f.Own}
	}
	panic("unknown selection form " + f.Form) // test code: a malformed fixture
}

type hubFixture struct {
	SharedImage   string                  `json:"sharedImage"`
	Widened       sandboxhub.PolicyUpdate `json:"widened"`
	DefaultPolicy json.RawMessage         `json:"defaultPolicy"`
	BuildOwnSpec  []struct {
		Policy  string                   `json:"policy"`
		Shared  bool                     `json:"shared"`
		Own     sandboxhub.OwnConfig     `json:"own"`
		Outcome outcome[json.RawMessage] `json:"outcome"`
	} `json:"buildOwnSpec"`
	Selections []struct {
		Setup     string               `json:"setup"`
		Policy    string               `json:"policy"`
		AgentID   string               `json:"agentId"`
		Selection selectionForm        `json:"selection"`
		Validate  outcome[any]         `json:"validate"`
		Resolve   outcome[viewSummary] `json:"resolve"`
	} `json:"selections"`
	Scenarios struct {
		LazySingle struct {
			Before int         `json:"before"`
			After  int         `json:"after"`
			A      viewSummary `json:"a"`
			B      viewSummary `json:"b"`
		} `json:"lazySingle"`
		FailedCreationNotCached struct {
			First       outcome[viewSummary] `json:"first"`
			Second      outcome[viewSummary] `json:"second"`
			CreateCount int                  `json:"createCount"`
		} `json:"failedCreationNotCached"`
		RegisterDuplicate                  outcome[any] `json:"registerDuplicate"`
		ConfigureSharedAfterUse            outcome[any] `json:"configureSharedAfterUse"`
		ConfigureSharedAfterFailedCreation outcome[any] `json:"configureSharedAfterFailedCreation"`
		ConfigureSharedTwiceBeforeUse      struct {
			SharedConfigured bool   `json:"sharedConfigured"`
			OwnImage         string `json:"ownImage"`
		} `json:"configureSharedTwiceBeforeUse"`
		SharedConfigured struct {
			Configured   bool `json:"configured"`
			Unconfigured bool `json:"unconfigured"`
		} `json:"sharedConfigured"`
		Dispose struct {
			Disposed    bool                 `json:"disposed"`
			Reconfigure outcome[any]         `json:"reconfigure"`
			Again       outcome[viewSummary] `json:"again"`
			CreateCount int                  `json:"createCount"`
		} `json:"dispose"`
	} `json:"scenarios"`
	AllowedOwnKinds []struct {
		Update sandboxhub.PolicyUpdate `json:"update"`
		Result []sandboxhub.Kind       `json:"result"`
	} `json:"allowedOwnKinds"`
	Policy []struct {
		Update sandboxhub.PolicyUpdate `json:"update"`
		Result json.RawMessage         `json:"result"`
	} `json:"policy"`
}

func loadHubFixture(t *testing.T) hubFixture {
	t.Helper()
	var f hubFixture
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "sandbox-hub.json"), &f); err != nil {
		t.Fatalf("decode sandbox-hub.json: %v", err)
	}
	return f
}

// hubFor mirrors makeHub of the wiregen case: setup is shared (docker provider and
// a configured shared sandbox), unconfigured (provider only), noprovider (a
// configured shared sandbox, no provider) or remote (a shared sandbox of kind
// remote, only a docker provider).
func hubFor(t *testing.T, f hubFixture, setup, policy string) *sandboxhub.SandboxHub {
	t.Helper()
	hub := sandboxhub.NewSandboxHub()
	if setup != "noprovider" {
		if err := hub.Register(newFakeProvider(sandboxhub.KindDocker)); err != nil {
			t.Fatal(err)
		}
	}
	var spec *sandboxhub.Spec
	switch setup {
	case "shared", "noprovider":
		spec = &sandboxhub.Spec{Kind: sandboxhub.KindDocker, Image: f.SharedImage}
	case "remote":
		spec = &sandboxhub.Spec{Kind: sandboxhub.KindRemote}
	}
	if spec != nil {
		if err := hub.ConfigureShared(*spec); err != nil {
			t.Fatal(err)
		}
	}
	if policy == "widened" {
		hub.SetPolicy(f.Widened)
	}
	return hub
}

func assertView(t *testing.T, name string, v sandboxhub.View, want viewSummary) {
	t.Helper()
	got := viewSummary{ID: v.ID(), WorkingDir: v.WorkingDir(), Roots: v.Roots(), Tenancy: string(v.Traits().Tenancy)}
	if got.ID != want.ID || got.WorkingDir != want.WorkingDir || !slices.Equal(got.Roots, want.Roots) || got.Tenancy != want.Tenancy {
		t.Errorf("%s: view = %v, want %v", name, got, want)
	}
}

func assertResolveOutcome(t *testing.T, name string, v sandboxhub.View, err error, want outcome[viewSummary]) {
	t.Helper()
	assertErrorMatches(t, name, err, want.Err)
	if want.Err != nil {
		return
	}
	if want.OK == nil {
		t.Fatalf("%s: the fixture row has neither ok nor err", name)
	}
	if err == nil {
		assertView(t, name, v, *want.OK)
	}
}

func TestHubMatchesGolden(t *testing.T) {
	f := loadHubFixture(t)
	ctx := context.Background()

	t.Run("defaultPolicy", func(t *testing.T) {
		got, err := jsonx.Marshal(sandboxhub.DefaultHubPolicy())
		if err != nil || string(got) != string(f.DefaultPolicy) {
			t.Errorf("DefaultHubPolicy = %s (%v), want %s", got, err, f.DefaultPolicy)
		}
		got, err = jsonx.Marshal(sandboxhub.NewSandboxHub().Policy())
		if err != nil || string(got) != string(f.DefaultPolicy) {
			t.Errorf("a new hub's Policy = %s (%v), want %s", got, err, f.DefaultPolicy)
		}
	})

	t.Run("buildOwnSpec", func(t *testing.T) {
		for _, row := range f.BuildOwnSpec {
			hub := sandboxhub.NewSandboxHub()
			if row.Shared {
				if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker, Image: f.SharedImage}); err != nil {
					t.Fatal(err)
				}
			}
			if row.Policy == "widened" {
				hub.SetPolicy(f.Widened)
			}
			own, _ := jsonx.Marshal(row.Own)
			name := row.Policy + " shared=" + map[bool]string{true: "yes", false: "no"}[row.Shared] + " " + string(own)
			spec, err := hub.BuildOwnSpec(row.Own)
			assertErrorMatches(t, name, err, row.Outcome.Err)
			if row.Outcome.Err != nil {
				continue
			}
			got, mErr := jsonx.Marshal(spec)
			if mErr != nil || string(got) != string(*row.Outcome.OK) {
				t.Errorf("%s:\n got %s\nwant %s", name, got, *row.Outcome.OK)
			}
		}
	})

	t.Run("selections", func(t *testing.T) {
		for _, row := range f.Selections {
			own, _ := jsonx.Marshal(row.Selection)
			name := row.Setup + "/" + row.Policy + " agent " + jsonQuote(t, row.AgentID) + " " + string(own)
			sel := row.Selection.selection()

			vErr := hubFor(t, f, row.Setup, row.Policy).ValidateSelection(row.AgentID, sel)
			assertErrorMatches(t, name+" validate", vErr, row.Validate.Err)

			view, rErr := hubFor(t, f, row.Setup, row.Policy).Resolve(ctx, row.AgentID, sel)
			assertResolveOutcome(t, name+" resolve", view, rErr, row.Resolve)
		}
	})

	t.Run("lazySingle", func(t *testing.T) {
		want := f.Scenarios.LazySingle
		hub, p := hubWith(t, true, 0)
		if got := p.createCount(); got != want.Before {
			t.Errorf("creates before first use = %d, want %d", got, want.Before)
		}
		a, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{})
		if err != nil {
			t.Fatal(err)
		}
		b, err := hub.Resolve(ctx, "agent-b", sandboxhub.Selection{Mode: sandboxhub.SelectionShared})
		if err != nil {
			t.Fatal(err)
		}
		if got := p.createCount(); got != want.After {
			t.Errorf("creates after two agents = %d, want %d", got, want.After)
		}
		assertView(t, "agent-a", a, want.A)
		assertView(t, "agent-b", b, want.B)
	})

	t.Run("failedCreationNotCached", func(t *testing.T) {
		want := f.Scenarios.FailedCreationNotCached
		hub, p := hubWith(t, true, 1)
		v1, err1 := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{})
		assertResolveOutcome(t, "first", v1, err1, want.First)
		v2, err2 := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{})
		assertResolveOutcome(t, "second", v2, err2, want.Second)
		if got := p.createCount(); got != want.CreateCount {
			t.Errorf("creates = %d, want %d", got, want.CreateCount)
		}
	})

	t.Run("registerDuplicate", func(t *testing.T) {
		hub, _ := hubWith(t, true, 0)
		err := hub.Register(newFakeProvider(sandboxhub.KindDocker))
		assertErrorMatches(t, "register twice", err, f.Scenarios.RegisterDuplicate.Err)
		if !errors.Is(err, sandboxhub.ErrProviderAlreadyRegistered) {
			t.Errorf("error %v does not match ErrProviderAlreadyRegistered", err)
		}
	})

	t.Run("configureSharedAfterUse", func(t *testing.T) {
		hub, _ := hubWith(t, true, 0)
		if _, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{}); err != nil {
			t.Fatal(err)
		}
		err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker, Image: "other"})
		assertErrorMatches(t, "configure after use", err, f.Scenarios.ConfigureSharedAfterUse.Err)
		if !errors.Is(err, sandboxhub.ErrSharedRunning) {
			t.Errorf("error %v does not match ErrSharedRunning", err)
		}
	})

	t.Run("configureSharedAfterFailedCreation", func(t *testing.T) {
		hub, _ := hubWith(t, true, 1)
		if _, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{}); err == nil {
			t.Fatal("the first creation must fail")
		}
		err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker, Image: "other"})
		assertErrorMatches(t, "configure after a failed creation", err, f.Scenarios.ConfigureSharedAfterFailedCreation.Err)
	})

	t.Run("configureSharedTwiceBeforeUse", func(t *testing.T) {
		want := f.Scenarios.ConfigureSharedTwiceBeforeUse
		hub, _ := hubWith(t, true, 0)
		if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker, Image: "second"}); err != nil {
			t.Fatal(err)
		}
		spec, err := hub.BuildOwnSpec(sandboxhub.OwnConfig{})
		if err != nil {
			t.Fatal(err)
		}
		if hub.SharedConfigured() != want.SharedConfigured || spec.Image != want.OwnImage {
			t.Errorf("configured=%v ownImage=%q, want %v %q", hub.SharedConfigured(), spec.Image, want.SharedConfigured, want.OwnImage)
		}
	})

	t.Run("sharedConfigured", func(t *testing.T) {
		want := f.Scenarios.SharedConfigured
		hub, _ := hubWith(t, true, 0)
		bare, _ := hubWith(t, false, 0)
		if hub.SharedConfigured() != want.Configured || bare.SharedConfigured() != want.Unconfigured {
			t.Errorf("SharedConfigured = %v / %v, want %v / %v", hub.SharedConfigured(), bare.SharedConfigured(), want.Configured, want.Unconfigured)
		}
	})

	t.Run("dispose", func(t *testing.T) {
		want := f.Scenarios.Dispose
		hub, p := hubWith(t, true, 0)
		if _, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{}); err != nil {
			t.Fatal(err)
		}
		shared := p.sandboxFor("docker:shared")
		if err := hub.Dispose(ctx); err != nil {
			t.Fatal(err)
		}
		if got := len(shared.disposes()) > 0; got != want.Disposed {
			t.Errorf("shared sandbox disposed = %v, want %v", got, want.Disposed)
		}
		reErr := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker, Image: "after"})
		assertErrorMatches(t, "reconfigure after dispose", reErr, want.Reconfigure.Err)
		view, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{})
		assertResolveOutcome(t, "resolve after dispose", view, err, want.Again)
		if got := p.createCount(); got != want.CreateCount {
			t.Errorf("creates = %d, want %d", got, want.CreateCount)
		}
	})

	t.Run("allowedOwnKinds", func(t *testing.T) {
		for _, row := range f.AllowedOwnKinds {
			hub := sandboxhub.NewSandboxHub()
			hub.SetPolicy(row.Update)
			if got := hub.AllowedOwnKinds(); !slices.Equal(got, row.Result) {
				u, _ := jsonx.Marshal(row.Update)
				t.Errorf("AllowedOwnKinds after %s = %v, want %v", u, got, row.Result)
			}
		}
	})

	t.Run("policy", func(t *testing.T) {
		for _, row := range f.Policy {
			hub := sandboxhub.NewSandboxHub()
			hub.SetPolicy(row.Update)
			got, err := jsonx.Marshal(hub.Policy())
			if err != nil || string(got) != string(row.Result) {
				u, _ := jsonx.Marshal(row.Update)
				t.Errorf("Policy after %s = %s (%v), want %s", u, got, err, row.Result)
			}
		}
	})
}

// --- the hub behaviour cases ------------------------------------------------------

func TestSandboxHubDefaultsToOneSharedSandboxCreatedLazilyAndReusedAcrossAgents(t *testing.T) {
	hub, p := hubWith(t, true, 0)
	if p.createCount() != 0 {
		t.Fatalf("creates = %d before first use", p.createCount())
	}
	ctx := context.Background()
	a, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := hub.Resolve(ctx, "agent-b", sandboxhub.Selection{Mode: sandboxhub.SelectionShared})
	if err != nil {
		t.Fatal(err)
	}
	if p.createCount() != 1 {
		t.Errorf("creates = %d, want 1", p.createCount())
	}
	if a.ID() != "docker:shared" || b.ID() != "docker:shared" {
		t.Errorf("ids = %q, %q", a.ID(), b.ID())
	}
	if a.Traits().Tenancy != sandboxhub.TenancyShared {
		t.Errorf("tenancy = %q", a.Traits().Tenancy)
	}
	if a.WorkingDir() != "/workspace/agents/agent-a" || b.WorkingDir() != "/workspace/agents/agent-b" {
		t.Errorf("working dirs = %q, %q", a.WorkingDir(), b.WorkingDir())
	}
}

func TestSandboxHubFailsClosedWhenNoSharedSandboxIsConfigured(t *testing.T) {
	hub, _ := hubWith(t, false, 0)
	_, err := hub.Resolve(context.Background(), "agent-a", sandboxhub.Selection{})
	expectCode(t, err, kernel.SandboxCodeNotConfigured)
	expectCode(t, hub.ValidateSelection("agent-a", sandboxhub.Selection{Mode: sandboxhub.SelectionShared}), kernel.SandboxCodeNotConfigured)
}

func TestSandboxHubNoneDisablesTheSandboxButIsAValidSelectionToSave(t *testing.T) {
	hub, _ := hubWith(t, true, 0)
	_, err := hub.Resolve(context.Background(), "agent-a", sandboxhub.Selection{Mode: sandboxhub.SelectionNone})
	expectCode(t, err, kernel.SandboxCodeDisabled)
	if err := hub.ValidateSelection("agent-a", sandboxhub.Selection{Mode: sandboxhub.SelectionNone}); err != nil {
		t.Errorf("ValidateSelection(none) = %v", err)
	}
}

func TestSandboxHubReportsAMissingProvider(t *testing.T) {
	hub := sandboxhub.NewSandboxHub()
	if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Fatal(err)
	}
	_, err := hub.Resolve(context.Background(), "agent-a", sandboxhub.Selection{})
	expectCode(t, err, kernel.SandboxCodeProviderUnavailable)
	expectCode(t, hub.ValidateSelection("agent-a", sandboxhub.Selection{}), kernel.SandboxCodeProviderUnavailable)
}

func TestSandboxHubDoesNotCacheAFailedCreationTheNextCallRetries(t *testing.T) {
	hub, p := hubWith(t, true, 1)
	ctx := context.Background()
	_, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{})
	expectCode(t, err, kernel.SandboxCodeEnvironmentUnavailable)
	view, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID() != "docker:shared" || p.createCount() != 2 {
		t.Errorf("id = %q, creates = %d, want docker:shared and 2", view.ID(), p.createCount())
	}
}

func TestSandboxHubRejectsAgentIdsThatAreUnsafeAsFolderNames(t *testing.T) {
	hub, _ := hubWith(t, true, 0)
	long := strings.Repeat("x", 200)
	for _, id := range []string{"../evil", "a/b", "", ".hidden", long} {
		_, err := hub.Resolve(context.Background(), id, sandboxhub.Selection{})
		expectCode(t, err, kernel.SandboxCodePolicyDenied)
		expectCode(t, hub.ValidateSelection(id, sandboxhub.Selection{}), kernel.SandboxCodePolicyDenied)
		expectCode(t, hub.ReleaseAgentData(context.Background(), id), kernel.SandboxCodePolicyDenied)
	}
}

func TestSandboxHubRefusesToReconfigureTheSharedSandboxOnceItIsRunning(t *testing.T) {
	hub, _ := hubWith(t, true, 0)
	if _, err := hub.Resolve(context.Background(), "agent-a", sandboxhub.Selection{}); err != nil {
		t.Fatal(err)
	}
	err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker, Image: "other"})
	if !errors.Is(err, sandboxhub.ErrSharedRunning) {
		t.Fatalf("ConfigureShared error = %v, want ErrSharedRunning", err)
	}
}

func TestSandboxHubRejectsOwnSelectionsThoseGoThroughSandboxManager(t *testing.T) {
	hub, _ := hubWith(t, true, 0)
	_, err := hub.Resolve(context.Background(), "agent-a", sandboxhub.Selection{Mode: sandboxhub.SelectionOwn, Own: &sandboxhub.OwnConfig{}})
	if !errors.Is(err, sandboxhub.ErrOwnSelection) {
		t.Fatalf("Resolve error = %v, want ErrOwnSelection", err)
	}
	if want := "Own sandboxes are managed by the agent's SandboxManager, not SandboxHub.resolve()"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
}

func TestOwnSandboxPolicyInheritsTheSharedImageAndAppliesThePolicyCeiling(t *testing.T) {
	hub, _ := hubWith(t, true, 0)
	spec, err := hub.BuildOwnSpec(sandboxhub.OwnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := jsonx.Marshal(spec)
	want := `{"kind":"docker","image":"aec-sandbox:latest","network":"none","limits":{"cpus":2,"memoryMb":2048,"pids":256}}`
	if string(got) != want {
		t.Errorf("spec = %s, want %s", got, want)
	}
}

func TestOwnSandboxPolicyEnforcesTheImageAllowlistAndNetworkPolicy(t *testing.T) {
	hub, _ := hubWith(t, true, 0)
	for _, own := range []sandboxhub.OwnConfig{{Image: "evil/miner"}, {Network: sandboxhub.NetworkAllowlist}, {Network: sandboxhub.NetworkFull}} {
		_, err := hub.BuildOwnSpec(own)
		expectCode(t, err, kernel.SandboxCodePolicyDenied)
	}

	hub.SetPolicy(sandboxhub.PolicyUpdate{
		AllowedImages:  ptr([]string{"python:3.12-slim"}),
		AllowedNetwork: ptr([]sandboxhub.NetworkMode{sandboxhub.NetworkNone, sandboxhub.NetworkAllowlist}),
	})
	spec, err := hub.BuildOwnSpec(sandboxhub.OwnConfig{Image: "python:3.12-slim", Network: sandboxhub.NetworkAllowlist})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Network != sandboxhub.NetworkAllowlist {
		t.Errorf("network = %q", spec.Network)
	}
	_, err = hub.BuildOwnSpec(sandboxhub.OwnConfig{Network: sandboxhub.NetworkFull})
	expectCode(t, err, kernel.SandboxCodePolicyDenied)
}

func TestOwnSandboxPolicyAcceptsFullNetworkForAnOwnSandboxOnlyWhenTheHostOptedIn(t *testing.T) {
	hub, _ := hubWith(t, true, 0)
	hub.SetPolicy(sandboxhub.PolicyUpdate{AllowedNetwork: ptr([]sandboxhub.NetworkMode{sandboxhub.NetworkNone, sandboxhub.NetworkFull})})
	full, err := hub.BuildOwnSpec(sandboxhub.OwnConfig{Network: sandboxhub.NetworkFull})
	if err != nil || full.Network != sandboxhub.NetworkFull {
		t.Errorf("full: %+v, %v", full, err)
	}
	none, err := hub.BuildOwnSpec(sandboxhub.OwnConfig{})
	if err != nil || none.Network != sandboxhub.NetworkNone {
		t.Errorf("default: %+v, %v", none, err)
	}
}

func TestOwnSandboxPolicyClampsRequestedResourcesToThePolicyCeilingAndIgnoresInvalidValues(t *testing.T) {
	hub, _ := hubWith(t, true, 0)
	cpus, mem, pids := 64.0, -5, 100
	spec, err := hub.BuildOwnSpec(sandboxhub.OwnConfig{Limits: &sandboxhub.LimitsOverride{Cpus: &cpus, MemoryMb: &mem, Pids: &pids}})
	if err != nil {
		t.Fatal(err)
	}
	l := spec.Limits
	if l == nil || l.Cpus == nil || l.MemoryMb == nil || l.Pids == nil || *l.Cpus != 2 || *l.MemoryMb != 2048 || *l.Pids != 100 {
		got, _ := jsonx.Marshal(spec)
		t.Errorf("limits = %s, want cpus 2, memoryMb 2048, pids 100", got)
	}
}

func TestOwnSandboxKindDefaultsToDockerAndRefusesKindsThePolicyDoesNotAllow(t *testing.T) {
	hub, _ := hubWith(t, true, 0)
	if err := hub.Register(newFakeProvider(sandboxhub.KindHost)); err != nil {
		t.Fatal(err)
	}
	spec, err := hub.BuildOwnSpec(sandboxhub.OwnConfig{})
	if err != nil || spec.Kind != sandboxhub.KindDocker {
		t.Errorf("default kind = %q, %v", spec.Kind, err)
	}
	_, err = hub.BuildOwnSpec(sandboxhub.OwnConfig{Kind: sandboxhub.KindHost})
	expectCode(t, err, kernel.SandboxCodePolicyDenied)
	hostOwn := sandboxhub.Selection{Mode: sandboxhub.SelectionOwn, Own: &sandboxhub.OwnConfig{Kind: sandboxhub.KindHost}}
	expectCode(t, hub.ValidateSelection("agent-a", hostOwn), kernel.SandboxCodePolicyDenied)
}

func TestOwnSandboxKindBuildsABareHostSpecOnceHostIsAllowed(t *testing.T) {
	hub, _ := hubWith(t, true, 0)
	if err := hub.Register(newFakeProvider(sandboxhub.KindHost)); err != nil {
		t.Fatal(err)
	}
	hub.SetPolicy(sandboxhub.PolicyUpdate{OwnKinds: ptr([]sandboxhub.Kind{sandboxhub.KindDocker, sandboxhub.KindHost})})
	if got := hub.AllowedOwnKinds(); !slices.Equal(got, []sandboxhub.Kind{sandboxhub.KindDocker, sandboxhub.KindHost}) {
		t.Errorf("AllowedOwnKinds = %v", got)
	}
	cpus := 9.0
	spec, err := hub.BuildOwnSpec(sandboxhub.OwnConfig{Kind: sandboxhub.KindHost, Image: "anything", Limits: &sandboxhub.LimitsOverride{Cpus: &cpus}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := jsonx.Marshal(spec); string(got) != `{"kind":"host"}` {
		t.Errorf("host spec = %s, want {\"kind\":\"host\"}", got)
	}
	spec, err = hub.BuildOwnSpec(sandboxhub.OwnConfig{})
	if err != nil || spec.Kind != sandboxhub.KindDocker {
		t.Errorf("default kind = %q, %v", spec.Kind, err)
	}
	hostOwn := sandboxhub.Selection{Mode: sandboxhub.SelectionOwn, Own: &sandboxhub.OwnConfig{Kind: sandboxhub.KindHost}}
	if err := hub.ValidateSelection("agent-a", hostOwn); err != nil {
		t.Errorf("ValidateSelection(own host) = %v", err)
	}
}

func TestSandboxHubSelectionValidationRejectsMalformedSelections(t *testing.T) {
	hub, _ := hubWith(t, true, 0)
	_, err := hub.Resolve(context.Background(), "agent-a", sandboxhub.Selection{Mode: "bogus"})
	expectCode(t, err, kernel.SandboxCodePolicyDenied)
}

func TestDefaultSandboxApprovalPolicyAutoApprovesEverythingInIsolatedSandboxes(t *testing.T) {
	traits := sandboxhub.Traits{Kind: sandboxhub.KindDocker, Isolated: true, Network: sandboxhub.NetworkNone, Persistent: true, Tenancy: sandboxhub.TenancyShared, OS: sandboxhub.OSLinux}
	for _, op := range []sandboxhub.Operation{sandboxhub.OperationFSRead, sandboxhub.OperationFSSearch, sandboxhub.OperationFSWrite, sandboxhub.OperationExec} {
		if sandboxhub.DefaultApprovalPolicy(traits, op) {
			t.Errorf("%s needs approval in an isolated sandbox", op)
		}
	}
}

func TestDefaultSandboxApprovalPolicyRequiresApprovalForWritesAndExecOnTheHost(t *testing.T) {
	traits := sandboxhub.Traits{Kind: sandboxhub.KindHost, Isolated: false, Network: sandboxhub.NetworkNone, Persistent: true, Tenancy: sandboxhub.TenancyShared, OS: sandboxhub.OSLinux}
	cases := map[sandboxhub.Operation]bool{
		sandboxhub.OperationFSRead:   false,
		sandboxhub.OperationFSSearch: false,
		sandboxhub.OperationFSWrite:  true,
		sandboxhub.OperationExec:     true,
	}
	for op, want := range cases {
		if got := sandboxhub.DefaultApprovalPolicy(traits, op); got != want {
			t.Errorf("%s: approval = %v, want %v", op, got, want)
		}
	}
}

// --- further behaviour -----------------------------------------------------------

// TestHubConcurrentResolveCreatesOnce: 16 goroutines resolving different agents
// create the shared sandbox exactly once (run with -race).
func TestHubConcurrentResolveCreatesOnce(t *testing.T) {
	hub, p := hubWith(t, true, 0)
	p.gate = make(chan struct{})

	const agents = 16
	var wg sync.WaitGroup
	views := make([]sandboxhub.View, agents)
	errs := make([]error, agents)
	for i := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			views[i], errs[i] = hub.Resolve(context.Background(), "agent-"+string(rune('a'+i)), sandboxhub.Selection{})
		}()
	}
	select {
	case <-p.entered:
	case <-time.After(waitTimeout):
		t.Fatal("no creation started")
	}
	close(p.gate)
	wg.Wait()

	for i := range agents {
		if errs[i] != nil {
			t.Fatalf("agent %d: %v", i, errs[i])
		}
		want := "/workspace/agents/agent-" + string(rune('a'+i))
		if views[i].WorkingDir() != want {
			t.Errorf("agent %d working dir = %q, want %q", i, views[i].WorkingDir(), want)
		}
	}
	if got := p.createCount(); got != 1 {
		t.Errorf("creates = %d, want 1", got)
	}
	p.mu.Lock()
	cc := p.lastCC
	p.mu.Unlock()
	if cc.ID != "docker:shared" || cc.Tenancy != sandboxhub.TenancyShared {
		t.Errorf("create context = %+v", cc)
	}
}

// TestHubCancelledWaiterDoesNotPoisonCreation: a caller whose ctx ends while the
// creation is in flight gets ctx.Err(), and the creation still completes for the
// other waiters.
func TestHubCancelledWaiterDoesNotPoisonCreation(t *testing.T) {
	hub, p := hubWith(t, true, 0)
	p.gate = make(chan struct{})

	ctx1, cancel1 := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := hub.Resolve(ctx1, "agent-a", sandboxhub.Selection{})
		first <- err
	}()
	select {
	case <-p.entered:
	case <-time.After(waitTimeout):
		t.Fatal("no creation started")
	}

	second := make(chan error, 1)
	var view sandboxhub.View
	go func() {
		var err error
		view, err = hub.Resolve(context.Background(), "agent-b", sandboxhub.Selection{})
		second <- err
	}()

	cancel1()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller error = %v, want context.Canceled", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("the cancelled caller did not return")
	}

	close(p.gate)
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("second caller: %v", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("the second caller did not return")
	}
	if view.ID() != "docker:shared" || p.createCount() != 1 {
		t.Errorf("id = %q, creates = %d", view.ID(), p.createCount())
	}
}

// TestHubResolveWithCancelledContextStartsNoCreation: a ctx that has ended
// before the first call never starts a creation.
func TestHubResolveWithCancelledContextStartsNoCreation(t *testing.T) {
	hub, p := hubWith(t, true, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if p.createCount() != 0 {
		t.Errorf("creates = %d, want 0", p.createCount())
	}
}

// TestHubDisposeDuringCreation: Dispose cancels a creation in flight; the
// waiting caller gets the failure and a later Resolve starts afresh.
func TestHubDisposeDuringCreation(t *testing.T) {
	hub, p := hubWith(t, true, 0)
	p.gate = make(chan struct{})

	result := make(chan error, 1)
	go func() {
		_, err := hub.Resolve(context.Background(), "agent-a", sandboxhub.Selection{})
		result <- err
	}()
	select {
	case <-p.entered:
	case <-time.After(waitTimeout):
		t.Fatal("no creation started")
	}

	if err := hub.Dispose(context.Background()); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	select {
	case err := <-result:
		expectCode(t, err, kernel.SandboxCodeEnvironmentUnavailable)
	case <-time.After(waitTimeout):
		t.Fatal("the waiting caller did not return")
	}

	// The cache is empty: configuring and resolving start afresh.
	p.gate = nil
	if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Resolve(context.Background(), "agent-a", sandboxhub.Selection{}); err != nil {
		t.Fatal(err)
	}
}

// TestHubDisposeWaitsForACreationThatIgnoresItsContext: Dispose waits for the
// creation and disposes the sandbox with removeData false.
func TestHubDisposeWaitsForACreationThatIgnoresItsContext(t *testing.T) {
	hub, p := hubWith(t, true, 0)
	p.gate = make(chan struct{})
	p.ignoreCtx = true

	resolved := make(chan error, 1)
	go func() {
		_, err := hub.Resolve(context.Background(), "agent-a", sandboxhub.Selection{})
		resolved <- err
	}()
	<-p.entered

	disposed := make(chan error, 1)
	go func() { disposed <- hub.Dispose(context.Background()) }()
	select {
	case err := <-disposed:
		t.Fatalf("Dispose returned %v before the creation finished", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(p.gate)
	if err := <-disposed; err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if err := <-resolved; err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	sb := p.sandboxFor("docker:shared")
	if got := sb.disposes(); !slices.Equal(got, []bool{false}) {
		t.Errorf("dispose calls = %v, want [false]", got)
	}
}

// TestHubDisposeGivingUpDisposesALateSandbox: when the ctx of Dispose ends while
// a creation is in flight, the goroutine that finishes the creation disposes the
// sandbox, so none is leaked.
func TestHubDisposeGivingUpDisposesALateSandbox(t *testing.T) {
	hub, p := hubWith(t, true, 0)
	p.gate = make(chan struct{})
	p.ignoreCtx = true

	resolved := make(chan error, 1)
	go func() {
		_, err := hub.Resolve(context.Background(), "agent-a", sandboxhub.Selection{})
		resolved <- err
	}()
	<-p.entered

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if err := hub.Dispose(gone); !errors.Is(err, context.Canceled) {
		t.Fatalf("Dispose error = %v, want context.Canceled", err)
	}

	close(p.gate)
	var sb *fakeSandbox
	select {
	case sb = <-p.created:
	case <-time.After(waitTimeout):
		t.Fatal("the creation never finished")
	}
	select {
	case <-sb.disposed:
	case <-time.After(waitTimeout):
		t.Fatal("the late sandbox was not disposed")
	}
	if got := sb.disposes(); !slices.Equal(got, []bool{false}) {
		t.Errorf("dispose calls = %v, want [false]", got)
	}
	sb.mu.Lock()
	bounded := sb.disposeDeadline
	sb.mu.Unlock()
	if !bounded {
		t.Error("the orphan dispose must run on a context with a deadline")
	}
	<-resolved
}

// TestHubLogsAFailedOrphanDispose: the dispose of a sandbox whose creation finished
// after Dispose gave up has no caller, so its error is logged at Warn.
func TestHubLogsAFailedOrphanDispose(t *testing.T) {
	hub, p := hubWith(t, true, 0)
	p.gate = make(chan struct{})
	p.ignoreCtx = true
	p.disposeErr = errors.New("orphan stop failed")
	var logs syncBuffer
	hub.SetLogger(slog.New(slog.NewTextHandler(&logs, nil)))

	resolved := make(chan error, 1)
	go func() {
		_, err := hub.Resolve(context.Background(), "agent-a", sandboxhub.Selection{})
		resolved <- err
	}()
	<-p.entered
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if err := hub.Dispose(gone); !errors.Is(err, context.Canceled) {
		t.Fatalf("Dispose error = %v, want context.Canceled", err)
	}
	close(p.gate)
	sb := <-p.created
	<-sb.disposed
	<-resolved
	deadline := time.Now().Add(waitTimeout)
	for !strings.Contains(logs.String(), "orphan stop failed") {
		if time.Now().After(deadline) {
			t.Fatalf("log = %q, want the orphan dispose error", logs.String())
		}
		time.Sleep(time.Millisecond)
	}
	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "sandboxId=docker:shared") {
		t.Errorf("log = %q", got)
	}
}

// syncBuffer is a bytes.Buffer safe for a logger on another goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestHubDisposeSwallowsTheSandboxErrorAndAllowsReconfigure: Dispose does not
// return the error of the sandbox's own dispose, and a later ConfigureShared works.
func TestHubDisposeSwallowsTheSandboxErrorAndAllowsReconfigure(t *testing.T) {
	hub, p := hubWith(t, true, 0)
	ctx := context.Background()
	if _, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{}); err != nil {
		t.Fatal(err)
	}
	sb := p.sandboxFor("docker:shared")
	sb.disposeErr = errors.New("docker stop failed")
	var logs syncBuffer
	hub.SetLogger(slog.New(slog.NewTextHandler(&logs, nil)))

	if err := hub.Dispose(ctx); err != nil {
		t.Fatalf("Dispose = %v, want nil", err)
	}
	// The swallowed error is logged at Warn with the sandbox id.
	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "sandboxId=docker:shared") || !strings.Contains(got, "docker stop failed") {
		t.Errorf("log = %q, want a Warn with the sandbox id and the error", got)
	}
	if got := sb.disposes(); !slices.Equal(got, []bool{false}) {
		t.Errorf("dispose calls = %v, want [false]", got)
	}
	if err := hub.Dispose(ctx); err != nil {
		t.Errorf("a second Dispose = %v, want nil", err)
	}
	if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Errorf("ConfigureShared after Dispose = %v", err)
	}
}

// TestHubReleaseAgentData: a running shared sandbox cleans through
// AgentDataRemover; a shared sandbox that is not running goes through the
// provider's ProviderDataRemover; an unconfigured hub does nothing.
func TestHubReleaseAgentData(t *testing.T) {
	ctx := context.Background()

	t.Run("running", func(t *testing.T) {
		hub, p := hubWith(t, true, 0)
		if _, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{}); err != nil {
			t.Fatal(err)
		}
		if err := hub.ReleaseAgentData(ctx, "agent-a"); err != nil {
			t.Fatal(err)
		}
		if got := p.sandboxFor("docker:shared").removedAgents(); !slices.Equal(got, []string{"agent-a"}) {
			t.Errorf("RemoveAgentData calls = %v", got)
		}
		if got := p.removedCalls(); len(got) != 0 {
			t.Errorf("the provider was asked too: %v", got)
		}
	})

	t.Run("not running", func(t *testing.T) {
		hub, p := hubWith(t, true, 0)
		if err := hub.ReleaseAgentData(ctx, "agent-a"); err != nil {
			t.Fatal(err)
		}
		want := []removeCall{{ID: "docker:shared", AgentID: "agent-a"}}
		if got := p.removedCalls(); !slices.Equal(got, want) {
			t.Errorf("RemoveData calls = %v, want %v", got, want)
		}
	})

	t.Run("failed creation is ignored", func(t *testing.T) {
		hub, p := hubWith(t, true, 1)
		p.gate = make(chan struct{})
		resolved := make(chan error, 1)
		go func() {
			_, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{})
			resolved <- err
		}()
		<-p.entered
		released := make(chan error, 1)
		go func() { released <- hub.ReleaseAgentData(ctx, "agent-a") }()
		close(p.gate)
		if err := <-released; err != nil {
			t.Fatal(err)
		}
		if err := <-resolved; err == nil {
			t.Fatal("the creation must have failed")
		}
		want := []removeCall{{ID: "docker:shared", AgentID: "agent-a"}}
		if got := p.removedCalls(); !slices.Equal(got, want) {
			t.Errorf("RemoveData calls = %v, want %v", got, want)
		}
	})

	t.Run("unconfigured", func(t *testing.T) {
		hub, p := hubWith(t, false, 0)
		if err := hub.ReleaseAgentData(ctx, "agent-a"); err != nil {
			t.Fatal(err)
		}
		if got := p.removedCalls(); len(got) != 0 {
			t.Errorf("RemoveData calls = %v", got)
		}
	})
}

// TestHubPolicyIsACopy: the policy a hub returns, and the slices handed to
// SetPolicy, are not shared with the hub.
func TestHubPolicyIsACopy(t *testing.T) {
	hub := sandboxhub.NewSandboxHub()
	p := hub.Policy()
	p.AllowedNetwork[0] = sandboxhub.NetworkFull
	p.OwnKinds = append(p.OwnKinds, sandboxhub.KindHost)
	p.MaxLimits.Cpus = 99
	if got := hub.Policy(); got.AllowedNetwork[0] != sandboxhub.NetworkNone || len(got.OwnKinds) != 1 || got.MaxLimits.Cpus != 2 {
		t.Errorf("a change to the returned policy reached the hub: %+v", got)
	}

	images := []string{"a"}
	hub.SetPolicy(sandboxhub.PolicyUpdate{AllowedImages: &images})
	images[0] = "mutated"
	if got := hub.Policy(); !slices.Equal(got.AllowedImages, []string{"a"}) {
		t.Errorf("a change to the SetPolicy input reached the hub: %v", got.AllowedImages)
	}
}

func TestHubRegisterRejectsNilAndTypedNil(t *testing.T) {
	hub := sandboxhub.NewSandboxHub()
	if err := hub.Register(nil); !errors.Is(err, sandboxhub.ErrNilProvider) {
		t.Errorf("Register(nil) = %v, want ErrNilProvider", err)
	}
	var typed *fakeProvider
	if err := hub.Register(typed); !errors.Is(err, sandboxhub.ErrNilProvider) {
		t.Errorf("Register(typed nil) = %v, want ErrNilProvider", err)
	}
	if _, ok := hub.Provider(sandboxhub.KindDocker); ok {
		t.Error("a nil provider was registered")
	}
}

func TestHubProviderLookup(t *testing.T) {
	hub, p := hubWith(t, true, 0)
	got, ok := hub.Provider(sandboxhub.KindDocker)
	if !ok || got != sandboxhub.Provider(p) {
		t.Errorf("Provider(docker) = %v, %v", got, ok)
	}
	if _, ok := hub.Provider(sandboxhub.KindHost); ok {
		t.Error("Provider(host) found a provider that was never registered")
	}
}

func TestHubRequiresApprovalUsesTheReplaceablePolicy(t *testing.T) {
	hub := sandboxhub.NewSandboxHub()
	host := sandboxhub.Traits{Kind: sandboxhub.KindHost}
	if !hub.RequiresApproval(host, sandboxhub.OperationFSWrite) || hub.RequiresApproval(host, sandboxhub.OperationFSRead) {
		t.Error("the default policy is not in use")
	}
	hub.SetApprovalPolicy(func(sandboxhub.Traits, sandboxhub.Operation) bool { return false })
	if hub.RequiresApproval(host, sandboxhub.OperationFSWrite) {
		t.Error("the replacement policy is not in use")
	}
	hub.SetApprovalPolicy(nil)
	if !hub.RequiresApproval(host, sandboxhub.OperationFSWrite) {
		t.Error("SetApprovalPolicy(nil) must restore the default")
	}
}

// TestHubForAgentErrorsPassThroughAsSandboxErrors: an error from ForAgent is
// wrapped with the shared sandbox id, unless it already is a SandboxError.
func TestHubForAgentErrorsPassThroughAsSandboxErrors(t *testing.T) {
	ctx := context.Background()
	hub, p := hubWith(t, true, 0)
	if _, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{}); err != nil {
		t.Fatal(err)
	}
	sb := p.sandboxFor("docker:shared")

	sb.forAgentErr = errors.New("volume gone")
	_, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{})
	expectCode(t, err, kernel.SandboxCodeEnvironmentUnavailable)
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.SandboxID != "docker:shared" || se.Message != `Sandbox "docker:shared" is unavailable: volume gone` {
		t.Errorf("wrapped error = %+v", se)
	}

	own := kernel.NewSandboxError("nope", kernel.SandboxCodeNotFound, "x")
	sb.forAgentErr = own
	_, err = hub.Resolve(ctx, "agent-a", sandboxhub.Selection{})
	if !errors.As(err, &se) || se != own {
		t.Errorf("a SandboxError must pass through unchanged, got %v", err)
	}
}

// TestHubProviderMisbehaviourBecomesAnError: a provider that returns a nil
// sandbox or panics fails the Resolve with an error and is not cached.
func TestHubProviderMisbehaviourBecomesAnError(t *testing.T) {
	ctx := context.Background()

	hub, p := hubWith(t, true, 0)
	p.returnNil = true
	_, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{})
	expectCode(t, err, kernel.SandboxCodeEnvironmentUnavailable)

	hub, p = hubWith(t, true, 0)
	p.panicWith = "boom"
	_, err = hub.Resolve(ctx, "agent-a", sandboxhub.Selection{})
	expectCode(t, err, kernel.SandboxCodeEnvironmentUnavailable)
	p.panicWith = nil
	if _, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{}); err != nil {
		t.Errorf("the failure was cached: %v", err)
	}
}

// TestOwnSandboxPolicyIgnoresNonFiniteCpus: NaN and infinity are not valid limits
// (only finite numbers count); the policy ceiling applies instead.
func TestOwnSandboxPolicyIgnoresNonFiniteCpus(t *testing.T) {
	hub, _ := hubWith(t, true, 0)
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, -1} {
		spec, err := hub.BuildOwnSpec(sandboxhub.OwnConfig{Limits: &sandboxhub.LimitsOverride{Cpus: &v}})
		if err != nil || spec.Limits == nil || spec.Limits.Cpus == nil || *spec.Limits.Cpus != 2 {
			t.Errorf("cpus %v: spec %+v, %v, want the ceiling 2", v, spec, err)
		}
	}
}

func TestLimitsApplyReplacesOnlySetFields(t *testing.T) {
	base := sandboxhub.Limits{MaxReadBytes: 1, MaxWriteBytes: 2, MaxListEntries: 3, MaxSearchMatches: 4, MaxExecTimeoutMs: 5, MaxExecOutputBytes: 6}
	zero := 0
	got := base.Apply(sandboxhub.LimitsPatch{MaxReadBytes: ptr(10), MaxExecTimeoutMs: &zero, MaxExecOutputBytes: ptr(60), MaxListEntries: ptr(30), MaxSearchMatches: ptr(40), MaxWriteBytes: ptr(20)})
	want := sandboxhub.Limits{MaxReadBytes: 10, MaxWriteBytes: 20, MaxListEntries: 30, MaxSearchMatches: 40, MaxExecTimeoutMs: 0, MaxExecOutputBytes: 60}
	if got != want {
		t.Errorf("Apply = %+v, want %+v", got, want)
	}
	if base.Apply(sandboxhub.LimitsPatch{}) != base || base.MaxReadBytes != 1 {
		t.Error("an empty patch must change nothing, and Apply must not modify its receiver")
	}
}
