// Package arch holds the module-wide architecture guards of the Go SDK: the layer
// table every package must be classified in, the vendor, host-access and wire-tag
// rules, the goleak TestMain rule and the go list -deps checks that keep the vendor
// SDKs and the host and Docker sandboxes out of the root. It has no code of its
// own; the tests run from the module root (see TestMain), so every path in them is
// module-relative.
package arch
