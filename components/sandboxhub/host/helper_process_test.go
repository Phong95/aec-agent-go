package host_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// helperLifetime bounds every helper that would otherwise run forever, so a test
// that fails to kill one cannot leave a process behind for long.
const helperLifetime = 60 * time.Second

// runHelper is the program the test binary becomes when AEC_HOST_HELPER names a mode.
// It returns the exit code. An unknown mode is 127, like a shell's unknown program.
//
//	sleep-forever    sleeps (at most helperLifetime)
//	spawn-grandchild starts a sleep-forever grandchild that inherits stdout, writes
//	                 "<own pid> <grandchild pid>" to the file named by
//	                 AEC_HOST_PIDFILE, then sleeps
//	orphan           the same, then exits 0 at once: the grandchild outlives its
//	                 parent and keeps the output pipe open
//	cwd              prints the working directory, no newline
//	env              prints AEC_HOST_SECRET (or "undefined"), a space, GREETING
//	exit3            exits with status 3
//	flood            writes "y\n" lines until a write fails
//	utf8-split       writes multi-byte characters in deliberately split writes
//	utf8-big         writes one chunk of 100 euro signs
//	write-n          writes AEC_HOST_N bytes of "x" in one write
//	stdin-echo       copies stdin to stdout
//	pidfile          writes its own pid to AEC_HOST_PIDFILE (proves it was started)
//	http-echo        A-160: an HTTP server on 127.0.0.1:$PORT answering "hi <path>"
//	                 (at most helperLifetime)
func runHelper(mode string) int {
	switch mode {
	case "sleep-forever":
		time.Sleep(helperLifetime)
		return 0
	case "spawn-grandchild", "orphan":
		return spawnGrandchild(mode == "orphan")
	case "cwd":
		dir, err := os.Getwd()
		if err != nil {
			return 2
		}
		_, _ = os.Stdout.WriteString(dir)
		return 0
	case "env":
		secret, ok := os.LookupEnv("AEC_HOST_SECRET")
		if !ok {
			secret = "undefined"
		}
		_, _ = os.Stdout.WriteString(secret + " " + os.Getenv("GREETING"))
		return 0
	case "exit3":
		return 3
	case "flood":
		chunk := []byte(strings.Repeat("y\n", 1000))
		deadline := time.Now().Add(helperLifetime)
		for time.Now().Before(deadline) {
			if _, err := os.Stdout.Write(chunk); err != nil {
				return 1
			}
		}
		return 0
	case "utf8-split":
		for _, part := range []string{"a\xe2\x82", "\xac b\xf0\x9f", "\x98\x80 c\xc3", "\xa9\n"} {
			_, _ = os.Stdout.WriteString(part)
			time.Sleep(60 * time.Millisecond)
		}
		return 0
	case "utf8-big":
		_, _ = os.Stdout.WriteString(strings.Repeat("\U000020ac", 100))
		return 0
	case "write-n":
		n, err := strconv.Atoi(os.Getenv("AEC_HOST_N"))
		if err != nil {
			return 2
		}
		_, _ = os.Stdout.WriteString(strings.Repeat("x", n))
		return 0
	case "stdin-echo":
		_, _ = io.Copy(os.Stdout, os.Stdin)
		return 0
	case "http-echo":
		srv := &http.Server{
			Addr:              "127.0.0.1:" + os.Getenv("PORT"),
			ReadHeaderTimeout: 5 * time.Second,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "hi "+r.URL.Path)
			}),
		}
		go func() {
			time.Sleep(helperLifetime)
			_ = srv.Close()
		}()
		_ = srv.ListenAndServe()
		return 0
	case "pidfile":
		if err := os.WriteFile(os.Getenv("AEC_HOST_PIDFILE"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			return 2
		}
		return 0
	}
	return 127
}

// spawnGrandchild starts a sleep-forever copy of this binary with the helper's own
// stdout and stderr (so it holds the output pipes of the command), records both pids
// and then sleeps, or exits at once when orphan is set.
func spawnGrandchild(orphan bool) int {
	exe, err := os.Executable()
	if err != nil {
		return 2
	}
	// A new file name cannot be guessed by the grandchild's environment: it inherits
	// everything of the helper except the mode.
	cmd := exec.CommandContext(context.Background(), exe) //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), "AEC_HOST_HELPER=sleep-forever")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return 2
	}
	pids := strconv.Itoa(os.Getpid()) + " " + strconv.Itoa(cmd.Process.Pid) + "\n"
	if err := os.WriteFile(os.Getenv("AEC_HOST_PIDFILE"), []byte(pids), 0o600); err != nil {
		return 2
	}
	if orphan {
		return 0
	}
	time.Sleep(helperLifetime)
	return 0
}
