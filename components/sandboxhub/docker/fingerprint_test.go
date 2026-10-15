package docker

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// docker-fingerprint.json is recorded by wiregen from reference provider and sandbox
// objects (their constructors run no docker command): the exact JSON text that is
// hashed, the fingerprint, the names and the docker run
// argument list for a corpus of settings and specs, plus the names of a list of ids
// and the resolved settings of a list of options. wiregen rebuilds the JSON from the
// object and fails when it does not hash to the object's own fingerprint, so the record
// cannot drift. The provider must give the same values for every row: a container
// created earlier with the same settings is then found and reused instead of recreated.

// optionsFixture is the JSON form of the provider options.
type optionsFixture struct {
	DockerPath         string                    `json:"dockerPath"`
	NamePrefix         string                    `json:"namePrefix"`
	DefaultImage       string                    `json:"defaultImage"`
	User               string                    `json:"user"`
	DefaultResources   sandboxhub.LimitsOverride `json:"defaultResources"`
	TmpfsSize          string                    `json:"tmpfsSize"`
	Limits             sandboxhub.LimitsPatch    `json:"limits"`
	BuildDefaultImage  *bool                     `json:"buildDefaultImage"`
	PullImages         *bool                     `json:"pullImages"`
	OperationTimeoutMs *int64                    `json:"operationTimeoutMs"`
}

func (o optionsFixture) options() Options {
	opts := Options{
		DockerPath: o.DockerPath, NamePrefix: o.NamePrefix, DefaultImage: o.DefaultImage, User: o.User,
		DefaultResources: o.DefaultResources, TmpfsSize: o.TmpfsSize, Limits: o.Limits,
		BuildDefaultImage: o.BuildDefaultImage, PullImages: o.PullImages,
	}
	if o.OperationTimeoutMs != nil {
		opts.OperationTimeout = time.Duration(*o.OperationTimeoutMs) * time.Millisecond
	}
	return opts
}

type fixtureError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	SandboxID string `json:"sandboxId"`
}

type fingerprintCase struct {
	Name            string                   `json:"name"`
	Options         optionsFixture           `json:"options"`
	Spec            sandboxhub.Spec          `json:"spec"`
	Context         sandboxhub.CreateContext `json:"context"`
	FingerprintJSON string                   `json:"fingerprintJSON"`
	Fingerprint     string                   `json:"fingerprint"`
	Container       string                   `json:"container"`
	Volume          string                   `json:"volume"`
	RunArgs         []string                 `json:"runArgs"`
	Error           *fixtureError            `json:"error"`
}

type namesCase struct {
	Prefix    string `json:"prefix"`
	ID        string `json:"id"`
	Container string `json:"container"`
	Volume    string `json:"volume"`
}

type settingsFixture struct {
	DefaultImage       string                    `json:"defaultImage"`
	User               string                    `json:"user"`
	DefaultResources   sandboxhub.ResourceLimits `json:"defaultResources"`
	TmpfsSize          string                    `json:"tmpfsSize"`
	Limits             sandboxhub.Limits         `json:"limits"`
	BuildDefaultImage  bool                      `json:"buildDefaultImage"`
	PullImages         bool                      `json:"pullImages"`
	OperationTimeoutMs int64                     `json:"operationTimeoutMs"`
	NamePrefix         string                    `json:"namePrefix"`
}

type settingsCase struct {
	Options  optionsFixture   `json:"options"`
	Settings *settingsFixture `json:"settings"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type fingerprintFixture struct {
	Cases    []fingerprintCase `json:"cases"`
	Names    []namesCase       `json:"names"`
	Settings []settingsCase    `json:"settings"`
}

func loadFingerprintFixture(t *testing.T) fingerprintFixture {
	t.Helper()
	var f fingerprintFixture
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "docker-fingerprint.json"), &f); err != nil {
		t.Fatalf("decode docker-fingerprint.json: %v", err)
	}
	return f
}

func newProvider(t *testing.T, o optionsFixture) *Provider {
	t.Helper()
	p, err := New(o.options())
	if err != nil {
		t.Fatalf("New(%+v): %v", o, err)
	}
	return p
}

func TestFingerprintMatchesGolden(t *testing.T) {
	f := loadFingerprintFixture(t)
	if len(f.Cases) < 15 {
		t.Fatalf("docker-fingerprint.json has %d cases", len(f.Cases))
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			p := newProvider(t, c.Options)
			sb, err := newSandbox(p, c.Spec, c.Context)
			if c.Error != nil {
				var se *kernel.SandboxError
				if !errors.As(err, &se) {
					t.Fatalf("newSandbox error = %v, want a SandboxError", err)
				}
				if string(se.Code) != c.Error.Code || se.Message != c.Error.Message || se.SandboxID != c.Error.SandboxID {
					t.Errorf("error = {%s, %q, %q}, TS gave {%s, %q, %q}", se.Code, se.Message, se.SandboxID, c.Error.Code, c.Error.Message, c.Error.SandboxID)
				}
				return
			}
			if err != nil {
				t.Fatalf("newSandbox: %v", err)
			}
			text, err := fingerprintText(fingerprintInput{
				V: 1, Image: sb.image, Network: string(sb.traits.Network),
				Resources: fingerprintResources{Cpus: sb.resources.Cpus, MemoryMb: sb.resources.MemoryMb, Pids: sb.resources.Pids},
				Tmpfs:     p.settings.TmpfsSize, User: p.settings.User,
			})
			if err != nil {
				t.Fatalf("fingerprintText: %v", err)
			}
			if text != c.FingerprintJSON {
				t.Errorf("fingerprint JSON\n got: %s\nwant: %s", text, c.FingerprintJSON)
			}
			if sb.fingerprint != c.Fingerprint {
				t.Errorf("fingerprint = %s, TS gave %s", sb.fingerprint, c.Fingerprint)
			}
			if sb.containerName != c.Container || sb.volumeName != c.Volume {
				t.Errorf("names = %s / %s, TS gave %s / %s", sb.containerName, sb.volumeName, c.Container, c.Volume)
			}
			args, err := sb.runArgs()
			if err != nil {
				t.Fatalf("runArgs: %v", err)
			}
			if !slices.Equal(args, c.RunArgs) {
				t.Errorf("docker run arguments\n got: %q\nwant: %q", args, c.RunArgs)
			}
			if sb.Image() != sb.image || sb.ContainerName() != c.Container || sb.VolumeName() != c.Volume || sb.ID() != c.Context.ID {
				t.Errorf("accessors disagree with the record")
			}
		})
	}
}

func TestDefaultFingerprintIsTheRecordedOne(t *testing.T) {
	p := newProvider(t, optionsFixture{})
	sb, err := newSandbox(p, sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatal(err)
	}
	if sb.fingerprint != "674be3aed771258a" {
		t.Errorf("default fingerprint = %s, want 674be3aed771258a", sb.fingerprint)
	}
}

func TestNamesMatchGolden(t *testing.T) {
	f := loadFingerprintFixture(t)
	providers := map[string]*Provider{}
	for _, n := range f.Names {
		p, ok := providers[n.Prefix]
		if !ok {
			var err error
			p, err = New(Options{NamePrefix: n.Prefix})
			if err != nil {
				t.Fatalf("New(prefix %q): %v", n.Prefix, err)
			}
			providers[n.Prefix] = p
		}
		container, volume := p.NamesFor(n.ID)
		if container != n.Container || volume != n.Volume {
			t.Errorf("NamesFor(%q) with prefix %q = %q / %q, TS gave %q / %q", n.ID, n.Prefix, container, volume, n.Container, n.Volume)
		}
	}
}

func TestSettingsMatchGolden(t *testing.T) {
	f := loadFingerprintFixture(t)
	for i, c := range f.Settings {
		p, err := New(c.Options.options())
		if c.Error != nil {
			if err == nil || err.Error() != c.Error.Message {
				t.Errorf("settings case %d: New error = %v, TS threw %q", i, err, c.Error.Message)
			}
			continue
		}
		if err != nil {
			t.Errorf("settings case %d: New: %v", i, err)
			continue
		}
		got := p.Settings()
		want := c.Settings
		if got.DefaultImage != want.DefaultImage || got.User != want.User || got.DefaultResources != want.DefaultResources ||
			got.TmpfsSize != want.TmpfsSize || got.Limits != want.Limits || got.BuildDefaultImage != want.BuildDefaultImage ||
			got.PullImages != want.PullImages || got.NamePrefix != want.NamePrefix ||
			got.OperationTimeout != time.Duration(want.OperationTimeoutMs)*time.Millisecond {
			t.Errorf("settings case %d: Settings()\n got: %+v\nwant: %+v", i, got, *want)
		}
	}
}

func TestProviderIdentity(t *testing.T) {
	p := newProvider(t, optionsFixture{})
	if p.Name() != "sandbox:docker" || p.Version() != "1.0.0" || p.Kind() != sandboxhub.KindDocker {
		t.Errorf("identity = %s %s %s", p.Name(), p.Version(), p.Kind())
	}
	want := sandboxhub.Limits{
		MaxReadBytes: 2_000_000, MaxWriteBytes: 5_000_000, MaxListEntries: 5_000,
		MaxSearchMatches: 500, MaxExecTimeoutMs: 600_000, MaxExecOutputBytes: 2_000_000,
	}
	if DefaultLimits() != want {
		t.Errorf("DefaultLimits() = %+v", DefaultLimits())
	}
}

// TestImageNamesStartingWithDashAreRefused is a hardening (T-07-25): the image
// is a positional docker argument, so a name like "--privileged" would be read as a
// flag. No docker call happens.
func TestImageNamesStartingWithDashAreRefused(t *testing.T) {
	if _, err := New(Options{DefaultImage: "-v"}); err == nil {
		t.Errorf("New accepted a default image starting with \"-\"")
	}
	p := newProvider(t, optionsFixture{})
	_, err := newSandbox(p, sandboxhub.Spec{Kind: sandboxhub.KindDocker, Image: "--privileged"}, sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared})
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodePolicyDenied || se.SandboxID != "docker:shared" {
		t.Errorf("newSandbox error = %v, want policy_denied for docker:shared", err)
	}
}

// TestDockerFixturesCovered fails when a docker-*.json fixture exists that no test of
// this package reads, so a new golden cannot be added and forgotten.
func TestDockerFixturesCovered(t *testing.T) {
	covered := []string{"docker-exec-transcripts.json", "docker-fingerprint.json", "docker-helper.json", "docker-transcripts.json", "docker-view-transcripts.json"}
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "docker-") && !e.IsDir() {
			found = append(found, filepath.Base(e.Name()))
		}
	}
	slices.Sort(found)
	if !slices.Equal(found, covered) {
		t.Errorf("docker fixtures on disk = %v, covered by tests = %v", found, covered)
	}
}
