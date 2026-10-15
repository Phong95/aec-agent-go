package sandboxhub

import (
	"context"
	"errors"
	"io"
	"maps"
	"slices"

	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// TracedView - a View that records every operation as a "sandbox-op" span.
//
// The entry point is WithTracing. The golden fixture sandbox-traced.json records
// the spans of the traced real in-memory view.
// ---------------------------------------------------------------------------

// tracedView is the decorator. It keeps no state beyond the inner view, the
// starter and the base attributes, so it is as safe for concurrent use as the
// inner view.
type tracedView struct {
	inner View
	start SpanStarter
	base  map[string]any
}

// WithTracing wraps v so every operation is recorded as a "sandbox-op" span.
// Spans carry the path or a command SUMMARY (program + argument count) and result
// metadata - never file contents, command arguments or command output, which may
// hold secrets (A-60 requirement).
//
// Every span starts with the attributes sandboxId, kind, tenancy (from v) and
// agentId and sessionId (only when set in scope), plus operation and the
// per-operation attributes, and ends with the result summary, or on an error with
// error (the SandboxError code, else "error") and message. The error is returned
// unchanged. A panic in v is not recovered and leaves its span open. A nil start
// returns v itself.
//
// The size attributes bytes (fs.write start), stdoutBytes and stderrBytes (exec
// end) are byte counts (len of the UTF-8 string), as their names say, not
// character counts, so non-ASCII text counts every byte.
func WithTracing(v View, start SpanStarter, scope TraceScope) View {
	if start == nil {
		return v
	}
	traits := v.Traits()
	base := map[string]any{
		"sandboxId": v.ID(),
		"kind":      string(traits.Kind),
		"tenancy":   string(traits.Tenancy),
	}
	if scope.AgentID != "" {
		base["agentId"] = scope.AgentID
	}
	if scope.SessionID != "" {
		base["sessionId"] = scope.SessionID
	}
	return &tracedView{inner: v, start: start, base: base}
}

// traced runs one operation inside a span: it starts the span with the base
// attributes, the operation and attrs, runs it, and ends the span with the
// summary or the error.
func traced[T any](tv *tracedView, operation string, attrs map[string]any, run func() (T, error), summarize func(T) map[string]any) (T, error) {
	startAttrs := make(map[string]any, len(tv.base)+len(attrs)+1)
	maps.Copy(startAttrs, tv.base)
	startAttrs["operation"] = operation
	maps.Copy(startAttrs, attrs)
	span := tv.start(operation, startAttrs)

	result, err := run()
	if err != nil {
		endSpan(span, map[string]any{"error": spanErrorCode(err), "message": err.Error()})
		return result, err
	}
	endSpan(span, summarize(result))
	return result, nil
}

func endSpan(span SpanEnder, attrs map[string]any) {
	if nilcheck.IsNil(span) {
		return
	}
	span.End(attrs)
}

// spanErrorCode is the code of a SandboxError, else "error".
func spanErrorCode(err error) string {
	var se *kernel.SandboxError
	if errors.As(err, &se) && se != nil {
		return string(se.Code)
	}
	return "error"
}

func (t *tracedView) ID() string         { return t.inner.ID() }
func (t *tracedView) Traits() Traits     { return t.inner.Traits() }
func (t *tracedView) Limits() Limits     { return t.inner.Limits() }
func (t *tracedView) WorkingDir() string { return t.inner.WorkingDir() }

// Capabilities returns a fresh copy of the inner view's capabilities.
func (t *tracedView) Capabilities() []Capability { return slices.Clone(t.inner.Capabilities()) }

// Roots returns a fresh copy of the inner view's roots.
func (t *tracedView) Roots() []string { return slices.Clone(t.inner.Roots()) }

func (t *tracedView) ReadFile(ctx context.Context, path string, opts ReadOptions) (FileContent, error) {
	return traced(t, "fs.read", map[string]any{"path": path},
		func() (FileContent, error) { return t.inner.ReadFile(ctx, path, opts) },
		func(r FileContent) map[string]any {
			return map[string]any{"path": r.Path, "bytes": r.Size, "truncated": r.Truncated}
		})
}

func (t *tracedView) WriteFile(ctx context.Context, path, content string, opts WriteOptions) (WriteResult, error) {
	// The length of the content in bytes.
	return traced(t, "fs.write", map[string]any{"path": path, "bytes": len(content)},
		func() (WriteResult, error) { return t.inner.WriteFile(ctx, path, content, opts) },
		func(r WriteResult) map[string]any {
			return map[string]any{"path": r.Path, "bytes": r.Bytes, "created": r.Created}
		})
}

func (t *tracedView) Stat(ctx context.Context, path string) (*FileStat, error) {
	return traced(t, "fs.stat", map[string]any{"path": path},
		func() (*FileStat, error) { return t.inner.Stat(ctx, path) },
		func(r *FileStat) map[string]any {
			if r == nil {
				return map[string]any{"exists": false}
			}
			return map[string]any{"exists": true, "type": string(r.Type)}
		})
}

func (t *tracedView) List(ctx context.Context, path string, opts ListOptions) (ListResult, error) {
	recursive := opts.Recursive != nil && *opts.Recursive
	return traced(t, "fs.list", map[string]any{"path": path, "recursive": recursive},
		func() (ListResult, error) { return t.inner.List(ctx, path, opts) },
		func(r ListResult) map[string]any {
			return map[string]any{"entries": len(r.Entries), "truncated": r.Truncated}
		})
}

func (t *tracedView) Search(ctx context.Context, q SearchQuery) (SearchResult, error) {
	literal := q.Literal == nil || *q.Literal
	attrs := map[string]any{"literal": literal}
	if q.Path != "" {
		attrs["path"] = q.Path
	}
	return traced(t, "fs.search", attrs,
		func() (SearchResult, error) { return t.inner.Search(ctx, q) },
		func(r SearchResult) map[string]any {
			return map[string]any{"matches": len(r.Matches), "truncated": r.Truncated}
		})
}

func (t *tracedView) Exec(ctx context.Context, req ExecRequest, opts ExecOptions) (ExecResult, error) {
	attrs := map[string]any{
		"argCount":  max(0, len(req.Command)-1),
		"timeoutMs": req.TimeoutMs,
	}
	// A shell command is summarised as "shell": its argv is the script, which may
	// hold secrets. An empty command leaves the attribute absent.
	switch {
	case req.Shell != nil && *req.Shell:
		attrs["command"] = "shell"
	case len(req.Command) > 0:
		attrs["command"] = req.Command[0]
	}
	if req.Cwd != "" {
		attrs["cwd"] = req.Cwd
	}
	return traced(t, "exec", attrs,
		func() (ExecResult, error) { return t.inner.Exec(ctx, req, opts) },
		func(r ExecResult) map[string]any {
			var exitCode any // nil: the process was killed, written as null
			if r.ExitCode != nil {
				exitCode = *r.ExitCode
			}
			return map[string]any{
				"exitCode":  exitCode,
				"timedOut":  r.TimedOut,
				"cancelled": r.Cancelled,
				"truncated": r.Truncated,
				// Byte lengths of the output.
				"stdoutBytes": len(r.Stdout),
				"stderrBytes": len(r.Stderr),
				"durationMs":  r.DurationMs,
				// A failed command is still a successful sandbox operation - flag,
				// don't mark the span as error.
				"failed": r.ExitCode == nil || *r.ExitCode != 0,
			}
		})
}

// ReadStream (A-124) records opening the stream - not the time a reader takes to
// consume Body. An inner view without "fs.stream" fails with capability_unsupported
// (AsStreamView on the traced view already says no, from its capabilities).
func (t *tracedView) ReadStream(ctx context.Context, path string, opts ReadStreamOptions) (*ByteStream, error) {
	attrs := map[string]any{"path": path, "offset": opts.Offset}
	if opts.Length != nil {
		attrs["length"] = *opts.Length
	}
	return traced(t, "fs.read-stream", attrs,
		func() (*ByteStream, error) {
			s, ok := t.inner.(StreamView)
			if !ok {
				return nil, kernel.NewSandboxError("This sandbox does not support fs.stream", kernel.SandboxCodeCapabilityUnsupported, t.inner.ID())
			}
			return s.ReadStream(ctx, path, opts)
		},
		func(r *ByteStream) map[string]any {
			return map[string]any{"path": r.Path, "size": r.Size, "offset": r.Offset, "length": r.Length}
		})
}

// WriteStream (A-124) records the whole upload.
func (t *tracedView) WriteStream(ctx context.Context, path string, body io.Reader, opts WriteStreamOptions) (WriteResult, error) {
	return traced(t, "fs.write-stream", map[string]any{"path": path},
		func() (WriteResult, error) {
			s, ok := t.inner.(StreamView)
			if !ok {
				return WriteResult{}, kernel.NewSandboxError("This sandbox does not support fs.stream", kernel.SandboxCodeCapabilityUnsupported, t.inner.ID())
			}
			return s.WriteStream(ctx, path, body, opts)
		},
		func(r WriteResult) map[string]any {
			return map[string]any{"path": r.Path, "bytes": r.Bytes, "created": r.Created}
		})
}

// servicesOf is the inner view's ServicesView, or capability_unsupported (AsServicesView
// on the traced view already says no, from its capabilities).
func (t *tracedView) servicesOf() (ServicesView, error) {
	s, ok := t.inner.(ServicesView)
	if !ok {
		return nil, kernel.NewSandboxError("This sandbox does not support services", kernel.SandboxCodeCapabilityUnsupported, t.inner.ID())
	}
	return s, nil
}

// StartService (A-124 phase 2) records the start. The span carries the name and the
// port, never the command (it may hold secrets).
func (t *tracedView) StartService(ctx context.Context, spec ServiceSpec) (ServiceInfo, error) {
	attrs := map[string]any{"name": spec.Name}
	if spec.Port != nil {
		attrs["port"] = *spec.Port
	}
	return traced(t, "service.start", attrs,
		func() (ServiceInfo, error) {
			s, err := t.servicesOf()
			if err != nil {
				return ServiceInfo{}, err
			}
			return s.StartService(ctx, spec)
		},
		func(r ServiceInfo) map[string]any {
			return map[string]any{"name": r.Name, "port": r.Port, "status": string(r.Status)}
		})
}

// StopService (A-124 phase 2) records the stop.
func (t *tracedView) StopService(ctx context.Context, name string) (*ServiceInfo, error) {
	return traced(t, "service.stop", map[string]any{"name": name},
		func() (*ServiceInfo, error) {
			s, err := t.servicesOf()
			if err != nil {
				return nil, err
			}
			return s.StopService(ctx, name)
		},
		func(r *ServiceInfo) map[string]any {
			if r == nil {
				return map[string]any{"found": false}
			}
			return map[string]any{"found": true, "status": string(r.Status)}
		})
}

// ListServices passes through untraced (it is polled).
func (t *tracedView) ListServices(ctx context.Context) ([]ServiceInfo, error) {
	s, err := t.servicesOf()
	if err != nil {
		return nil, err
	}
	return s.ListServices(ctx)
}

// ServiceLogs passes through untraced.
func (t *tracedView) ServiceLogs(ctx context.Context, name string, maxBytes int) (ServiceLogs, error) {
	s, err := t.servicesOf()
	if err != nil {
		return ServiceLogs{}, err
	}
	return s.ServiceLogs(ctx, name, maxBytes)
}

// Connect passes through untraced (one per proxied connection).
func (t *tracedView) Connect(ctx context.Context, port int) (io.ReadWriteCloser, error) {
	s, err := t.servicesOf()
	if err != nil {
		return nil, err
	}
	return s.Connect(ctx, port)
}

// Compile-time proof that the traced view can pass services through.
var _ ServicesView = (*tracedView)(nil)
