package host

import "github.com/Phong95/aec-agent-go/components/sandboxhub"

// Options configures a Provider.
type Options struct {
	// Root is the absolute folder on the server machine that holds every host
	// sandbox. Set by the host, never by a profile.
	Root string
	// Limits overlays DefaultLimits.
	Limits sandboxhub.LimitsPatch
}
