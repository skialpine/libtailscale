package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/tailscale/libtailscale/tsnetctest"
)

func TestConn(t *testing.T) {
	tsnetctest.RunTestConn(t)

	// RunTestConn cleans up after itself, so there shouldn't be
	// anything left in the global maps.

	servers.mu.Lock()
	rem := len(servers.m)
	servers.mu.Unlock()

	if rem > 0 {
		t.Fatalf("want no remaining tsnet objects, got %d", rem)
	}

	var remConns, remLns int

	for i := 0; i < 50; i++ {
		conns.mu.Lock()
		remConns = len(conns.m)
		conns.mu.Unlock()

		listeners.mu.Lock()
		remLns = len(listeners.m)
		listeners.mu.Unlock()

		if remConns == 0 && remLns == 0 {
			break
		}

		// We are waiting for cleanup goroutines to finish.
		//
		// libtailscale closes one side of a socketpair and
		// then Go responds to the other side being unreadable
		// by closing the connections and listeners.
		//
		// This is inherently asynchronous.
		// Without ditching the standard close(2) and having our
		// own close functions.
		//
		// So we spin for a while
		time.Sleep(100 * time.Millisecond)
	}

	if remConns > 0 {
		t.Errorf("want no remaining tsnet_conn objects, got %d", remConns)
	}

	if remLns > 0 {
		t.Errorf("want no remaining tsnet_listener objects, got %d", remLns)
	}
}

func TestExtractIP(t *testing.T) {
	ipv4 := "1.23.33.4:12343"
	ipv6 := "[1::2234::34fc::44]:56576"

	got4 := extractIP(ipv4)
	got6 := extractIP(ipv6)

	want4 := "1.23.33.4"
	want6 := "[1::2234::34fc::44]"

	if got4 != want4 {
		t.Errorf("ipv4 port stripping failed")
	}

	if got6 != want6 {
		t.Errorf("ipv6 port stripping failed %s != %s", got6, want6)
	}
}

// TestLocalAPIGoesThroughOmitAuthHelper asserts that every LocalAPI client in
// this file is obtained via (*server).localClient(), which sets OmitAuth, and
// never via s.s.LocalClient() directly.
//
// Without OmitAuth, local.Client.DoLocalRequest calls
// safesocket.LocalTCPPortAndToken() on every request, which on macOS runs
// `lsof -c IPNExtension` — a fork+exec per LocalAPI call, useless for tsnet
// (see the localClient doc comment), and a crash under ASan. The regression
// shape this catches is a new or reverted call site going straight to
// s.s.LocalClient(): it compiles, it works, and it silently reintroduces the
// forks. A runtime assertion would only cover the call sites the test itself
// exercises; this covers all of them, including ones not yet written.
func TestLocalAPIGoesThroughOmitAuthHelper(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "tailscale.go", nil, 0) // 0 => comments dropped
	if err != nil {
		t.Fatalf("parsing tailscale.go: %v", err)
	}

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name == "localClient" { // the one legitimate caller
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "LocalClient" {
				return true
			}
			t.Errorf("%s: %s calls LocalClient() directly; use s.localClient() so OmitAuth is set",
				fset.Position(call.Pos()), fn.Name.Name)
			return true
		})
	}
}
