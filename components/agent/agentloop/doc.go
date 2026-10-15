// Package agentloop is the agent loop: the OTAR cycle (Observe, Think, Act, Reflect)
// that turns one user message into a final assistant message, calling the model,
// running tools and compacting a long conversation along the way.
//
// # What the loop is built from
//
// The loop talks to the rest of the SDK only through small consumer-side ports
// (Models, Tools, Skills, Context, Conversation, the optional Memory, plus the
// optional OptionResolver and ProviderNamer) bundled in Deps. It imports neither the
// persistence package nor a concrete tool hub: the agent package adapts its managers
// (ModelManager, ToolManager, SkillManager, ContextManager, ConversationManager)
// to these ports, and package aecmock holds scripted fakes for tests.
//
// # Contracts every run keeps
//
//   - Abort is a result, never an error: a run whose ctx is cancelled or whose
//     deadline passes returns RunResult.Aborted (09-03). Errors are for
//     preconditions (ErrNoActiveSession), port failures and model failures.
//   - Every conversation and session write (AddMessage, SetCompactedMessages,
//     UpdateSession) runs under context.WithoutCancel of the caller's ctx plus a
//     timeout (default 10 s, WithPersistTimeout), so a cancel can never lose the
//     user message or a partial reply, and a store that hangs cannot hang the run.
//     Model, tool and approval calls use the caller's ctx.
//   - Events carry a fixed payload key order and spans open and close in a fixed
//     order with their attributes in a fixed key order (ordered.Object), so a
//     trace sink or an event log sees a stable wire shape. The wiregen
//     transcripts under tests/testdata/golden/agentloop-*.json record that shape
//     and the tests replay them byte for byte.
//   - A long conversation is compacted before the model is asked: the Counter
//     estimates tokens (UTF-16 lengths, calibrated against the provider's real
//     usage, which is saved on the session between turns), the Compactor keeps a
//     protected head and a recent tail and has the summarizer replace the middle
//     by one summary message, and repairToolPairs makes sure a tool call is never
//     separated from its result in what the model receives. Recalled memories join
//     the last system message, and the context-assembly span records the exact
//     model input.
//   - A model failure on Run closes every open span, removes the trace context
//     and returns the *kernel.ModelError unchanged.
package agentloop
