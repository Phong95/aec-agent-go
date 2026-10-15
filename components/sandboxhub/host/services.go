package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Services (A-124 phase 2) - dev servers in a host sandbox. A service is a real
// child process started through the shell (sh -c / cmd /c) in its own process tree
// (a process group on Unix, a Job Object on Windows), with its output in a log file
// below os.TempDir()/aec-services. The records live in server memory on the Sandbox:
// a service ends with its sandbox (Dispose) and is forgotten when the server stops.
// ---------------------------------------------------------------------------

// Compile-time proof of the services contract.
var _ sandboxhub.ServicesView = (*view)(nil)

const (
	// maxServicesPerAgent is how many services one agent may run at once in a host sandbox.
	maxServicesPerAgent = 3
	// serviceLogMaxBytes is the most ServiceLogs returns of the end of a log.
	serviceLogMaxBytes = 64 * 1024
	// serviceProbeTimeout bounds one TCP probe of a service's port.
	serviceProbeTimeout = 500 * time.Millisecond
	// serviceStopWait is how long StopService waits for the killed tree to end
	// (30 x 100 ms, like core/ts).
	serviceStopWait = 3 * time.Second
)

// serviceName is the form of a service name: lower-case letters, digits and dashes.
var serviceName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// hostService is one service of a host sandbox.
type hostService struct {
	name      string
	command   string
	cwd       string // canonical logical path
	port      int
	startedAt int64
	log       string // host path of the log file
	tree      *tree
	// done closes once the process was waited for; exited and exitCode are final then.
	done chan struct{}

	mu       sync.Mutex
	exited   bool
	exitCode *int
	killed   bool
}

func (h *hostService) state() (exited bool, exitCode *int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exited, h.exitCode
}

// kill ends the whole tree; the exit code of a killed service is unknown (nil), as
// for a process ended by a signal.
func (h *hostService) kill() {
	h.mu.Lock()
	if h.exited {
		h.mu.Unlock()
		return
	}
	h.killed = true
	h.mu.Unlock()
	_ = h.tree.kill()
}

// waitDone waits at most serviceStopWait for the process to end.
func (h *hostService) waitDone() {
	t := time.NewTimer(serviceStopWait)
	defer t.Stop()
	select {
	case <-h.done:
	case <-t.C:
	}
}

// serviceKey is the key of a service in Sandbox.services: "<workingDir>\n<name>".
func (v *view) serviceKey(name string) string { return v.workingDir + "\n" + name }

// ownServices are this agent's services, sorted by name.
func (v *view) ownServices() []*hostService {
	prefix := v.workingDir + "\n"
	v.sb.mu.Lock()
	out := make([]*hostService, 0, len(v.sb.services))
	for key, s := range v.sb.services {
		if strings.HasPrefix(key, prefix) {
			out = append(out, s)
		}
	}
	v.sb.mu.Unlock()
	slices.SortFunc(out, func(a, b *hostService) int { return strings.Compare(a.name, b.name) })
	return out
}

func (v *view) service(name string) *hostService {
	v.sb.mu.Lock()
	defer v.sb.mu.Unlock()
	return v.sb.services[v.serviceKey(name)]
}

// describeService probes the port of a running service: listening once it accepts
// connections, starting before.
func describeService(ctx context.Context, s *hostService) sandboxhub.ServiceInfo {
	exited, code := s.state()
	info := sandboxhub.ServiceInfo{
		Name: s.name, Command: s.command, Cwd: s.cwd, Port: s.port, Status: sandboxhub.ServiceExited, StartedAt: s.startedAt,
	}
	if exited {
		info.ExitCode = code
		return info
	}
	info.Status = sandboxhub.ServiceStarting
	if conn, err := dialLoopback(ctx, s.port, serviceProbeTimeout); err == nil {
		_ = conn.Close()
		info.Status = sandboxhub.ServiceListening
	}
	return info
}

// dialLoopback opens a TCP connection to the port on this machine: IPv4 loopback,
// then IPv6. timeout > 0 bounds each attempt.
func dialLoopback(ctx context.Context, port int, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err == nil {
		return conn, nil
	}
	return d.DialContext(ctx, "tcp", net.JoinHostPort("::1", strconv.Itoa(port)))
}

// portFree reports whether the port can be listened on at 127.0.0.1.
func portFree(ctx context.Context, port int) bool {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// freePort is a free port the OS picks.
func freePort(ctx context.Context) (int, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port //nolint:forcetypeassert // a tcp listener's address is a *net.TCPAddr
	_ = l.Close()
	return port, nil
}

// serviceLogDir is the folder of this agent's service logs:
// os.TempDir()/aec-services/<sha256(base + "\n" + workingDir), first 16 hex>.
func (v *view) serviceLogDir() string {
	sum := sha256.Sum256([]byte(v.sb.base + "\n" + v.workingDir))
	return filepath.Join(os.TempDir(), "aec-services", hex.EncodeToString(sum[:])[:16])
}

func validPort(port int) bool { return port > 0 && port < 65536 }

// StartService starts the command through the shell with PORT set and returns at
// once (status starting or listening).
func (v *view) StartService(ctx context.Context, spec sandboxhub.ServiceSpec) (sandboxhub.ServiceInfo, error) { //nolint:contextcheck // the service outlives the call: it runs on a context without the caller's cancellation
	if err := v.begin(ctx, sandboxhub.CapabilityServices); err != nil {
		return sandboxhub.ServiceInfo{}, err
	}
	if !serviceName.MatchString(spec.Name) {
		return sandboxhub.ServiceInfo{}, v.fail("Service names are lower-case letters, digits and dashes (at most 40)", kernel.SandboxCodeInvalidArgument)
	}
	if strings.TrimSpace(spec.Command) == "" {
		return sandboxhub.ServiceInfo{}, v.fail("command must be a non-empty string", kernel.SandboxCodeInvalidArgument)
	}
	if spec.Port != nil && !validPort(*spec.Port) {
		return sandboxhub.ServiceInfo{}, v.fail("port must be a whole number from 1 to 65535", kernel.SandboxCodeInvalidArgument)
	}
	cwdInput := spec.Cwd
	if cwdInput == "" {
		cwdInput = v.workingDir
	}
	res, err := v.resolve(cwdInput)
	if err != nil {
		return sandboxhub.ServiceInfo{}, err
	}
	label := spec.Cwd
	if label == "" {
		label = "."
	}
	err = inRoot(res.realRoot, res.realPath, func(root *os.Root, rel string) error {
		st, err := root.Stat(rel)
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return v.fail(fmt.Sprintf("\"%s\" is not a directory", spec.Cwd), kernel.SandboxCodeInvalidPath)
		}
		return nil
	})
	if err != nil {
		return sandboxhub.ServiceInfo{}, mapFSError(err, label, v.sb.id)
	}

	if err := v.checkServiceRoom(spec.Name); err != nil {
		return sandboxhub.ServiceInfo{}, err
	}
	var port int
	if spec.Port != nil {
		port = *spec.Port
		if !portFree(ctx, port) {
			return sandboxhub.ServiceInfo{}, v.fail(fmt.Sprintf("Port %d is already in use", port), kernel.SandboxCodeConflict)
		}
	} else if port, err = freePort(ctx); err != nil {
		return sandboxhub.ServiceInfo{}, v.fail("No free port found: "+err.Error(), kernel.SandboxCodeEnvironmentUnavailable)
	}

	env := maps.Clone(spec.Env)
	if env == nil {
		env = map[string]string{}
	}
	env["PORT"] = strconv.Itoa(port)
	dir := v.serviceLogDir()
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return sandboxhub.ServiceInfo{}, v.fail("Could not create the service log folder: "+err.Error(), kernel.SandboxCodeEnvironmentUnavailable)
	}
	logPath := filepath.Join(dir, spec.Name+".log")
	logFile, err := os.Create(logPath) //nolint:gosec // G304: a fixed folder below the temp dir and a validated service name
	if err != nil {
		return sandboxhub.ServiceInfo{}, v.fail("Could not create the service log: "+err.Error(), kernel.SandboxCodeEnvironmentUnavailable)
	}
	// The service outlives this call: it must not be killed when the caller's ctx ends.
	cmd := shellCommand(context.WithoutCancel(ctx), []string{spec.Command})
	cmd.Dir = res.realPath
	cmd.Env = buildEnv(env)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	tr, err := startCommand(cmd)
	_ = logFile.Close() // the child has its own handle
	if err != nil {
		return sandboxhub.ServiceInfo{}, v.fail(fmt.Sprintf("Could not start \"%s\": %v", spec.Name, err), kernel.SandboxCodeEnvironmentUnavailable)
	}
	s := &hostService{
		name: spec.Name, command: spec.Command, cwd: res.logical, port: port,
		startedAt: time.Now().UnixMilli(), log: logPath, tree: tr, done: make(chan struct{}),
	}
	go s.wait(cmd)

	v.sb.mu.Lock()
	if v.sb.disposed {
		v.sb.mu.Unlock()
		s.kill() //nolint:contextcheck // killing a tree must not depend on a caller's context (the taskkill fallback bounds itself)
		s.waitDone()
		return sandboxhub.ServiceInfo{}, v.sb.disposedError()
	}
	if old := v.sb.services[v.serviceKey(spec.Name)]; old != nil {
		if exited, _ := old.state(); !exited {
			// Another call started a service of this name meanwhile.
			v.sb.mu.Unlock()
			s.kill() //nolint:contextcheck // killing a tree must not depend on a caller's context (the taskkill fallback bounds itself)
			s.waitDone()
			return sandboxhub.ServiceInfo{}, v.fail(fmt.Sprintf("A service named \"%s\" is already running — stop it first", spec.Name), kernel.SandboxCodeConflict)
		}
	}
	if v.sb.services == nil {
		v.sb.services = map[string]*hostService{}
	}
	v.sb.services[v.serviceKey(spec.Name)] = s
	v.sb.mu.Unlock()
	return describeService(ctx, s), nil
}

// checkServiceRoom fails when a service of that name runs, or when the agent already
// runs the most services it may.
func (v *view) checkServiceRoom(name string) error {
	var running []string
	for _, s := range v.ownServices() {
		if exited, _ := s.state(); !exited {
			running = append(running, s.name)
		}
	}
	if slices.Contains(running, name) {
		return v.fail(fmt.Sprintf("A service named \"%s\" is already running — stop it first", name), kernel.SandboxCodeConflict)
	}
	if len(running) >= maxServicesPerAgent {
		return v.fail(fmt.Sprintf("At most %d services may run at once — stop one first (%s)", maxServicesPerAgent, strings.Join(running, ", ")), kernel.SandboxCodePolicyDenied)
	}
	return nil
}

// wait records the exit of the process and releases its tree.
func (h *hostService) wait(cmd *exec.Cmd) {
	_ = cmd.Wait()
	h.mu.Lock()
	h.exited = true
	if !h.killed && cmd.ProcessState != nil {
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			h.exitCode = &code
		}
	}
	h.mu.Unlock()
	h.tree.release()
	close(h.done)
}

// StopService kills the service's whole tree and waits up to 3 s for it to end.
func (v *view) StopService(ctx context.Context, name string) (*sandboxhub.ServiceInfo, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityServices); err != nil {
		return nil, err
	}
	// The same form as at start (the Docker helper joins names into file names).
	if !serviceName.MatchString(name) {
		return nil, v.fail("Service names are lower-case letters, digits and dashes (at most 40)", kernel.SandboxCodeInvalidArgument)
	}
	s := v.service(name)
	if s == nil {
		return nil, nil //nolint:nilnil // nil, nil is the contract: there is no service of that name
	}
	if exited, _ := s.state(); !exited {
		s.kill() //nolint:contextcheck // killing a tree must not depend on a caller's context (the taskkill fallback bounds itself)
		s.waitDone()
	}
	info := describeService(ctx, s)
	return &info, nil
}

// ListServices lists this agent's services, the exited ones too.
func (v *view) ListServices(ctx context.Context) ([]sandboxhub.ServiceInfo, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityServices); err != nil {
		return nil, err
	}
	own := v.ownServices()
	out := make([]sandboxhub.ServiceInfo, len(own))
	var wg sync.WaitGroup
	for i, s := range own {
		wg.Go(func() { out[i] = describeService(ctx, s) })
	}
	wg.Wait()
	return out, nil
}

// ServiceLogs returns the end of the service's log, at most maxBytes (clamped to 64 KiB;
// 0 or less is 64 KiB).
func (v *view) ServiceLogs(ctx context.Context, name string, maxBytes int) (sandboxhub.ServiceLogs, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityServices); err != nil {
		return sandboxhub.ServiceLogs{}, err
	}
	if !serviceName.MatchString(name) {
		return sandboxhub.ServiceLogs{}, v.fail("Service names are lower-case letters, digits and dashes (at most 40)", kernel.SandboxCodeInvalidArgument)
	}
	s := v.service(name)
	if s == nil {
		return sandboxhub.ServiceLogs{}, v.fail(fmt.Sprintf("There is no service named \"%s\"", name), kernel.SandboxCodeNotFound)
	}
	limit := serviceLogMaxBytes
	if maxBytes > 0 {
		limit = min(maxBytes, serviceLogMaxBytes)
	}
	raw, err := os.ReadFile(s.log)
	if err != nil {
		raw = nil
	}
	start := max(0, len(raw)-limit)
	return sandboxhub.ServiceLogs{Name: name, Text: strings.ToValidUTF8(string(raw[start:]), string(utf8.RuneError)), Truncated: start > 0}, nil
}

// Connect dials the port on this machine (127.0.0.1, then ::1). The result is a
// *net.TCPConn.
func (v *view) Connect(ctx context.Context, port int) (io.ReadWriteCloser, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityServices); err != nil {
		return nil, err
	}
	if !validPort(port) {
		return nil, v.fail("port must be a whole number from 1 to 65535", kernel.SandboxCodeInvalidArgument)
	}
	conn, err := dialLoopback(ctx, port, 0)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return nil, v.fail("Operation cancelled", kernel.SandboxCodeCancelled)
		}
		return nil, v.fail(fmt.Sprintf("Nothing listens on port %d", port), kernel.SandboxCodeNotFound)
	}
	return conn, nil
}

// stopServices kills every service of the sandbox and waits (bounded) for them to end:
// services end with their sandbox.
func (s *Sandbox) stopServices() {
	s.mu.Lock()
	all := slices.Collect(maps.Values(s.services))
	s.services = nil
	s.mu.Unlock()
	for _, svc := range all {
		svc.kill()
	}
	for _, svc := range all {
		svc.waitDone()
	}
}
