// Package web holds the built-in web tools: the SSRF-guarded page fetch behind
// web_extract, the text parsers
// that turn a page into readable text, and the DuckDuckGo search provider behind
// web_search, and the two tools built on them: SearchTool (web_search) and ExtractTool
// (web_extract). The risky half is everything that touches the network or reads
// attacker-controlled text.
//
// Every page a tool fetches goes through one guarded path (fetchPage). The model's URL,
// and every Location header a server returns, is untrusted: the URL is parsed by a small
// WHATWG-compatible helper (url.go) so numeric hosts such as 2130706433 or 0x7f.1 are
// canonicalized before they are checked, then the pre-check (guard.go,
// assertPublicHTTPURL) refuses a non-http scheme, embedded credentials, localhost, a
// literal address and any host that resolves to a non-public address, and finally the
// transport (transport.go) re-validates the ip:port it actually dials, which closes the
// DNS-rebinding window a resolve-then-fetch check leaves open. Redirects are followed by
// hand, one guarded hop at a time, and the whole page (every hop and the body) shares one
// deadline derived from the tool's context.
//
// The DuckDuckGo endpoint is configured by the developer and is not SSRF-guarded; the
// provider still bounds the response body.
//
// The package is host-access guarded (tests/arch/hostaccess_test.go hostAccessGuarded) with a per-package
// exception for exactly net and net/http: it needs sockets and HTTP and nothing else. It
// reads no file, starts no process and imports no syscall, os or crypto/tls (the default
// client TLS configuration is used, and the dial-time check is a generic function, so it
// needs no syscall type). The names inside the package drop the package prefix
// (web.SearchTool, not WebSearchTool).
package web
