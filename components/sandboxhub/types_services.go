package sandboxhub

import (
	"context"
	"io"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// ---------------------------------------------------------------------------
// Services (A-124 phase 2) - the optional "services" part of a View.
//
// A service is a long-running process (a dev server: npm run dev, a Python server)
// that outlives the call that started it. The user sees it live next to the chat:
// the host proxies a preview origin to the service's port over Connect, which works
// also when the sandbox has no network. A view with CapabilityServices implements
// ServicesView; a caller checks with AsServicesView.
// ---------------------------------------------------------------------------

// CapabilityServices is StartService / StopService / ListServices / ServiceLogs and
// Connect (ServicesView).
const CapabilityServices Capability = "services"

// ServiceSpec is what StartService starts.
type ServiceSpec struct {
	// Name is unique per agent: lower-case letters, digits and dashes, at most 40.
	Name string `json:"name"`
	// Command runs through the sandbox's shell, e.g.
	// "npm run dev -- --host 127.0.0.1 --port $PORT".
	Command string `json:"command"`
	// Cwd is a logical directory. Default: the working directory.
	Cwd string `json:"cwd,omitempty"`
	// Port is the port the service listens on inside the sandbox. nil = a free port is
	// picked. Either way it is passed as PORT.
	Port *int `json:"port,omitempty"`
	// Env holds explicit variables only, like exec.
	Env map[string]string `json:"env,omitempty"`
}

// ServiceStatus is the state of a service.
type ServiceStatus string

// The states of a service: starting until the port accepts connections, exited once
// the process ended.
const (
	ServiceStarting  ServiceStatus = "starting"
	ServiceListening ServiceStatus = "listening"
	ServiceExited    ServiceStatus = "exited"
)

// ServiceInfo describes one service.
type ServiceInfo struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	// Cwd is the canonical logical path.
	Cwd    string        `json:"cwd"`
	Port   int           `json:"port"`
	Status ServiceStatus `json:"status"`
	// ExitCode is the exit code once exited; nil while the service runs and when the
	// code is unknown (killed). On the wire it is written only for an exited service,
	// as a number or null (MarshalJSON), like core/ts.
	ExitCode *int `json:"exitCode,omitempty"`
	// StartedAt is epoch milliseconds.
	StartedAt int64 `json:"startedAt"`
}

// MarshalJSON writes exitCode (a number or null) exactly when the service exited.
func (s ServiceInfo) MarshalJSON() ([]byte, error) {
	if s.Status != ServiceExited {
		type plain ServiceInfo
		p := plain(s)
		p.ExitCode = nil
		return jsonx.Marshal(p)
	}
	return jsonx.Marshal(struct {
		Name      string        `json:"name"`
		Command   string        `json:"command"`
		Cwd       string        `json:"cwd"`
		Port      int           `json:"port"`
		Status    ServiceStatus `json:"status"`
		ExitCode  *int          `json:"exitCode"`
		StartedAt int64         `json:"startedAt"`
	}{s.Name, s.Command, s.Cwd, s.Port, s.Status, s.ExitCode, s.StartedAt})
}

// ServiceLogs is the end of a service's output.
type ServiceLogs struct {
	Name string `json:"name"`
	// Text is the end of the service's combined stdout + stderr.
	Text string `json:"text"`
	// Truncated is true when older output was left out.
	Truncated bool `json:"truncated"`
}

// ServicesView is the optional "services" part of a View.
type ServicesView interface {
	// StartService starts a long-running process that outlives the call. It fails
	// with "conflict" when a service of that name runs or the port is taken, and
	// "policy_denied" past the sandbox's limit of services per agent.
	StartService(ctx context.Context, spec ServiceSpec) (ServiceInfo, error)
	// StopService stops a service (its whole process group). nil (and no error) when
	// there is none of that name.
	StopService(ctx context.Context, name string) (*ServiceInfo, error)
	// ListServices lists this agent's services, the exited ones too (until restarted
	// or the sandbox restarts), sorted by name.
	ListServices(ctx context.Context) ([]ServiceInfo, error)
	// ServiceLogs returns the end of a service's output; maxBytes <= 0 is the
	// sandbox's ceiling.
	ServiceLogs(ctx context.Context, name string, maxBytes int) (ServiceLogs, error)
	// Connect opens a TCP connection to 127.0.0.1:<port> inside the sandbox, as a
	// byte stream - how a host proxies a preview to a service, also when the sandbox
	// has no network. ctx bounds opening it only; the caller closes the connection.
	// It is an io.ReadWriteCloser, not a net.Conn, because this contract package may
	// not import net (tests/arch host-access guard); the backends return a net.Conn
	// (host: the TCP connection, docker: an adapter over the helper process), so a
	// caller that needs one type-asserts it.
	Connect(ctx context.Context, port int) (io.ReadWriteCloser, error)
}

// AsServicesView returns v as a ServicesView when it has the "services" capability.
func AsServicesView(v View) (ServicesView, bool) {
	s, ok := v.(ServicesView)
	if !ok {
		return nil, false
	}
	for _, c := range v.Capabilities() {
		if c == CapabilityServices {
			return s, true
		}
	}
	return nil, false
}
