package preview

import (
	"context"
	"path"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// share_preview - a URL of a preview (a workspace page, an app with its file, a running
// service) the user can open outside the chat or send to someone. The other preview tools
// return link URIs only (A-132): the chat resolves them when the user clicks, and the
// model never sees a URL. This tool is the one exception, called when the user asks for
// a URL. The URL only reads the page's folder (widened to the files its query names, such
// as the model an app opens), so sharing a page does not share the whole workspace. It
// makes workspace files reachable outside the chat, so every call asks the user first.
// The host signs the URL: NewSharePreviewTool(sharer). The same texts and limits as
// core/ts.
//
// Update (A-210): the share_preview tool is gone - preview with public: true makes the
// URL (tools.go), and this file keeps the host contract (PreviewSharer) and the folder
// helpers. The folder is the page's, widened to the file an app opens; the query is no
// longer searched for files, since preview knows the app's file.
// ---------------------------------------------------------------------------

// SharePreviewRequest is what to share.
type SharePreviewRequest struct {
	// URI is the page: a "preview://<workspace path>[?query]" or "service://<name>/<path>" link (A-124).
	URI     string
	AgentID string
	// SessionID is the session whose workspace the page is in (A-160).
	SessionID string
	// Folder (preview:// only) is the workspace folder the URL may read, relative to the
	// working directory ("" = all of it). A file outside it is refused, so a shared page
	// does not open the rest of the workspace.
	Folder string
	// TTL is how long the URL should work. The host may cap it (the result says how long
	// it works).
	TTL time.Duration
}

// SharedPreview is a URL to open the page outside the chat.
type SharedPreview struct {
	URL string
	// ExpiresAt is when the URL stops working (ms since the epoch).
	ExpiresAt int64
	// Notes are what the user should know about the URL, e.g. that only this computer
	// can open it.
	Notes []string
}

// PreviewSharer is the host side of share_preview. Share returns an error whose message
// the model reads when no URL can be made (previews are off, the service stopped).
type PreviewSharer interface {
	Share(ctx context.Context, req SharePreviewRequest) (SharedPreview, error)
}

// CommonFolder is the deepest folder holding both workspace-relative folders ("" = the
// working directory).
func CommonFolder(a, b string) string {
	x := splitPath(a)
	y := splitPath(b)
	n := 0
	for n < len(x) && n < len(y) && x[n] == y[n] {
		n++
	}
	return strings.Join(x[:n], "/")
}

func splitPath(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r == '/' })
}

// folderInside reports whether workspace-relative p is folder or inside it.
func folderInside(folder, p string) bool {
	return folder == "" || p == folder || strings.HasPrefix(p, folder+"/")
}

// parentFolder is the folder of a workspace-relative path ("" = the working directory).
func parentFolder(p string) string {
	if dir := path.Dir(p); dir != "." {
		return dir
	}
	return ""
}
