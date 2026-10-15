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
// start_service
// ---------------------------------------------------------------------------

// StartServiceTool is start_service: start a dev server and show it live.
type StartServiceTool struct{}

var _ toolhub.Handler = (*StartServiceTool)(nil)

// NewStartServiceTool returns the start_service tool.
func NewStartServiceTool() *StartServiceTool { return &StartServiceTool{} }

// Name returns the plugin name.
func (*StartServiceTool) Name() string { return "tool:start-service" }

// Version returns the plugin version.
func (*StartServiceTool) Version() string { return "1.0.0" }

// Definition returns the tool definition.
func (*StartServiceTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "start_service",
		Description: "Start a long-running web server in your workspace (a dev server with hot reload, an API, a Python server) " +
			"and show it to the user live next to the chat. The command gets its port in $PORT and must listen on it " +
			`on 127.0.0.1 ` + emDash + ` e.g. "npx vite --host 127.0.0.1 --port $PORT", "python3 -m http.server $PORT --bind 127.0.0.1". ` +
			"It keeps running after this call; edits to its files show up through its own reload. Your sandbox may have no " +
			"internet, so dependencies must already be installed. For plain HTML files use preview instead.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "name", Schema: toolhub.JSONSchema{Type: "string", Description: `Short name, lower-case letters, digits and dashes, e.g. "web"`}},
				toolhub.Property{Name: "command", Schema: toolhub.JSONSchema{Type: "string", Description: "Shell command that starts the server (use $PORT)"}},
				toolhub.Property{Name: "cwd", Schema: toolhub.JSONSchema{Type: "string", Description: "Folder to run it in (default: your working directory)"}},
				toolhub.Property{Name: "port", Schema: toolhub.JSONSchema{Type: "number", Description: "Only when the server cannot take $PORT: the fixed port it listens on"}},
			),
			Required: []string{"name", "command"},
		},
		Risk:             toolhub.RiskDraft,
		SandboxOperation: sandboxhub.OperationExec,
	}
}

// Execute starts the service and waits (up to 30 s) until it listens or exits.
func (*StartServiceTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	name := trimmedString(a, "name")
	command := trimmedString(a, "command")
	if name == "" {
		return argError("start_service", `"name" is required`), nil
	}
	if command == "" {
		return argError("start_service", `"command" is required`), nil
	}
	spec := sandboxhub.ServiceSpec{Name: name, Command: command, Cwd: trimmedString(a, "cwd")}
	if p, ok := a.Number("port"); ok {
		port := int(math.Floor(p))
		spec.Port = &port
	}
	return withSandbox(ctx, tc, "start_service", func(env sandboxhub.View) (toolhub.Output, error) {
		svc, ok := sandboxhub.AsServicesView(env)
		if !ok {
			return noServices("start_service"), nil
		}
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
			return toolkit.Failure("start_service: "+name+" exited"+code+" before it listened on port "+strconv.Itoa(info.Port)+
				". Its output:\n"+logTail(ctx, svc, name), false, serviceMeta(name, info)), nil
		}
		link := serviceLink(name)
		var content string
		if info.Status == sandboxhub.ServiceListening {
			content = name + " is running on port " + strconv.Itoa(info.Port) + " " + emDash + " the user sees it live next to the chat." + toolhub.CiteLinkNote(*link)
		} else {
			content = name + " started but is not listening on port " + strconv.Itoa(info.Port) + " yet (still starting after " +
				strconv.Itoa(int(startWait/time.Second)) + " s). Check it with list_services / service_logs. Its output so far:\n" +
				logTail(ctx, svc, name) + toolhub.CiteLinkNote(*link)
		}
		out := toolkit.Result(content, serviceMeta(name, info))
		out.Link = link
		return out, nil
	}), nil
}

// ---------------------------------------------------------------------------
// stop_service
// ---------------------------------------------------------------------------

// StopServiceTool is stop_service: stop a running service.
type StopServiceTool struct{}

var _ toolhub.Handler = (*StopServiceTool)(nil)

// NewStopServiceTool returns the stop_service tool.
func NewStopServiceTool() *StopServiceTool { return &StopServiceTool{} }

// Name returns the plugin name.
func (*StopServiceTool) Name() string { return "tool:stop-service" }

// Version returns the plugin version.
func (*StopServiceTool) Version() string { return "1.0.0" }

// Definition returns the tool definition.
func (*StopServiceTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name:        "stop_service",
		Description: "Stop one of your running services (see list_services). Its preview stops working.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "name", Schema: toolhub.JSONSchema{Type: "string", Description: "The service's name"}},
			),
			Required: []string{"name"},
		},
		Risk:             toolhub.RiskDraft,
		SandboxOperation: sandboxhub.OperationExec,
	}
}

// Execute stops the service.
func (*StopServiceTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	name := trimmedString(toolkit.DecodeArgs(args), "name")
	if name == "" {
		return argError("stop_service", `"name" is required`), nil
	}
	return withSandbox(ctx, tc, "stop_service", func(env sandboxhub.View) (toolhub.Output, error) {
		svc, ok := sandboxhub.AsServicesView(env)
		if !ok {
			return noServices("stop_service"), nil
		}
		stopped, err := svc.StopService(ctx, name)
		if err != nil {
			return toolhub.Output{}, err
		}
		if stopped == nil {
			return argError("stop_service", `there is no service named "`+name+`" `+emDash+` see list_services`), nil
		}
		meta := ordered.NewObject()
		meta.Set("name", name)
		meta.Set("status", string(stopped.Status))
		return toolkit.Result("Stopped "+name+".", meta), nil
	}), nil
}

// ---------------------------------------------------------------------------
// list_services
// ---------------------------------------------------------------------------

// ListServicesTool is list_services: the agent's services and their state.
type ListServicesTool struct{}

var _ toolhub.Handler = (*ListServicesTool)(nil)

// NewListServicesTool returns the list_services tool.
func NewListServicesTool() *ListServicesTool { return &ListServicesTool{} }

// Name returns the plugin name.
func (*ListServicesTool) Name() string { return "tool:list-services" }

// Version returns the plugin version.
func (*ListServicesTool) Version() string { return "1.0.0" }

// Definition returns the tool definition.
func (*ListServicesTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name:        "list_services",
		Description: "List your services (started with start_service): name, state (starting, listening, exited), port and command.",
		Parameters: toolhub.JSONSchema{
			Type:       "object",
			Properties: toolhub.NewProperties(),
			Required:   []string{},
		},
		Risk:             toolhub.RiskReadOnly,
		SandboxOperation: sandboxhub.OperationFSRead,
	}
}

// Execute lists the services.
func (*ListServicesTool) Execute(ctx context.Context, _ kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	return withSandbox(ctx, tc, "list_services", func(env sandboxhub.View) (toolhub.Output, error) {
		svc, ok := sandboxhub.AsServicesView(env)
		if !ok {
			return noServices("list_services"), nil
		}
		services, err := svc.ListServices(ctx)
		if err != nil {
			return toolhub.Output{}, err
		}
		meta := ordered.NewObject()
		meta.Set("count", len(services))
		if len(services) == 0 {
			return toolkit.Result("No services. Start one with start_service.", meta), nil
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
	}), nil
}

// ---------------------------------------------------------------------------
// service_logs
// ---------------------------------------------------------------------------

// ServiceLogsTool is service_logs: the end of a service's output.
type ServiceLogsTool struct{}

var _ toolhub.Handler = (*ServiceLogsTool)(nil)

// NewServiceLogsTool returns the service_logs tool.
func NewServiceLogsTool() *ServiceLogsTool { return &ServiceLogsTool{} }

// Name returns the plugin name.
func (*ServiceLogsTool) Name() string { return "tool:service-logs" }

// Version returns the plugin version.
func (*ServiceLogsTool) Version() string { return "1.0.0" }

// Definition returns the tool definition.
func (*ServiceLogsTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name:        "service_logs",
		Description: "Read the end of a service's output (stdout and stderr) " + emDash + " to see why it fails or what it printed.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "name", Schema: toolhub.JSONSchema{Type: "string", Description: "The service's name"}},
				toolhub.Property{Name: "max_bytes", Schema: toolhub.JSONSchema{Type: "number", Description: "How much of the end to return (default 8000)"}},
			),
			Required: []string{"name"},
		},
		Risk:             toolhub.RiskReadOnly,
		SandboxOperation: sandboxhub.OperationFSRead,
	}
}

// Execute reads the end of the log.
func (*ServiceLogsTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	name := trimmedString(a, "name")
	if name == "" {
		return argError("service_logs", `"name" is required`), nil
	}
	maxBytes := serviceLogsDefaultBytes
	if m, ok := a.Number("max_bytes"); ok && m > 0 {
		maxBytes = int(math.Min(math.Floor(m), math.MaxInt32))
	}
	return withSandbox(ctx, tc, "service_logs", func(env sandboxhub.View) (toolhub.Output, error) {
		svc, ok := sandboxhub.AsServicesView(env)
		if !ok {
			return noServices("service_logs"), nil
		}
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
	}), nil
}
