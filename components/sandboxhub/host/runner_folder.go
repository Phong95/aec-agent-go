package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// OpenRunnerFolder (A-153) is the folder the user chose, as a sandbox view for the
// client side of a runner (sandboxhub.ServeRunner): /workspace is the folder itself,
// with the host sandbox's confinement (logical path check, real path check, os.Root).
// It must be an existing absolute folder (it is never created). A nil limits means
// DefaultLimits.
func OpenRunnerFolder(ctx context.Context, folder string, limits *sandboxhub.Limits) (sandboxhub.View, error) {
	if folder == "" || !filepath.IsAbs(folder) {
		return nil, kernel.NewSandboxError(fmt.Sprintf("The runner needs an absolute folder (got %q)", folder), kernel.SandboxCodeInvalidPath, "")
	}
	base := filepath.Clean(folder)
	st, err := os.Stat(base) //nolint:gosec // the folder the user picked in the client app, never profile data
	if err != nil || !st.IsDir() {
		return nil, kernel.NewSandboxError(fmt.Sprintf("%q is not a folder", base), kernel.SandboxCodeInvalidPath, "")
	}
	l := DefaultLimits()
	if limits != nil {
		l = *limits
	}
	name := filepath.Base(base)
	if name == "" || name == string(filepath.Separator) || name == "." {
		name = base
	}
	traits := sandboxhub.Traits{
		Kind:       sandboxhub.KindHost,
		Isolated:   false,
		Network:    sandboxhub.NetworkFull,
		Persistent: true,
		Tenancy:    sandboxhub.TenancyOwn,
		OS:         hostOS(),
	}
	return newSandbox("runner:"+name, traits, l, base).ForAgent(ctx, "runner")
}
