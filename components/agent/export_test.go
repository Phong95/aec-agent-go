package agent

import "github.com/Phong95/aec-agent-go/components/agent/agentloop"

// WithMemoryWrap is the test hook that replaces the memory port the loop sees with
// wrap(the agent's memory system), so a test can record the recalls of a turn or make
// them fail. It lives in a _test file, so it is not part of the package's API.
func WithMemoryWrap(wrap func(agentloop.Memory) agentloop.Memory) Option {
	return func(o *options) { o.memoryWrap = wrap }
}

// InspectReplay (A-174) exposes checks 2 and 3 of a verification replay to the tests.
var InspectReplay = inspectReplay
