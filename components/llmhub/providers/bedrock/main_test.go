package bedrock

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package if any test leaves a goroutine running. The provider
// starts no goroutine of its own; a leak would mean an event stream, an HTTP
// connection or a fake stream was not closed.
func TestMain(m *testing.M) {
	// Go 1.27 net/http can leave the read loop of a connection whose response body
	// failed with a non-EOF error (a stream the test cancels or cuts off) blocked in an
	// unbuffered send of its new post-close drain (transport.go, eofc), until the
	// connection is torn down. That goroutine is the runtime's, not a leak of this
	// package; a really leaked connection waits in a read, a different top frame, and
	// is still reported.
	goleak.VerifyTestMain(m, goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"))
}
