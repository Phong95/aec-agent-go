package docker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Services (A-124 phase 2) - the helper's service operations and its connect mode.
//
// svcstart, svcstop, svclist and svclogs are file operations (one JSON request, one
// JSON answer); the service itself runs detached in its own process group inside the
// container and outlives the call. Its record, log and exit code live in
// /tmp/aec-services/<agent key>/ (tmpfs: a container restart forgets them, as it ends
// the processes).
//
// connect: a JSON header line {"port":N} on stdin; stdout is "AEC-OK\n" or
// "AEC-ERR {json}\n", then stdin/stdout are piped to/from 127.0.0.1:<port> (or ::1)
// inside the container until either side closes - one docker exec per connection,
// so it works with --network none.
// ---------------------------------------------------------------------------

// Compile-time proof of the services contract.
var _ sandboxhub.ServicesView = (*view)(nil)

const (
	// maxServicesPerAgent is how many services one agent may run at once in a Docker sandbox.
	maxServicesPerAgent = 3
	// serviceLogMaxBytes is the most ServiceLogs returns of the end of a log.
	serviceLogMaxBytes = 64 * 1024
)

type serviceStartRequest struct {
	Name        string            `json:"name"`
	Command     string            `json:"command"`
	Cwd         string            `json:"cwd"`
	Roots       []string          `json:"roots"`
	WorkingDir  string            `json:"workingDir"`
	Port        *int              `json:"port,omitempty"`
	Env         map[string]string `json:"env"`
	MaxServices int               `json:"maxServices"`
	Op          string            `json:"op"`
}

type serviceStopRequest struct {
	Name       string `json:"name"`
	WorkingDir string `json:"workingDir"`
	Op         string `json:"op"`
}

type serviceListRequest struct {
	WorkingDir string `json:"workingDir"`
	Op         string `json:"op"`
}

type serviceLogsRequest struct {
	Name       string `json:"name"`
	WorkingDir string `json:"workingDir"`
	MaxBytes   int    `json:"maxBytes"`
	Op         string `json:"op"`
}

type connectHeader struct {
	Port int `json:"port"`
}

func validPort(port int) bool { return port > 0 && port < 65536 }

// serviceName is the form of a service name, as the helper checks it: it is joined into
// file names, so only the plain form passes - for stop and logs too.
var serviceName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

const serviceNameMessage = "Service names are lower-case letters, digits and dashes (at most 40)"

// StartService starts a service through the helper's svcstart.
func (v *view) StartService(ctx context.Context, spec sandboxhub.ServiceSpec) (sandboxhub.ServiceInfo, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityServices); err != nil {
		return sandboxhub.ServiceInfo{}, err
	}
	if !serviceName.MatchString(spec.Name) {
		return sandboxhub.ServiceInfo{}, v.fail(serviceNameMessage, kernel.SandboxCodeInvalidArgument)
	}
	if spec.Port != nil && !validPort(*spec.Port) {
		return sandboxhub.ServiceInfo{}, v.fail("port must be a whole number from 1 to 65535", kernel.SandboxCodeInvalidArgument)
	}
	// A Go map has no insertion order, so the names are checked in sorted order.
	for _, name := range slices.Sorted(maps.Keys(spec.Env)) {
		if !validEnvName(name) {
			return sandboxhub.ServiceInfo{}, v.fail(fmt.Sprintf("Invalid environment variable \"%s\"", name), kernel.SandboxCodeInvalidArgument)
		}
	}
	cwd := spec.Cwd
	if cwd == "" {
		cwd = v.workingDir
	}
	logical, err := v.logical(cwd)
	if err != nil {
		return sandboxhub.ServiceInfo{}, err
	}
	env := spec.Env
	if env == nil {
		env = map[string]string{}
	}
	raw, err := v.sb.helper(ctx, serviceStartRequest{
		Name: spec.Name, Command: spec.Command, Cwd: logical, Roots: v.roots, WorkingDir: v.workingDir,
		Port: spec.Port, Env: env, MaxServices: maxServicesPerAgent, Op: "svcstart",
	})
	if err != nil {
		return sandboxhub.ServiceInfo{}, err
	}
	var out sandboxhub.ServiceInfo
	err = v.decode(raw, &out)
	return out, err
}

// StopService stops a service (its whole process group); nil when there is none.
func (v *view) StopService(ctx context.Context, name string) (*sandboxhub.ServiceInfo, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityServices); err != nil {
		return nil, err
	}
	if !serviceName.MatchString(name) {
		return nil, v.fail(serviceNameMessage, kernel.SandboxCodeInvalidArgument)
	}
	raw, err := v.sb.helper(ctx, serviceStopRequest{Name: name, WorkingDir: v.workingDir, Op: "svcstop"})
	if err != nil {
		return nil, err
	}
	var out *sandboxhub.ServiceInfo
	if err := v.decode(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListServices lists this agent's services, the exited ones too.
func (v *view) ListServices(ctx context.Context) ([]sandboxhub.ServiceInfo, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityServices); err != nil {
		return nil, err
	}
	raw, err := v.sb.helper(ctx, serviceListRequest{WorkingDir: v.workingDir, Op: "svclist"})
	if err != nil {
		return nil, err
	}
	out := []sandboxhub.ServiceInfo{}
	if err := v.decode(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []sandboxhub.ServiceInfo{}
	}
	return out, nil
}

// ServiceLogs returns the end of a service's log (at most 64 KiB; maxBytes <= 0 is 64 KiB).
func (v *view) ServiceLogs(ctx context.Context, name string, maxBytes int) (sandboxhub.ServiceLogs, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityServices); err != nil {
		return sandboxhub.ServiceLogs{}, err
	}
	if !serviceName.MatchString(name) {
		return sandboxhub.ServiceLogs{}, v.fail(serviceNameMessage, kernel.SandboxCodeInvalidArgument)
	}
	limit := serviceLogMaxBytes
	if maxBytes > 0 {
		limit = min(maxBytes, serviceLogMaxBytes)
	}
	raw, err := v.sb.helper(ctx, serviceLogsRequest{Name: name, WorkingDir: v.workingDir, MaxBytes: limit, Op: "svclogs"})
	if err != nil {
		return sandboxhub.ServiceLogs{}, err
	}
	var out sandboxhub.ServiceLogs
	err = v.decode(raw, &out)
	return out, err
}

// Connect is one docker exec of the helper in connect mode per connection; it returns
// once the helper reached the port. The result is a net.Conn (processConn).
func (v *view) Connect(ctx context.Context, port int) (io.ReadWriteCloser, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityServices); err != nil {
		return nil, err
	}
	if !validPort(port) {
		return nil, v.fail("port must be a whole number from 1 to 65535", kernel.SandboxCodeInvalidArgument)
	}
	proc, err := v.sb.spawnRaw(ctx, "connect")
	if err != nil {
		return nil, err
	}
	stderr := &cappedBuffer{max: rawOutputMaxBytes}
	stderrDone := drainInto(stderr, proc.Stderr())
	// ctx bounds opening the connection only; the connection itself lives until Close.
	stop := context.AfterFunc(ctx, func() { _ = proc.Kill() })
	header, _ := jsonx.Marshal(connectHeader{Port: port})
	_, _ = proc.Stdin().Write(append(header, '\n'))

	out := bufio.NewReaderSize(proc.Stdout(), rawHeaderMaxBytes)
	line, readErr := out.ReadSlice('\n')
	stopped := stop()
	abandon := func() {
		_ = proc.Stdin().Close()
		_ = proc.Kill()
		_, _ = io.Copy(io.Discard, out)
		<-stderrDone
		_, _ = proc.Wait()
	}
	if readErr != nil || !stopped {
		abandon()
		if ctx.Err() != nil {
			return nil, v.fail("Operation cancelled", kernel.SandboxCodeCancelled)
		}
		if readErr == bufio.ErrBufferFull { //nolint:errorlint // ReadSlice returns the sentinel itself
			return nil, v.fail("Unexpected output from the sandbox helper", kernel.SandboxCodeEnvironmentUnavailable)
		}
		detail := firstLines(stderr.String())
		if detail == "" {
			detail = "no answer from the sandbox helper"
		}
		return nil, v.fail("The sandbox did not answer: "+detail, kernel.SandboxCodeEnvironmentUnavailable)
	}
	text := strings.TrimSuffix(string(line), "\n")
	switch {
	case text == "AEC-OK":
		return &processConn{proc: proc, r: out, stderrDone: stderrDone, port: port}, nil
	case strings.HasPrefix(text, "AEC-ERR "):
		abandon()
		return nil, helperError(strings.TrimPrefix(text, "AEC-ERR "), v.sb.id)
	default:
		abandon()
		return nil, v.fail("Unexpected output from the sandbox helper: "+truncateText(text, 200), kernel.SandboxCodeEnvironmentUnavailable)
	}
}

// processConn is a connect-mode docker exec as a net.Conn: bytes written go to the
// helper's stdin, its stdout is what is read (the bytes buffered after the AEC-OK
// line first). Close kills the docker client. Deadlines are not supported (no-ops):
// the docker client's pipes have none, and an http.Transport does not need them.
type processConn struct {
	proc       Process
	r          *bufio.Reader
	stderrDone <-chan struct{}
	port       int

	writeMu   sync.Mutex
	closeOnce sync.Once
}

// Compile-time proof that processConn stands in for a socket.
var _ net.Conn = (*processConn)(nil)

func (c *processConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *processConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	n, err := c.proc.Stdin().Write(p)
	if err != nil {
		return n, fmt.Errorf("the sandbox connection is closed: %w", err)
	}
	return n, nil
}

// CloseWrite ends the stream to the service (a half close); reading goes on.
func (c *processConn) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.proc.Stdin().Close()
}

func (c *processConn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.proc.Stdin().Close()
		_ = c.proc.Kill()
		<-c.stderrDone
		_, _ = c.proc.Wait()
	})
	return nil
}

// sandboxAddr is the address of either end of a processConn.
type sandboxAddr string

func (a sandboxAddr) Network() string { return "sandbox" }
func (a sandboxAddr) String() string  { return string(a) }

func (c *processConn) LocalAddr() net.Addr { return sandboxAddr("sandbox-helper") }
func (c *processConn) RemoteAddr() net.Addr {
	return sandboxAddr(net.JoinHostPort("127.0.0.1", strconv.Itoa(c.port)))
}
func (c *processConn) SetDeadline(time.Time) error      { return nil }
func (c *processConn) SetReadDeadline(time.Time) error  { return nil }
func (c *processConn) SetWriteDeadline(time.Time) error { return nil }
