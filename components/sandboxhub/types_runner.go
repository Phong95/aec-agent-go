package sandboxhub

import (
	"encoding/json"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Runner protocol (A-153) - a folder on the user's own machine as a sandbox.
//
//	client app (desktop / TUI)                server
//	-------------------------                 ------
//	picks a folder, opens a connection  --->  hello        -> RunnerSandbox, attached to
//	                                                          one session (SandboxHub)
//	runs the operation in the folder    <---  request      <- a workspace tool of that session
//	streams command output              --->  output
//	answers                             --->  response / error
//	stops a running operation           <---  cancel
//
// The SDK never opens the connection: the host (server, client app) carries these
// messages over whatever transport it has (a WebSocket) and hands them to
// RunnerSandbox.Receive on the server and to ServeRunner's handle on the client.
// Paths are logical POSIX paths, as in every sandbox: /workspace is the chosen folder.
// The JSON is the same as core/ts's, so either side may be either SDK.
// ---------------------------------------------------------------------------

// RunnerProtocolVersion is bumped when a message changes incompatibly; the server
// refuses a runner of another version.
const RunnerProtocolVersion = 1

// RunnerOperation is a View operation a runner serves. Byte streams and services
// are not offered.
type RunnerOperation string

// The operations a runner serves.
const (
	RunnerReadFile  RunnerOperation = "readFile"
	RunnerWriteFile RunnerOperation = "writeFile"
	RunnerStat      RunnerOperation = "stat"
	RunnerList      RunnerOperation = "list"
	RunnerSearch    RunnerOperation = "search"
	RunnerExec      RunnerOperation = "exec"
)

// The types of the runner messages.
const (
	RunnerMessageHello    = "hello"
	RunnerMessageRequest  = "request"
	RunnerMessageCancel   = "cancel"
	RunnerMessageOutput   = "output"
	RunnerMessageResponse = "response"
	RunnerMessageError    = "error"
)

// RunnerHello is the first message of a runner: who it is and what it offers.
type RunnerHello struct {
	Type     string `json:"type"`
	Protocol int    `json:"protocol"`
	OS       OS     `json:"os"`
	// FolderName is the display name of the chosen folder (its last segment), never
	// used to find files.
	FolderName string `json:"folderName"`
	// AllowExec false = the user turned commands off for this folder: exec is not offered.
	AllowExec bool `json:"allowExec"`
	// Limits are the runner's own ceilings; the server keeps the lower of these and
	// its defaults. A zero field is unset.
	Limits *Limits `json:"limits,omitempty"`
}

// RunnerMessage is one runner message after the hello; Type says which fields are set:
//
//	request  (server -> runner) ID, Op, Args - the View method's arguments without ctx
//	cancel   (server -> runner) ID
//	output   (runner -> server) ID, Stream, Data - live output of a running exec
//	response (runner -> server) ID, Result
//	error    (runner -> server) ID, Message, Code
type RunnerMessage struct {
	Type    string                  `json:"type"`
	ID      string                  `json:"id"`
	Op      RunnerOperation         `json:"op,omitempty"`
	Args    []json.RawMessage       `json:"args,omitempty"`
	Stream  OutputStream            `json:"stream,omitempty"`
	Data    string                  `json:"data,omitempty"`
	Result  json.RawMessage         `json:"result,omitempty"`
	Message string                  `json:"message,omitempty"`
	Code    kernel.SandboxErrorCode `json:"code,omitempty"`
}

// RunnerInfo is what a host shows about a session's runner; the event payload type,
// so SandboxRunnerChanged carries it as it is.
type RunnerInfo = events.RunnerInfo
