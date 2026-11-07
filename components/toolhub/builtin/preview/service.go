package preview

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ---------------------------------------------------------------------------
// A-124 phase 2: dev servers in the agent's sandbox. A service is a long-running process
// (npm run dev, a Python server) the user sees live next to the chat through the preview
// origin, hot reload included. start_service returns a "service://<name>/" link
// (Output.Link, A-132); the host turns it into a URL on an origin of its own.
//
// Update (A-210): one "service" tool (action start | stop | list | logs) replaces
// start_service, stop_service, list_services and service_logs. When to run a service
// instead of a static preview lives in the live-preview skill. Risk and sandbox operation
// follow the action: list and logs are read-only reads; start and stop are drafts that run
// commands (exec). The definition states the worst case.
// ---------------------------------------------------------------------------

const (
	// startWait is how long start_service waits for the port before it answers.
	startWait = 30 * time.Second
	// startPoll is how often start_service looks at the service while it waits.
	startPoll = 500 * time.Millisecond
	// logTailBytes is how much of the log start_service shows.
	logTailBytes = 4000
	// serviceLogsDefaultBytes is what service_logs returns without max_bytes.
	serviceLogsDefaultBytes = 8000
)

// ellipsis marks output that was cut (U+2026), built from its code point so this source
// stays ASCII-only.
var ellipsis = string(rune(0x2026))

// serviceLink is the link of a service (its root page).
func serviceLink(name string) *kernel.ToolResultLink {
	return &kernel.ToolResultLink{URI: "service://" + name + "/", Name: name, Type: "service"}
}

func noServices(tool string) toolhub.Output {
	return argError(tool, "this agent's sandbox cannot run services "+emDash+" use preview for static pages")
}

// describeService is one line of list_services.
func describeService(s sandboxhub.ServiceInfo) string {
	state := string(s.Status)
	if s.Status == sandboxhub.ServiceExited && s.ExitCode != nil {
		state += " (code " + strconv.Itoa(*s.ExitCode) + ")"
	}
	return s.Name + " " + midDot + " " + state + " " + midDot + " port " + strconv.Itoa(s.Port) + " " + midDot + " " + s.Command
}

// logTail is the end of a service's output for start_service ("(no output)" when there
// is none or it cannot be read).
func logTail(ctx context.Context, svc sandboxhub.ServicesView, name string) string {
	logs, err := svc.ServiceLogs(ctx, name, logTailBytes)
	if err != nil || jsstr.Trim(logs.Text) == "" {
		return "(no output)"
	}
	prefix := ""
	if logs.Truncated {
		prefix = ellipsis
	}
	return prefix + jsstr.TrimEnd(logs.Text)
}

// sleepCtx waits d, or less when ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// serviceMeta is the metadata of start_service: name, port and status.
func serviceMeta(name string, info sandboxhub.ServiceInfo) ordered.Object {
	meta := ordered.NewObject()
	meta.Set("name", name)
	meta.Set("port", info.Port)
	meta.Set("status", string(info.Status))
	return meta
}

// trimmedString is the trimmed string argument key ("" when absent or not a string).
func trimmedString(a toolkit.Args, key string) string {
	s, _ := a.String(key)
	return jsstr.Trim(s)
}

// ---------------------------------------------------------------------------
// service (A-210)
// ---------------------------------------------------------------------------

// serviceActions are the actions of the service tool (A-210), in order.
var serviceActions = []string{"start", "stop", "list", "logs"}

// ServiceActions returns the actions of the service tool, in order (a fresh slice).
func ServiceActions() []string { return append([]string(nil), serviceActions...) }

// ServiceActionOf is the call's action, trimmed and lower-cased; "" when it is not one of
// ServiceActions.
func ServiceActionOf(args kernel.Arguments) string {
	action := strings.ToLower(trimmedString(toolkit.DecodeArgs(args), "action"))
	for _, a := range serviceActions {
		if a == action {
			return a
		}
	}
	return ""
}

// readsOnly reports whether a service action only reads (list, logs).
func readsOnly(action string) bool { return action == "list" || action == "logs" }

// ServiceTool is service: start, stop, list or read the logs of the agent's dev servers.
type ServiceTool struct{}

var (
	_ toolhub.Handler             = (*ServiceTool)(nil)
	_ toolhub.CallRisker          = (*ServiceTool)(nil)
	_ toolhub.CallSandboxOperator = (*ServiceTool)(nil)
)

// NewServiceTool returns the service tool.
func NewServiceTool() *ServiceTool { return &ServiceTool{} }

// Name returns the plugin name.
func (*ServiceTool) Name() string { return "tool:service" }

// Version returns the plugin version.
func (*ServiceTool) Version() string { return "1.0.0" }

// Definition returns the tool definition.
func (*ServiceTool) Definition() toolhub.Definition {
	str := func(desc string) toolhub.JSONSchema { return toolhub.JSONSchema{Type: "string", Description: desc} }
	return toolhub.Definition{
		Name: "service",
		Description: "Run long-running web servers in your workspace (a dev server with hot reload, an API, a Python server). " +
			"start: name and command, which gets its port in $PORT and must listen on it on 127.0.0.1 " +
			`(e.g. "npx vite --host 127.0.0.1 --port $PORT"); it keeps running and the user sees it live next to the chat. ` +
			"stop: name. list: your services and their state. logs: the end of a service's output. For plain HTML files " +
			"use preview instead; the live-preview skill says which to use.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "action", Schema: toolhub.Enum("start, stop, list or logs", serviceActions...)},
				toolhub.Property{Name: "name", Schema: str(`start/stop/logs: short name, lower-case letters, digits and dashes, e.g. "web"`)},
				toolhub.Property{Name: "command", Schema: str("start: shell command that starts the server (use $PORT)")},
				toolhub.Property{Name: "cwd", Schema: str("start: folder to run it in (default: your working directory)")},
				toolhub.Property{Name: "port", Schema: toolhub.JSONSchema{Type: "number", Description: "start: only when the server cannot take $PORT, the fixed port it listens on"}},
				toolhub.Property{Name: "max_bytes", Schema: toolhub.JSONSchema{Type: "number", Description: "logs: how much of the end to return (default " + strconv.Itoa(serviceLogsDefaultBytes) + ")"}},
			),
			Required: []string{"action"},
		},
		Risk:             toolhub.RiskDraft,
		SandboxOperation: sandboxhub.OperationExec,
		// A-160: in the session's runner folder when the user attached one (the link carries the session).
		SessionWorkspace: true,
	}
}

// RiskFor is the risk of one call: list and logs are read-only; start, stop and an
// unknown action are drafts (the worst case).
func (*ServiceTool) RiskFor(args kernel.Arguments) toolhub.Risk {
	if readsOnly(ServiceActionOf(args)) {
		return toolhub.RiskReadOnly
	}
	return toolhub.RiskDraft
}

// SandboxOperationFor is the sandbox operation of one call: list and logs only read;
// start, stop and an unknown action run commands.
func (*ServiceTool) SandboxOperationFor(args kernel.Arguments) sandboxhub.Operation {
	if readsOnly(ServiceActionOf(args)) {
		return sandboxhub.OperationFSRead
	}
	return sandboxhub.OperationExec
}

// Execute runs the action.
func (t *ServiceTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	action := ServiceActionOf(args)
	if action == "" {
		return argError("service", `"action" must be one of `+strings.Join(serviceActions, ", ")), nil
	}
	label := "service " + action
	a := toolkit.DecodeArgs(args)
	name := trimmedString(a, "name")
	if action != "list" && name == "" {
		return argError(label, `"name" is required`), nil
	}
	command := trimmedString(a, "command")
	if action == "start" && command == "" {
		return argError(label, `"command" is required`), nil
	}
	return withSandbox(ctx, tc, label, func(env sandboxhub.View) (toolhub.Output, error) {
		svc, ok := sandboxhub.AsServicesView(env)
		if !ok {
			return noServices(label), nil
		}
		switch action {
		case "start":
			spec := sandboxhub.ServiceSpec{Name: name, Command: command, Cwd: trimmedString(a, "cwd")}
			// Strict-schema models (OpenAI) fill every field: an empty string, 0 or [] means not given.
			if p, ok := a.Number("port"); ok && p > 0 {
				port := int(math.Floor(p))
				spec.Port = &port
			}
			return startService(ctx, svc, label, spec)
		case "stop":
			return stopService(ctx, svc, label, name)
		case "list":
			return listServices(ctx, svc)
		default:
			maxBytes := serviceLogsDefaultBytes
			if m, ok := a.Number("max_bytes"); ok && m > 0 {
				maxBytes = int(math.Min(math.Floor(m), math.MaxInt32))
			}
			return serviceLogs(ctx, svc, name, maxBytes)
		}
	}), nil
}

// startService starts the service and waits (up to 30 s) until it listens or exits.
func startService(ctx context.Context, svc sandboxhub.ServicesView, label string, spec sandboxhub.ServiceSpec) (toolhub.Output, error) {
	name := spec.Name
	info, err := svc.StartService(ctx, spec)
	if err != nil {
		return toolhub.Output{}, err
	}
	deadline := time.Now().Add(startWait)
	for info.Status == sandboxhub.ServiceStarting && time.Now().Before(deadline) && ctx.Err() == nil {
		sleepCtx(ctx, startPoll)
		list, err := svc.ListServices(ctx)
		if err != nil {
			return toolhub.Output{}, err
		}
		for _, s := range list {
			if s.Name == name {
				info = s
			}
		}
	}
	if info.Status == sandboxhub.ServiceExited {
		code := ""
		if info.ExitCode != nil {
			code = " with code " + strconv.Itoa(*info.ExitCode)
		}
		return toolkit.Failure(label+": "+name+" exited"+code+" before it listened on port "+strconv.Itoa(info.Port)+
			". Its output:\n"+logTail(ctx, svc, name), false, serviceMeta(name, info)), nil
	}
	link := serviceLink(name)
	var content string
	if info.Status == sandboxhub.ServiceListening {
		content = name + " is running on port " + strconv.Itoa(info.Port) + " " + emDash + " the user sees it live next to the chat." + toolhub.CiteLinkNote(*link)
	} else {
		content = name + " started but is not listening on port " + strconv.Itoa(info.Port) + " yet (still starting after " +
			strconv.Itoa(int(startWait/time.Second)) + " s). Check it with action list or logs. Its output so far:\n" +
			logTail(ctx, svc, name) + toolhub.CiteLinkNote(*link)
	}
	out := toolkit.Result(content, serviceMeta(name, info))
	out.Link = link
	return out, nil
}

// stopService stops the service.
func stopService(ctx context.Context, svc sandboxhub.ServicesView, label, name string) (toolhub.Output, error) {
	stopped, err := svc.StopService(ctx, name)
	if err != nil {
		return toolhub.Output{}, err
	}
	if stopped == nil {
		return argError(label, `there is no service named "`+name+`" `+emDash+` see action list`), nil
	}
	meta := ordered.NewObject()
	meta.Set("name", name)
	meta.Set("status", string(stopped.Status))
	return toolkit.Result("Stopped "+name+".", meta), nil
}

// listServices lists the services.
func listServices(ctx context.Context, svc sandboxhub.ServicesView) (toolhub.Output, error) {
	services, err := svc.ListServices(ctx)
	if err != nil {
		return toolhub.Output{}, err
	}
	meta := ordered.NewObject()
	meta.Set("count", len(services))
	if len(services) == 0 {
		return toolkit.Result("No services. Start one with action start.", meta), nil
	}
	lines := make([]string, 0, len(services))
	list := make([]any, 0, len(services))
	for _, s := range services {
		lines = append(lines, describeService(s))
		item := ordered.NewObject()
		item.Set("name", s.Name)
		item.Set("status", string(s.Status))
		item.Set("port", s.Port)
		list = append(list, item)
	}
	meta.Set("services", list)
	noun := "services"
	if len(services) == 1 {
		noun = "service"
	}
	return toolkit.Result(strconv.Itoa(len(services))+" "+noun+":\n"+strings.Join(lines, "\n"), meta), nil
}

// serviceLogs reads the end of the log.
func serviceLogs(ctx context.Context, svc sandboxhub.ServicesView, name string, maxBytes int) (toolhub.Output, error) {
	logs, err := svc.ServiceLogs(ctx, name, maxBytes)
	if err != nil {
		return toolhub.Output{}, err
	}
	text := jsstr.TrimEnd(logs.Text)
	content := name + " printed nothing yet."
	if text != "" {
		if logs.Truncated {
			content = ellipsis + text
		} else {
			content = text
		}
	}
	meta := ordered.NewObject()
	meta.Set("name", name)
	meta.Set("truncated", logs.Truncated)
	return toolkit.Result(content, meta), nil
}
