package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
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
// this package is obtained via (*server).localClient(), which sets OmitAuth,
// and never via s.s.LocalClient() directly.
//
// Without OmitAuth, local.Client.DoLocalRequest calls
// safesocket.LocalTCPPortAndToken() on every request, which on macOS runs
// `lsof -c IPNExtension` — a fork+exec per LocalAPI call, useless for tsnet
// (see the localClient doc comment), and a crash under ASan. The regression
// shape this catches is a new or reverted call site going straight to
// s.s.LocalClient(): it compiles, it works, and it silently reintroduces the
// forks. A runtime assertion would only cover the call sites the test itself
// exercises; this covers all of them, including ones not yet written.
//
// Every non-test .go file in the package is parsed, not just tailscale.go —
// package main already has other files (netmon_android.go) and is the natural
// home for build-tagged ones, and a bypassing call there would compile, run,
// and leave a single-file check green. Build tags are deliberately NOT
// evaluated: a call guarded behind GOOS=android still needs the flag set.
//
// Scope limit worth stating, because a green run does not mean no forks:
// this can only see calls in THIS package. tsnet issues its own LocalAPI
// requests on the same shared client (tsnet.Server.Up, getCert), which is why
// the flag is primed in TsnetStart/TsnetUp rather than only at our call sites.
// No AST check over this package can catch a regression there.
func TestLocalAPIGoesThroughOmitAuthHelper(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package dir: %v", err)
	}

	var checked int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, 0) // 0 => comments dropped
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		// Walk the whole file, not just FuncDecls: a package-level
		// `var lc, _ = srv.s.LocalClient()` is a GenDecl and would otherwise
		// be invisible. Match on SelectorExpr rather than CallExpr.Fun so a
		// method value (`f := s.s.LocalClient; f()`) is caught too.
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "LocalClient" {
				return true
			}
			if isInsideLocalClientHelper(f, sel.Pos()) {
				return true
			}
			t.Errorf("%s: LocalClient() reached directly; use s.localClient() so OmitAuth is set",
				fset.Position(sel.Pos()))
			return true
		})
	}

	if checked == 0 {
		t.Fatal("no .go files found to check; the guard would pass vacuously")
	}
}

// isInsideLocalClientHelper reports whether pos falls inside the body of
// (*server).localClient — the one function allowed to call LocalClient().
//
// The receiver is checked, not just the name: a localClient() method added to
// some other type must not silently inherit the exemption.
func isInsideLocalClientHelper(f *ast.File, pos token.Pos) bool {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "localClient" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		ident, ok := star.X.(*ast.Ident)
		if !ok || ident.Name != "server" {
			continue
		}
		if pos >= fn.Pos() && pos <= fn.End() {
			return true
		}
	}
	return false
}
