package host

import (
	"os"
	"runtime"
	"slices"
)

// passthroughEnvFor is the list of server variables a command gets besides the ones
// it asks for: just enough for programs to start, in a fixed order, for the given
// operating system name.
func passthroughEnvFor(goos string) []string {
	if goos == "windows" {
		return []string{
			"SystemRoot", "windir", "ComSpec", "PATHEXT", "PATH", "Path", "TEMP", "TMP", "USERPROFILE", "APPDATA",
			"LOCALAPPDATA", "ProgramFiles", "ProgramData", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
		}
	}
	return []string{"PATH", "HOME", "LANG", "TMPDIR", "USER", "SHELL"}
}

// passthroughEnv is the passthrough list of the operating system this binary runs on.
var passthroughEnv = passthroughEnvFor(runtime.GOOS)

// buildEnv is the whole environment of a command (threat T-07-22): each
// passthrough variable that is set in the server environment, then the request env
// laid over it. Nothing else of the server environment crosses. os.LookupEnv is
// case-insensitive on Windows, and os/exec drops the duplicate that "PATH" and
// "Path" make there, the last entry winning. The request keys are sorted so the
// order is stable.
func buildEnv(requestEnv map[string]string) []string {
	env := make([]string, 0, len(passthroughEnv)+len(requestEnv))
	for _, key := range passthroughEnv {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	keys := make([]string, 0, len(requestEnv))
	for key := range requestEnv {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		env = append(env, key+"="+requestEnv[key])
	}
	return env
}
