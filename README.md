# AEC Agent — Go SDK

A Go SDK for building LLM agents that use tools. It covers sessions, a tool-calling loop with approval, memory, skills, artifacts, sandboxes and scheduled jobs. It is written as idiomatic Go: contexts, functional options, `iter.Seq2` streams and errors as values. It is a standalone SDK that evolves on its own.

**Module:** `github.com/Phong95/aec-agent-go`, Go 1.25+. **Status:** pre-release v0.x, so a minor release may change the API. **License:** Apache-2.0 (`LICENSE`).

## Install

```sh
go get github.com/Phong95/aec-agent-go@latest
```

Import the root package as `aec "github.com/Phong95/aec-agent-go"`.

The repository `github.com/Phong95/aec-agent-go` is a read-only mirror. The SDK is developed in a private monorepo, and each change merged there is published to the mirror as one snapshot commit. Releases are the mirror's `vX.Y.Z` tags.

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	aec "github.com/Phong95/aec-agent-go"
	_ "github.com/Phong95/aec-agent-go/components/llmhub/providers/all" // registers the providers by name
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/kernel"
)

func main() {
	ctx := context.Background()
	a, err := aec.CreateAgent(ctx, aec.AgentConfig{
		Provider:     "anthropic",
		APIKey:       os.Getenv("ANTHROPIC_API_KEY"),
		Model:        "claude-sonnet-4-20250514",
		SystemPrompt: "You are a helpful site assistant.",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer a.Dispose(ctx) // mandatory: the memory system runs goroutines

	if _, err := a.StartSession(ctx); err != nil {
		log.Fatal(err)
	}

	res, err := a.Chat(ctx, "Hi", agent.WithTemperature(0.2))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Message.Content)

	for chunk, err := range a.ChatStream(ctx, "Summarise that in one line.") {
		if err != nil {
			log.Fatal(err) // a failure is always the last pair
		}
		if chunk.Type == kernel.ChunkText && chunk.Content != nil {
			fmt.Print(*chunk.Content)
		}
	}
}
```

- **Providers:** `anthropic`, `openai`, `novita`, `bedrock`, `openai-subscription` and `gemini`.
  - Blank-import `components/llmhub/providers/all`, or a single provider package.
  - For `bedrock`, also set `SecretKey` and `Region`.
  - `AgentConfig.LLM` takes a provider you built in code.
  - `AgentConfig.Profile` edits the `AgentProfile` (loop, inference, memory, sandbox) before the agent is created.
  - `AgentConfig` never prints its keys.
- **Where things live:** the root package `aec` holds only `CreateAgent`, `AgentConfig`, `AgentCore` and their options. Everything else is imported from the package that owns it: the agent and its options from `components/agent`, shared types and errors from `kernel`, tools from `components/toolhub` and `components/toolhub/builtin/...`.
- **Options** (package `agent`):
  - `Chat` and `ChatStream` take `WithModel`, `WithProvider`, `WithTemperature`, `WithMaxTokens`, `WithTopP`, `WithStopSequences` and `WithModelOptions`.
  - `StartSession` takes `WithMetadata` and `WithSessionModel`.
  - `Complete` takes the model options and `WithCompleteSession`.
  - `aec.CreateAgent` takes `WithCore(core)`, to set up the core first, and `WithProviderRegistry(reg)`.
- **Full control:**
  - Build the core with `aec.NewAgentCore`, which takes `WithPersistence`, `WithEventBus`, `WithTrace` and `WithLogger`.
  - Register what the agent needs with `UseLLMProvider`, `UseTool`, `UseSkill` and `UseSandboxProvider`, then call `SpawnAgent(ctx, profile)`.
  - The core exposes its hubs (`LLMHub`, `ToolHub`, `SkillHub`, `SandboxHub`, `ArtifactHub`, `PersistenceHub`, `Events`, `Trace`) and its agents (`Agent`, `ListAgents`, `DisposeAgent`, `RemoveAgent`).

Runnable offline versions of these examples are in [`example_test.go`](example_test.go) (`go test -run Example .`).

## Writing a tool

```go
type deliveryArgs struct {
	Item     string `json:"item"`
	Quantity int    `json:"quantity"`
}

// import "github.com/Phong95/aec-agent-go/components/toolhub"
logDelivery := toolhub.NewTypedTool(toolhub.Definition{
	Name:        "log_delivery",
	Description: "Logs a material delivery on site.",
	Risk:        toolhub.RiskDraft,
	Parameters: toolhub.Object(
		toolhub.Prop("item", toolhub.String("What was delivered")),
		toolhub.Prop("quantity", toolhub.Integer("How many units")),
	).Require("item", "quantity"),
}, func(_ context.Context, a deliveryArgs, _ toolhub.Context) (toolhub.Output, error) {
	return toolhub.Text(fmt.Sprintf("logged %d x %s", a.Quantity, a.Item)), nil
})

a, err := aec.CreateAgent(ctx, aec.AgentConfig{ /* ... */ Tools: []toolhub.Handler{logDelivery}})
```

- **Ways to build a tool** (package `tool`):
  - `toolhub.NewTypedTool` decodes the call's JSON arguments into your type `A`. If decoding fails, the model gets an error output, and the call is not retried.
  - `toolhub.NewTool` gives you the raw `kernel.Arguments`.
  - Any type that implements `toolhub.Handler` also works.
- **Helpers:**
  - Schema: `toolhub.Object`, `Prop`, `String`, `Enum`, `Number`, `Integer`, `Boolean` and `Array`.
  - Output: `toolhub.Text`, `toolhub.Error` and `toolhub.Errorf`.
- **Risk decides whether a call needs approval:**
  - `toolhub.RiskReadOnly` and `toolhub.RiskDraft` run once the policy check passes.
  - `toolhub.RiskConsequential` asks the user before it runs.
  - **A tool with no Risk counts as consequential.**
  - If no interaction handler is set, the approval check fails and the call is denied.
- **Interaction handler:** implement `toolhub.InteractionHandler` and set it before you create the agent.
  - `AskUser` answers the `clarify` tool, and `RequestApproval` answers approval requests.
  - Both may be called concurrently and must honour ctx.
  - The SDK sets no timeouts; that is up to you.

```go
core, err := aec.NewAgentCore()
core.ToolHub().SetInteractionHandler(myHandler)
a, err := aec.CreateAgent(ctx, cfg, aec.WithCore(core))
```

**No built-in tool is registered automatically.** Pass the ones you want:

- `workspace.NewTools(workspace.Options{})`: 7 file and command tools, which act only inside the agent's sandbox;
- `artifacts.NewTools(...)`;
- `web.NewSearchTool` and `web.NewExtractTool`;
- `clarify.NewTool()`;
- `skills.NewManageTool()` and `skills.NewViewTool()`.

They live under `components/toolhub/builtin/` (`workspace`, `artifacts`, `web`, `clarify`, `skills`).

A zero options struct means the defaults.

## Layout and concepts

Each folder is a Go package named after the last part of its path. Contracts that a hub shares with its stores live in the hub's folder (`skillhub/skill`, `artifacthub/artifact`), which avoids import cycles.

```
core/go/
├── aec.go, agentcore*.go, createagent.go,
│   agentcore_options.go                 root package aec: CreateAgent, AgentCore
├── kernel/                              shared types, errors, Plugin, EncodeJSON
│   └── ordered/                         insertion-ordered JSON values
├── components/
│   ├── events/                          the typed event bus
│   ├── trace/                           trace spans and sinks
│   ├── llmhub/                          provider contract, hub and registry
│   │   └── providers/                   opt-in: anthropic, openaicompat, bedrock, openaisub, gemini, all
│   ├── toolhub/                         tool contracts, hub, policy, approval
│   │   └── builtin/                     workspace, artifacts, skills, web, clarify
│   ├── skillhub/                        the skill library
│   │   ├── skill/                       SKILL.md contracts and parser
│   │   └── essential/                   built-in essential skills
│   ├── artifacthub/                     per-agent artifacts with versions
│   │   └── artifact/                    artifact contracts
│   ├── sandboxhub/                      sandbox hub; opt-in: host/, docker/
│   ├── persistence/                     store contracts, in-memory stores, PersistenceHub
│   ├── agent/                           Agent and its managers
│   │   ├── agentloop/                   the tool-calling loop
│   │   ├── memorysystem/                memory extraction, recall, expiry
│   │   │   └── memory/                  memory contracts
│   │   └── profile/                     agent profile and loop config
│   └── orchestration/                   the registry of an AgentCore's agents
├── conformance/                         public conformance suites
├── aecmock/                             public fakes for testing apps built on the SDK
├── tests/                              everything that only tests the SDK:
│   ├── arch/, e2e/                      module-wide guards, full agent turns
│   └── testdata/                        golden fixtures, Go-written records, API snapshot
├── internal/                            module-private helpers (jsonx, jsstr, toolkit, ...)
└── scripts/                            check scripts (smoke, racecheck, releasecheck)
```

### Where things are: code, tests and test helpers

Several names here contain "test", but they do four different jobs:

| What | What it is | Who it is for |
| --- | --- | --- |
| `foo_test.go` next to `foo.go` | The tests of that one package. Go requires them in the same folder: `go test` finds them there, and only from there can they test the package's unexported details. | SDK developers |
| `tests/` | Everything that only tests the SDK and belongs to no single package: `tests/arch` checks rules for the whole SDK (which package may import which, no vendor SDK in the root, no leaked goroutines); `tests/e2e` runs full agent conversations through the public API; `tests/testdata` is the answer key the tests compare against. | SDK developers |
| `tests/testdata/` | Not code: the data files tests read. `golden/` holds saved correct outputs, `written/` the records the SDK writes, `api/aec.txt` the list of the root package's public API. The folder is named `testdata` so Go and the `tests/arch` checks skip it when they scan for code. | SDK tests |
| `aecmock/` | Not tests: a **public package of fakes** (a scripted AI model, mock tools, in-memory stores and sandbox) that you import to test **your own** app without real models, keys or network. Same idea as Go's `net/http/httptest`. | SDK users |

`conformance/` is the other public helper: ready-made test suites that check your own store, provider or tool behaves like the built-in ones.

So: SDK code lives in the root, `kernel/`, `components/` and `internal/`; the two public helpers are `aecmock/` and `conformance/`; everything else named "test" exists only to test the SDK itself.

| Package | What it does |
| --- | --- |
| `aec` (root) | The entry point: `CreateAgent`/`AgentConfig`, `AgentCore` and their options. Nothing is re-exported. |
| `kernel` | Shared types, typed errors, `Plugin` and `EncodeJSON`. |
| `components/events`, `trace` | The typed event bus, whose event log is a fixed-size ring buffer; trace spans and their sinks. |
| `components/llmhub`, `toolhub` | The provider contract, hub and registry; the tool contracts, hub, policy, approval and schema helpers. |
| `components/skillhub`, `artifacthub` | The skill library (SKILL.md files) with the built-in essential skills; per-agent artifacts with versions. |
| `components/sandboxhub`, `persistence` | The sandbox hub, path rules and approval policy; the store contracts, in-memory stores and `PersistenceHub`. |
| `components/agent` | `Agent`: sessions, chat and scheduled jobs. Subpackages: `agentloop` (the loop, tool execution and context compaction), `memorysystem` and `profile`. |

**Opt-in packages:** importing the root links no vendor SDK and no sandbox backend (`tests/arch` checks this with `go list -deps`). Import these yourself if you need them:

- `components/llmhub/providers/...`;
- `components/sandboxhub/host`: real folders on the host, not isolated;
- `components/sandboxhub/docker`: needs a Docker daemon;
- `aecmock`.

**Wire-exact JSON:** shared data types serialize to stable bytes, pinned by the golden fixtures. Use `kernel.EncodeJSON` for that output (it matches `JSON.stringify`); `json.Marshal` escapes `<`, `>`, `&`, U+2028 and U+2029.

## Behaviour you should know

- **One turn at a time per agent.**
  - A second `Chat` or `ChatStream` waits for the running turn and gives up when its ctx ends. A stream holds the turn while you iterate it.
  - A chat started from inside a chat of the same agent fails with `agent.ErrReentrantChat`.
  - A chat with no active session fails with `agent.ErrNoActiveSession`.
  - Switching session in the middle of a turn makes the turn fail with `agentloop.ErrSessionNotActive`.
  - *Superseded (A-97):* the rule above is now **one turn at a time per session**.
    - `Chat` and `ChatStream` take `agent.WithChatSession(id)`; without it they use the session `StartSession` or `LoadSession` set last.
    - Turns in different sessions of one agent run in parallel. Turns in the same session wait for each other, in arrival order, and give up when their ctx ends (nothing is saved then).
    - `profile.LoopConfig.MaxConcurrentTurns` (JSON `maxConcurrentTurns`) caps how many turns one agent runs at once; 0 or unset means no cap. `Agent.RunningTurns()` reports the turns running now.
    - `agent.ErrReentrantChat` is only for a nested chat on the same session. A nested chat on another session runs.
    - Each turn loads its session from the stores when it starts. A session that does not exist fails with `agent.ErrSessionNotFound`, and switching session mid-turn no longer affects the running turn (no more `ErrSessionNotActive`).
- **Cancellation is a result, not an error.** Cancelling the ctx stops the run at the next safe point. The result then has `Aborted == true`, and the error is nil.
- **Dispose is mandatory.** The memory system and scheduled jobs run goroutines.
  - `Agent.Dispose(ctx)` refuses new chats (`agent.ErrAgentDisposed`), cancels the running one, and waits for them to stop until ctx ends. (A-97: every running turn, in every session.)
  - `Agent.Delete(ctx)` also deletes the agent's own sandbox files.
  - `AgentCore.DisposeAgent` stops and unloads an agent but keeps its stored data. `RemoveAgent` deletes it.
  - `AgentCore.Dispose` disposes all agents in parallel, then cleans up the hubs with its own time budget.
- **Typed errors:**
  - `kernel.HasCode(err, kernel.ModelCodeRateLimit)` checks an error code, and `errors.Is(err, kernel.ErrModelAuth)` matches a sentinel.
  - `kernel.IsRetryable` reports whether retrying makes sense.
  - `*kernel.ModelError` has `StatusCode` and `RetryAfter`.
  - `kernel.WithCause` attaches a cause that `errors.Is` and `errors.As` can find but `Error()` never prints.
- **Stream errors:** a non-nil error is always the last pair. A model failure yields the error chunk together with the error, and no done chunk follows.
- **Background work is bounded.** Memory passes have a timeout, scheduled jobs can set `JobDefinition.Timeout`, and the event log is a ring buffer.

## Testing

`core/go/Makefile` wraps the usual commands; run `make` in `core/go` to list them. It works from PowerShell, cmd and Git Bash; the targets that run `scripts/*.sh` use Git Bash on Windows.

```sh
make test            # every package (TESTFLAGS=-v adds flags)
make check           # tidy, build, vet, lint and test: a quick pre-push check
make ci              # what the core-go CI job runs, including -race and the smoke test
make examples        # the godoc examples
make racecheck       # -race in a golang:1.25 container
make api             # refresh tests/testdata/api/aec.txt after an intended API change
```

- **Layout:**
  - Unit tests sit next to the code, and all of them check for leaked goroutines with goleak.
  - `tests/arch` holds the module-wide guards: the layer table (add every new package to it), the vendor and host-access rules, and the `go list -deps` checks.
  - `tests/e2e` runs full agent turns through `AgentCore` and `CreateAgent` on `aecmock` fakes.
- **Testing your own stores and providers:** run the public `conformance` suites. Each takes `*testing.T` and a factory:
  - stores: `SessionStore`, `MessageStore`, `MemoryStore`, `AgentStore`, `ProviderStore`, `ArtifactStore`, `SkillStore`;
  - the rest: `EventBus`, `ToolPolicy`, `ToolHandler`, `Provider`, `SandboxView`, `AgentLoop`.

  `aecmock` has `NewScriptedProvider`, `NewScriptedInteraction`, mocks and in-memory stores. `server/go` (its app and store tests) is a worked example.
- **Golden fixtures:**
  - `tests/testdata/golden` holds frozen scenario corpora (inputs with the expected outputs) for the providers, the agent loop, memory, tools, sandboxes and stored records. Tests always compare against them. When you change behaviour on purpose, edit the affected JSON by hand in the same commit.
  - `tests/testdata/written` holds records written by Go code. Refresh them with `AEC_WIRE_UPDATE=1 go test ./components/persistence/ -run TestGoWritten` and review the diff.
- **API snapshot:** `tests/testdata/api/aec.txt` records the root API. After an intended change, run `go test -run TestAPISnapshot -update .` and review the diff.
- **Race detector and opt-in tests:**
  - `-race` needs cgo, so `scripts/racecheck/racecheck.sh` (`make racecheck`) runs it in Docker; it accepts `go test` flags.
  - Live tests need `RUN_LIVE_TESTS=1`, and Docker tests need `AEC_DOCKER_TESTS=1`.

## Release

Releases are `vX.Y.Z` tags on the public mirror `github.com/Phong95/aec-agent-go`. The maintainer tags `core/go/vX.Y.Z` on the merge commit on `main` in the monorepo, and the `go-sdk-mirror` workflow republishes it as `vX.Y.Z`. [`RELEASE.md`](RELEASE.md) is the maintainer checklist.
