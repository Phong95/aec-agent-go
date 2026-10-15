package conformance_test

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// helperLifetime bounds every helper that would otherwise run for as long as the
// caller lets it, so a test that fails to kill one cannot leave a process behind.
const helperLifetime = 60 * time.Second

// runConformanceHelper is what the test binary becomes when AEC_CONFORMANCE_HELPER
// names a command: the seven commands the SandboxView suite runs (echo, pwd, false,
// cat, printenv, sleep, yes), so the suite runs on a host without coreutils - a
// Windows machine - exactly as it does anywhere else (CONTEXT B3). The output is the
// same on every operating system: lines end in LF, never CRLF.
//
// It returns the exit code of the command. Like a shell, an unknown command is 127.
func runConformanceHelper(mode string, args []string) int {
	switch mode {
	case "echo":
		_, _ = io.WriteString(os.Stdout, strings.Join(args, " ")+"\n")
		return 0
	case "pwd":
		dir, err := os.Getwd()
		if err != nil {
			return 1
		}
		_, _ = io.WriteString(os.Stdout, dir+"\n")
		return 0
	case "false":
		return 1
	case "cat":
		for _, name := range args {
			data, err := os.ReadFile(name)
			if err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "cat: %v\n", err)
				return 1
			}
			if _, err := os.Stdout.Write(data); err != nil {
				return 1
			}
		}
		return 0
	case "printenv":
		if len(args) != 1 {
			return 1
		}
		value, ok := os.LookupEnv(args[0])
		if !ok {
			return 1
		}
		_, _ = io.WriteString(os.Stdout, value+"\n")
		return 0
	case "sleep":
		if len(args) != 1 {
			return 1
		}
		seconds, err := strconv.ParseFloat(args[0], 64)
		if err != nil || seconds < 0 {
			return 1
		}
		time.Sleep(min(time.Duration(seconds*float64(time.Second)), helperLifetime))
		return 0
	case "yes":
		chunk := []byte(strings.Repeat("y\n", 4096))
		deadline := time.Now().Add(helperLifetime)
		for time.Now().Before(deadline) {
			if _, err := os.Stdout.Write(chunk); err != nil {
				return 1
			}
		}
		return 0
	}
	return 127
}
