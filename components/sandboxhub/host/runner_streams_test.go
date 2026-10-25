package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-160: previews and dev servers in a folder on the user's machine. The runner offers
// byte streams and services (hello.features); their bytes travel on a channel per
// request with acks for flow control. Server and runner are wired back to back through
// JSON, as in runner_test.go.

// framePair is a runner pair that also counts the frames each side sent.
type framePair struct {
	*runnerPair
	mu       sync.Mutex
	toRunner []sandboxhub.RunnerMessage
	toServer []sandboxhub.RunnerMessage
}

func (p *framePair) count(fromServer bool, typ string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	list := p.toServer
	if fromServer {
		list = p.toRunner
	}
	n := 0
	for _, m := range list {
		if m.Type == typ {
			n++
		}
	}
	return n
}

// connectFrames is connectRunner with frame counting and an optional hello rewrite
// (features: nil = as the runner says).
func connectFrames(t *testing.T, folder string, allowExec bool, features []sandboxhub.RunnerFeature, extra ...sandboxhub.RunnerFolderSpec) *framePair {
	t.Helper()
	specs := append([]sandboxhub.RunnerFolderSpec{{Path: folder, DenyExec: !allowExec}}, extra...)
	local, err := OpenRunnerFolders(t.Context(), specs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.StopServices(context.Background()) })
	h := sandboxhub.RunnerHelloFor(local)
	if features != nil {
		h.Features = features
	}
	helloJSON, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	hello, err := sandboxhub.ParseRunnerHello(helloJSON)
	if err != nil {
		t.Fatal(err)
	}
	p := &framePair{runnerPair: &runnerPair{local: local}}
	p.sb = sandboxhub.NewRunnerSandbox(sandboxhub.RunnerSandboxOptions{
		ID:    "remote:runner:s1",
		Hello: hello,
		Send: func(m sandboxhub.RunnerMessage) error {
			p.mu.Lock()
			p.toRunner = append(p.toRunner, m)
			p.mu.Unlock()
			b, err := json.Marshal(m)
			if err != nil {
				return err
			}
			p.handle.Receive(b)
			return nil
		},
	})
	p.handle = sandboxhub.ServeRunner(local, func(m sandboxhub.RunnerMessage) error {
		p.mu.Lock()
		p.toServer = append(p.toServer, m)
		p.mu.Unlock()
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		p.sb.Receive(b)
		return nil
	}, sandboxhub.ServeRunnerOptions{DenyExec: !allowExec})
	t.Cleanup(p.handle.Close)
	p.view, err = p.sb.ForAgent(t.Context(), "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func streamFolder(t *testing.T) string {
	t.Helper()
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("<h1>landing</h1>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return folder
}

func mustStream(t *testing.T, v sandboxhub.View) sandboxhub.StreamView {
	t.Helper()
	sv, ok := sandboxhub.AsStreamView(v)
	if !ok {
		t.Fatal("the runner view has no byte streams")
	}
	return sv
}

func mustServices(t *testing.T, v sandboxhub.View) sandboxhub.ServicesView {
	t.Helper()
	svc, ok := sandboxhub.AsServicesView(v)
	if !ok {
		t.Fatal("the runner view has no services")
	}
	return svc
}

func TestRunnerOffersStreamsAndServices(t *testing.T) {
	on := connectFrames(t, streamFolder(t), true, nil)
	if got := on.sb.Features(); !slices.Contains(got, sandboxhub.RunnerFeatureStream) || !slices.Contains(got, sandboxhub.RunnerFeatureServices) {
		t.Fatalf("features = %v", got)
	}
	mustStream(t, on.view)
	mustServices(t, on.view)

	off := connectFrames(t, streamFolder(t), false, nil)
	mustStream(t, off.view)
	if _, ok := off.view.(sandboxhub.ServicesView); ok {
		t.Fatal("services offered with commands off")
	}

	// An older runner (no features): neither, not even by type assertion.
	old := connectFrames(t, streamFolder(t), true, []sandboxhub.RunnerFeature{"something-else"})
	if len(old.sb.Features()) != 0 {
		t.Fatalf("features = %v", old.sb.Features())
	}
	if _, ok := old.view.(sandboxhub.StreamView); ok {
		t.Fatal("an older runner's view is a StreamView")
	}
	if _, ok := old.view.(sandboxhub.ServicesView); ok {
		t.Fatal("an older runner's view is a ServicesView")
	}
	if info, ok := func() (sandboxhub.RunnerInfo, bool) {
		hub := sandboxhub.NewSandboxHub()
		if _, err := hub.AttachRunner("a1", "s1", on.sb); err != nil {
			t.Fatal(err)
		}
		return hub.RunnerInfo("a1", "s1")
	}(); !ok || len(info.Features) != 2 {
		t.Fatalf("RunnerInfo features = %+v", info.Features)
	}
}

func TestRunnerReadsAWholeFileAndARange(t *testing.T) {
	folder := streamFolder(t)
	p := connectFrames(t, folder, true, nil)
	sv := mustStream(t, p.view)
	whole, err := sv.ReadStream(t.Context(), "index.html", sandboxhub.ReadStreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(whole.Body)
	_ = whole.Body.Close()
	if err != nil || string(body) != "<h1>landing</h1>\n" || whole.Size != 17 || whole.Path != rootOf(folder)+"/index.html" {
		t.Fatalf("whole = %+v %q %v", whole, body, err)
	}
	length := int64(7)
	part, err := sv.ReadStream(t.Context(), "index.html", sandboxhub.ReadStreamOptions{Offset: 4, Length: &length})
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(part.Body)
	_ = part.Body.Close()
	if err != nil || string(body) != "landing" || part.Offset != 4 || part.Length != 7 {
		t.Fatalf("part = %+v %q %v", part, body, err)
	}
}

func TestRunnerStreamsABigFileWithinTheWindow(t *testing.T) {
	folder := streamFolder(t)
	big := make([]byte, sandboxhub.RunnerStreamChunkBytes*(sandboxhub.RunnerStreamWindow+5)+123)
	for i := range big {
		big[i] = byte((i * 7) % 251)
	}
	if err := os.WriteFile(filepath.Join(folder, "model.bin"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	p := connectFrames(t, folder, true, nil)
	stream, err := mustStream(t, p.view).ReadStream(t.Context(), "model.bin", sandboxhub.ReadStreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()
	var got bytes.Buffer
	buf := make([]byte, 64*1024)
	ahead := 0
	for {
		n, err := stream.Body.Read(buf)
		got.Write(buf[:n])
		// A slow reader: the runner must stop at the window and wait for acks.
		time.Sleep(time.Millisecond)
		ahead = max(ahead, p.count(false, sandboxhub.RunnerMessageData)-p.count(true, sandboxhub.RunnerMessageAck))
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got.Bytes(), big) {
		t.Fatalf("read %d bytes, want %d equal ones", got.Len(), len(big))
	}
	if ahead > sandboxhub.RunnerStreamWindow {
		t.Fatalf("%d frames ahead of the reader, window %d", ahead, sandboxhub.RunnerStreamWindow)
	}
}

func TestRunnerStopsWhenTheReaderStopsEarly(t *testing.T) {
	folder := streamFolder(t)
	if err := os.WriteFile(filepath.Join(folder, "model.bin"), make([]byte, sandboxhub.RunnerStreamChunkBytes*20), 0o600); err != nil {
		t.Fatal(err)
	}
	p := connectFrames(t, folder, true, nil)
	stream, err := mustStream(t, p.view).ReadStream(t.Context(), "model.bin", sandboxhub.ReadStreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Body.Read(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	_ = stream.Body.Close()
	time.Sleep(50 * time.Millisecond)
	if p.count(true, sandboxhub.RunnerMessageCancel) == 0 {
		t.Fatal("the runner was not told to stop")
	}
	frames := p.count(false, sandboxhub.RunnerMessageData)
	time.Sleep(50 * time.Millisecond)
	if got := p.count(false, sandboxhub.RunnerMessageData); got != frames || frames > sandboxhub.RunnerStreamWindow+1 {
		t.Fatalf("data frames %d then %d", frames, got)
	}
}

func TestRunnerStreamErrorsLikeAnyRead(t *testing.T) {
	p := connectFrames(t, streamFolder(t), true, nil)
	sv := mustStream(t, p.view)
	_, err := sv.ReadStream(t.Context(), "missing.png", sandboxhub.ReadStreamOptions{})
	wantSandboxCode(t, err, kernel.SandboxCodeNotFound)
	_, err = sv.ReadStream(t.Context(), "../outside.txt", sandboxhub.ReadStreamOptions{})
	wantSandboxCode(t, err, kernel.SandboxCodePathEscape)
}

func TestRunnerWritesAnUploadAndLeavesNothingPastMaxBytes(t *testing.T) {
	folder := streamFolder(t)
	p := connectFrames(t, folder, true, nil)
	sv := mustStream(t, p.view)
	data := bytes.Repeat([]byte{7}, sandboxhub.RunnerStreamChunkBytes*3+9)
	res, err := sv.WriteStream(t.Context(), "files/tower.ifc", bytes.NewReader(data), sandboxhub.WriteStreamOptions{MaxBytes: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != rootOf(folder)+"/files/tower.ifc" || res.Bytes != len(data) || !res.Created {
		t.Fatalf("result = %+v", res)
	}
	if got, err := os.ReadFile(filepath.Join(folder, "files", "tower.ifc")); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("file = %d bytes, %v", len(got), err)
	}
	_, err = sv.WriteStream(t.Context(), "files/big.ifc", bytes.NewReader(data), sandboxhub.WriteStreamOptions{MaxBytes: 1000})
	wantSandboxCode(t, err, kernel.SandboxCodeTooLarge)
	entries, _ := os.ReadDir(filepath.Join(folder, "files"))
	if len(entries) != 1 || entries[0].Name() != "tower.ifc" {
		t.Fatalf("files = %v", entries)
	}
}

// echoSpec is a service running this test binary as the http-echo helper.
func echoSpec(t *testing.T, name, cwd string) sandboxhub.ServiceSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return sandboxhub.ServiceSpec{Name: name, Command: `"` + exe + `"`, Cwd: cwd, Env: map[string]string{"AEC_HOST_HELPER": "http-echo"}}
}

func waitListening(t *testing.T, svc sandboxhub.ServicesView, name string) sandboxhub.ServiceInfo {
	t.Helper()
	for range 150 {
		list, err := svc.ListServices(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.Name != name {
				continue
			}
			if s.Status == sandboxhub.ServiceListening {
				return s
			}
			if s.Status == sandboxhub.ServiceExited {
				logs, _ := svc.ServiceLogs(t.Context(), name, 0)
				t.Fatalf("%s exited: %s", name, logs.Text)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not start", name)
	return sandboxhub.ServiceInfo{}
}

// getOver sends one HTTP GET over a connect tunnel and closes it.
func getOver(t *testing.T, conn io.ReadWriteCloser, path string) string {
	t.Helper()
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n", path); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %q %v", path, res.StatusCode, body, err)
	}
	return string(body)
}

func TestRunnerServiceOverATunnel(t *testing.T) {
	folder := streamFolder(t)
	p := connectFrames(t, folder, true, nil)
	svc := mustServices(t, p.view)
	started, err := svc.StartService(t.Context(), echoSpec(t, "site", ""))
	if err != nil {
		t.Fatal(err)
	}
	if started.Cwd != rootOf(folder) {
		t.Fatalf("cwd = %q", started.Cwd)
	}
	info := waitListening(t, svc, "site")
	for _, path := range []string{"/hello", "/again"} {
		conn, err := svc.Connect(t.Context(), info.Port)
		if err != nil {
			t.Fatal(err)
		}
		if got := getOver(t, conn, path); got != "hi "+path {
			t.Fatalf("body = %q", got)
		}
	}
	if logs, err := svc.ServiceLogs(t.Context(), "site", 0); err != nil || logs.Name != "site" {
		t.Fatalf("logs = %+v, %v", logs, err)
	}
	stopped, err := svc.StopService(t.Context(), "site")
	if err != nil || stopped == nil || stopped.Status != sandboxhub.ServiceExited {
		t.Fatalf("stop = %+v, %v", stopped, err)
	}
	if none, err := svc.StopService(t.Context(), "nothing"); err != nil || none != nil {
		t.Fatalf("stop nothing = %+v, %v", none, err)
	}
}

func TestRunnerConnectsOnlyToItsOwnServices(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	p := connectFrames(t, streamFolder(t), true, nil)
	_, err = mustServices(t, p.view).Connect(t.Context(), ln.Addr().(*net.TCPAddr).Port)
	wantSandboxCode(t, err, kernel.SandboxCodeNotFound)
}

func TestRunnerServicesFollowCwdWithNamesUniqueAcrossFolders(t *testing.T) {
	second := streamFolder(t)
	p := connectFrames(t, streamFolder(t), true, nil, sandboxhub.RunnerFolderSpec{Path: second})
	svc := mustServices(t, p.view)
	other := rootOf(second)
	a, err := svc.StartService(t.Context(), echoSpec(t, "docs", other))
	if err != nil {
		t.Fatal(err)
	}
	if a.Cwd != other {
		t.Fatalf("cwd = %q", a.Cwd)
	}
	_, err = svc.StartService(t.Context(), echoSpec(t, "docs", ""))
	wantSandboxCode(t, err, kernel.SandboxCodeConflict)
	list, err := svc.ListServices(t.Context())
	if err != nil || len(list) != 1 || list[0].Name != "docs" || list[0].Cwd != other {
		t.Fatalf("list = %+v, %v", list, err)
	}
	info := waitListening(t, svc, "docs")
	conn, err := svc.Connect(t.Context(), info.Port)
	if err != nil {
		t.Fatal(err)
	}
	if got := getOver(t, conn, "/x"); got != "hi /x" {
		t.Fatalf("body = %q", got)
	}
}

func TestRunnerRefusesServicesWhereCommandsAreOff(t *testing.T) {
	second := streamFolder(t)
	p := connectFrames(t, streamFolder(t), true, nil, sandboxhub.RunnerFolderSpec{Path: second, DenyExec: true})
	_, err := mustServices(t, p.view).StartService(t.Context(), echoSpec(t, "nope", rootOf(second)))
	wantSandboxCode(t, err, kernel.SandboxCodePolicyDenied)
}

func TestRunnerStopsARemovedFoldersServices(t *testing.T) {
	second := streamFolder(t)
	p := connectFrames(t, streamFolder(t), true, nil, sandboxhub.RunnerFolderSpec{Path: second})
	svc := mustServices(t, p.view)
	if _, err := svc.StartService(t.Context(), echoSpec(t, "docs", rootOf(second))); err != nil {
		t.Fatal(err)
	}
	port := waitListening(t, svc, "docs").Port
	if ok, err := p.local.Remove(filepath.Base(second)); !ok || err != nil {
		t.Fatalf("remove = %v, %v", ok, err)
	}
	if list, err := svc.ListServices(t.Context()); err != nil || len(list) != 0 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	p.local.StopServices(t.Context())
	// The process is gone: soon nothing listens on its port (Windows frees it a moment
	// after the shell exits).
	for range 30 {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the removed folder's service still listens")
}
