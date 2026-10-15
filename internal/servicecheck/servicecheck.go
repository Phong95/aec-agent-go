// Package servicecheck is the shared suite for the optional "services" part of a
// sandbox view (A-124 phase 2, sandboxhub.ServicesView): start / list / logs / stop,
// the per-agent limit, an exited service's exit code, and Connect to a port inside the
// sandbox (what the preview proxy uses). Every provider that offers the capability
// runs it (host, and the opt-in real Docker tests), as core/ts runs
// tests/unit/sandbox-services.test.ts against its sandboxes. It needs node on the
// machine (host) or in the image (docker).
//
// It is a non-test file that imports "testing" so that tests in any package can use it.
package servicecheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Server is a tiny HTTP server on $PORT that answers "hi <url>" and logs a line.
// It works in sh and cmd.
const Server = `node -e "console.log('up on '+process.env.PORT);require('http').createServer((q,s)=>s.end('hi '+q.url)).listen(process.env.PORT,'127.0.0.1')"`

// Run runs the suite; newView returns a fresh view of one agent for each case. The
// suite stops the services a case started when it ends.
func Run(t *testing.T, newView func(t *testing.T) sandboxhub.View) {
	t.Helper()
	cases := []struct {
		name string
		run  func(t *testing.T, v sandboxhub.View, s sandboxhub.ServicesView)
	}{
		{"starts a service on a free port, connects to it, shows its logs and stops it", startConnectStop},
		{"refuses a second service of the same name, a taken port and past the limit", refusals},
		{"reports an exited service with its exit code and refuses a closed port", exited},
		{"keeps the service's working directory inside the roots", cwdEscape},
		{"refuses a name that is not plain, for stop and logs too", hostileNames},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := newView(t)
			s, ok := sandboxhub.AsServicesView(v)
			if !ok {
				t.Fatalf("view %s has no services (capabilities %v)", v.ID(), v.Capabilities())
			}
			t.Cleanup(func() { StopAll(t, s) })
			c.run(t, v, s)
		})
	}
}

// StopAll stops every service of the view (test cleanup).
func StopAll(t *testing.T, s sandboxhub.ServicesView) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := s.ListServices(ctx)
	if err != nil {
		return
	}
	for _, svc := range list {
		if _, err := s.StopService(ctx, svc.Name); err != nil {
			t.Logf("stop %s: %v", svc.Name, err)
		}
	}
}

// Until polls ListServices until the service has the status (15 s at most).
func Until(t *testing.T, s sandboxhub.ServicesView, name string, status sandboxhub.ServiceStatus) sandboxhub.ServiceInfo {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		list, err := s.ListServices(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var found *sandboxhub.ServiceInfo
		for i := range list {
			if list[i].Name == name {
				found = &list[i]
			}
		}
		if found != nil && found.Status == status {
			return *found
		}
		if time.Now().After(deadline) {
			state := "missing"
			if found != nil {
				state = string(found.Status)
			}
			t.Fatalf("service %s is %s, wanted %s", name, state, status)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// HTTPOver sends one HTTP/1.1 request over a sandbox connection and returns the whole
// raw response (the connection is closed afterwards).
func HTTPOver(t *testing.T, conn io.ReadWriteCloser, url string) string {
	t.Helper()
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n", url); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(conn)
	if err != nil && len(data) == 0 {
		t.Fatal(err)
	}
	return string(data)
}

func wantCode(t *testing.T, err error, code kernel.SandboxErrorCode) {
	t.Helper()
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func intPtr(n int) *int { return &n }

func startConnectStop(t *testing.T, v sandboxhub.View, s sandboxhub.ServicesView) {
	ctx := context.Background()
	started, err := s.StartService(ctx, sandboxhub.ServiceSpec{Name: "web", Command: Server})
	if err != nil {
		t.Fatal(err)
	}
	if started.Name != "web" || started.Command != Server || started.Cwd != v.WorkingDir() {
		t.Fatalf("started = %+v", started)
	}
	if started.Port <= 0 {
		t.Fatalf("port = %d", started.Port)
	}
	if started.Status != sandboxhub.ServiceStarting && started.Status != sandboxhub.ServiceListening {
		t.Fatalf("status = %s", started.Status)
	}

	up := Until(t, s, "web", sandboxhub.ServiceListening)
	conn, err := s.Connect(ctx, up.Port)
	if err != nil {
		t.Fatal(err)
	}
	response := HTTPOver(t, conn, "/hello?x=1")
	if !regexp.MustCompile(`^HTTP/1\.1 200`).MatchString(response) || !strings.Contains(response, "hi /hello?x=1") {
		t.Fatalf("response = %q", response)
	}

	logs, err := s.ServiceLogs(ctx, "web", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.Text, fmt.Sprintf("up on %d", up.Port)) {
		t.Fatalf("logs = %q", logs.Text)
	}

	stopped, err := s.StopService(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if stopped == nil || stopped.Status != sandboxhub.ServiceExited {
		t.Fatalf("stopped = %+v", stopped)
	}
	list, err := s.ListServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "web" || list[0].Status != sandboxhub.ServiceExited {
		t.Fatalf("list = %+v", list)
	}
	none, err := s.StopService(ctx, "nope")
	if err != nil || none != nil {
		t.Fatalf("stop nope = %+v, %v", none, err)
	}
}

func refusals(t *testing.T, _ sandboxhub.View, s sandboxhub.ServicesView) {
	ctx := context.Background()
	a, err := s.StartService(ctx, sandboxhub.ServiceSpec{Name: "a", Command: Server})
	if err != nil {
		t.Fatal(err)
	}
	Until(t, s, "a", sandboxhub.ServiceListening)
	_, err = s.StartService(ctx, sandboxhub.ServiceSpec{Name: "a", Command: Server})
	wantCode(t, err, kernel.SandboxCodeConflict)
	_, err = s.StartService(ctx, sandboxhub.ServiceSpec{Name: "b", Command: Server, Port: intPtr(a.Port)})
	wantCode(t, err, kernel.SandboxCodeConflict)
	if _, err := s.StartService(ctx, sandboxhub.ServiceSpec{Name: "b", Command: Server}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartService(ctx, sandboxhub.ServiceSpec{Name: "c", Command: Server}); err != nil {
		t.Fatal(err)
	}
	_, err = s.StartService(ctx, sandboxhub.ServiceSpec{Name: "d", Command: Server})
	wantCode(t, err, kernel.SandboxCodePolicyDenied)
	_, err = s.StartService(ctx, sandboxhub.ServiceSpec{Name: "Bad Name", Command: Server})
	wantCode(t, err, kernel.SandboxCodeInvalidArgument)
}

func exited(t *testing.T, _ sandboxhub.View, s sandboxhub.ServicesView) {
	ctx := context.Background()
	if _, err := s.StartService(ctx, sandboxhub.ServiceSpec{Name: "oops", Command: `node -e "console.log('boom');process.exit(3)"`}); err != nil {
		t.Fatal(err)
	}
	done := Until(t, s, "oops", sandboxhub.ServiceExited)
	if done.ExitCode == nil || *done.ExitCode != 3 {
		t.Fatalf("exit code = %v", done.ExitCode)
	}
	logs, err := s.ServiceLogs(ctx, "oops", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.Text, "boom") {
		t.Fatalf("logs = %q", logs.Text)
	}
	_, err = s.Connect(ctx, done.Port)
	wantCode(t, err, kernel.SandboxCodeNotFound)
	_, err = s.ServiceLogs(ctx, "ghost", 0)
	wantCode(t, err, kernel.SandboxCodeNotFound)
}

func cwdEscape(t *testing.T, _ sandboxhub.View, s sandboxhub.ServicesView) {
	_, err := s.StartService(context.Background(), sandboxhub.ServiceSpec{Name: "x", Command: Server, Cwd: "../../.."})
	wantCode(t, err, kernel.SandboxCodePathEscape)
}

// hostileNames: names go into file names on Docker, so only the plain form passes - for
// stop and logs too, which would otherwise reach another agent's services.
func hostileNames(t *testing.T, _ sandboxhub.View, s sandboxhub.ServicesView) {
	ctx := context.Background()
	_, err := s.StopService(ctx, "../x")
	wantCode(t, err, kernel.SandboxCodeInvalidArgument)
	_, err = s.ServiceLogs(ctx, "../x", 0)
	wantCode(t, err, kernel.SandboxCodeInvalidArgument)
}
