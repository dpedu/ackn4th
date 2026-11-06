package task

// Choosing a backend is a table lookup, not a chain of conditions.
//
// A process uses exactly one Backend, and it is chosen once, here. Every
// implementation the build ships is a row in `backends`: the kind it answers
// to, the URL scheme that selects it, and how to construct it. Adding another
// (a different transport, a shared filesystem, a queue) is adding a row plus a
// type that satisfies Backend — no caller changes, and nothing in
// internal/cli or internal/dash learns its name.
//
// Both rows ship the same implementation, the gRPC client. What differs is
// where the server is — in this process on a loopback socket, or on a
// controller another machine runs — so every CLI command and every dashboard
// run reaches state by one code path. See local.go for why reading the data
// dir directly was taken out of the middle of that path.
//
// Callers never choose. internal/cli calls OpenBackend once while loading and
// keeps the result in one field; internal/dash is handed the same value. No
// accessor or reader re-decides: there is no "if the backend is nil use the
// store" fallback behind a read, because that is a second, silent selection,
// and it is how a configured controller once ended up ignored while everything
// still compiled. (The dash fills an unset Options.State once, by construction,
// in newModel; that is a default for an embedder, not a choice at read time.)

import (
	"fmt"
	"strings"
)

// backendKind names an implementation in the selection table. It is the word a
// person can be shown when a URL names something this build cannot speak.
type backendKind string

const (
	// KindLocal is the default: state in this machine's data dir, served by
	// this process over a loopback socket and reached through the same client
	// a controller is.
	KindLocal backendKind = "local"
	// KindController is task state on a host reached over gRPC.
	KindController backendKind = "controller"
)

// backendRow is one implementation: what selects it, and how it is built.
type backendRow struct {
	kind backendKind
	// scheme is the URL scheme that selects this implementation. Empty for
	// the default row, which is selected by the absence of a URL.
	scheme string
	// bareHostPort accepts `host:port` with no scheme, which people type and
	// which a single-remote-host build can afford to mean.
	bareHostPort bool
	// open builds it. target is whatever the URL named (a host:port here);
	// dataDir is this machine's data dir, which every backend is given even
	// when it keeps its state elsewhere: it is where local artifacts live.
	open func(dataDir, target string) (Backend, error)
}

// backends is the selection table, in the order a person should read it: the
// default first, then the ones a URL selects.
var backends = []backendRow{
	{
		kind: KindLocal,
		// The default: this machine's data dir, served in this process. It is
		// the same implementation as the row below, so nothing above this
		// table can tell local from remote by its type.
		open: func(dataDir, _ string) (Backend, error) { return Local(dataDir) },
	},
	{
		kind:   KindController,
		scheme: "ackn",
		// A bare `host:port` is a controller: nothing else in this build
		// takes an address, so refusing it would only make people type a
		// scheme to say the same thing.
		bareHostPort: true,
		open: func(dataDir, target string) (Backend, error) {
			return OpenGRPC(target, dataDir)
		},
	},
}

// Select returns the backend the URL names: the local one for "", the
// controller for ackn://host:port (or a bare host:port), and an error naming
// the variable and the supported schemes for anything else. It is the single
// point at which the build decides which implementation a process uses.
func Select(url, dataDir string) (Backend, error) {
	url = strings.TrimSpace(url)
	row, target, err := rowFor(url)
	if err != nil {
		return nil, err
	}
	be, err := row.open(dataDir, target)
	if err != nil {
		if url == "" {
			// Nothing was configured, so the variable is not the thing to
			// blame: the local endpoint could not be opened.
			return nil, err
		}
		return nil, fmt.Errorf("%s=%q: %w", EnvBackendURL, url, err)
	}
	return be, nil
}

// rowFor picks the row a URL names. The URL is passed whole to the
// implementation as its target, so a scheme may carry more than a host:port
// without this function needing to understand it.
func rowFor(url string) (backendRow, string, error) {
	if url == "" {
		for _, r := range backends {
			if r.scheme == "" {
				return r, "", nil
			}
		}
		// Not reachable with the table above; fail loudly rather than
		// silently returning the zero Backend.
		return backendRow{}, "", fmt.Errorf("%s: no default backend in the selection table", EnvBackendURL)
	}
	scheme, _, hasScheme := strings.Cut(url, "://")
	for _, r := range backends {
		if r.scheme != "" && hasScheme && r.scheme == scheme {
			return r, url, nil
		}
	}
	if !hasScheme {
		for _, r := range backends {
			if r.bareHostPort {
				return r, url, nil
			}
		}
	}
	if hasScheme {
		return backendRow{}, "", fmt.Errorf("%s=%q: %s:// is not a backend this build has; it speaks %s", EnvBackendURL, url, scheme, schemeList())
	}
	return backendRow{}, "", fmt.Errorf("%s=%q: this build speaks %s", EnvBackendURL, url, schemeList())
}

// schemeList names the URL schemes in the table, for a refusal a person can act
// on: it is generated from the table so it cannot go stale.
func schemeList() string {
	var parts []string
	for _, r := range backends {
		if r.scheme != "" {
			parts = append(parts, r.scheme+"://host:port")
		}
	}
	return strings.Join(parts, ", ") + ", or unset for the local data dir"
}
