// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// A Go c-archive of the tsnet package. See tailscale.h for details.
package main

//#include "errno.h"
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
	"tailscale.com/client/local"
	"tailscale.com/hostinfo"
	"tailscale.com/ipn"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
)

func main() {}

// servers tracks all the allocated *tsnet.Server objects.
var servers struct {
	mu   sync.Mutex
	next C.int
	m    map[C.int]*server
}

type server struct {
	s       *tsnet.Server
	lastErr string
	started bool

	omitAuthOnce sync.Once
}

// localClient returns the server's LocalAPI client with OmitAuth set.
//
// Every request through local.Client goes through DoLocalRequest, which by
// default calls safesocket.LocalTCPPortAndToken() to attach a Basic-Auth
// header (client/local/local.go). On macOS that resolves to
// readMacosSameUserProof(), which runs `lsof -n -a -u<uid> -c IPNExtension -F`
// — one fork+exec per LocalAPI request. Its purpose is to find the LocalAPI
// port and token of the sandboxed Mac App Store Tailscale GUI, which has
// nothing to do with us: tsnet serves its own LocalAPI over an in-process
// memnet listener and sets no RequiredPassword on that handler
// (tsnet/tsnet.go, where localClient is built as &local.Client{Dial: lal.Dial}).
// So the token is never checked, and on a Mac without the GUI app installed
// lsof matches nothing and the lookup fails anyway. Pure waste — and `lsof -u`
// walks every fd of every process owned by the user, so it is not cheap.
//
// It is also a crash: Decenza ships ASan-instrumented debug builds, and
// fork() in a multithreaded ASan process can inherit a permanently-locked
// allocator lock in the child ("BUG IN CLIENT OF LIBPLATFORM: os_unfair_lock
// is corrupt", "crashed on child side of fork pre-exec"). Same failure mode
// as the `dscl` exec that ts_omit_ssh fixed; see CLAUDE.md. TsnetGetAuthURL
// and TsnetGetBackendState are polled during connect, so this fired often.
//
// What justifies skipping auth is the RequiredPassword point above, not
// upstream's blessing: local.go's own comment on OmitAuth describes a
// narrower scenario than ours — "meant for when Dial is set and the LocalAPI
// is being proxied to a different operating system, such as in integration
// tests." Ours is in-process and same-OS. The mechanism is identical (skip a
// header nothing validates), but do not read that comment as upstream having
// sanctioned this exact use.
//
// Ordering, not locking, is what makes the write safe. sync.Once alone would
// NOT: it synchronizes callers of Do with each other, and the readers that
// matter never call it — tsnet publishes this same *local.Client to its own
// backend and reads OmitAuth from other goroutines (tsnet.Server.Up, getCert,
// ConfigureWebClient). A Do() concurrent with one of those is a plain data
// race on the field.
//
// So the flag is primed by primeLocalClient() from TsnetStart and TsnetUp,
// before the server can service any LocalAPI request, and the Once then
// guarantees the write happens exactly once and never again. Every subsequent
// read — ours or tsnet's — is ordered after it. Do not "simplify" this by
// dropping the priming calls and relying on the helper alone.
func (s *server) localClient() (*local.Client, error) {
	lc, err := s.s.LocalClient()
	if err != nil {
		return nil, err
	}
	s.omitAuthOnce.Do(func() { lc.OmitAuth = true })
	return lc, nil
}

// primeLocalClient sets OmitAuth before any LocalAPI request can be issued.
//
// Routing our own three call sites through localClient() is not enough:
// tsnet.Server.Up obtains the shared client itself and issues WatchIPNBus,
// Status and SetServeConfig on it, and Up runs before anything polls status.
// Without priming, those three requests fork lsof on the connect path — the
// exact crash this change exists to remove — and only later calls were fixed.
//
// Errors are deliberately swallowed: the caller is about to call Start/Up and
// will surface a real failure with better context. Priming is best-effort.
func (s *server) primeLocalClient() {
	_, _ = s.localClient()
}

func getServer(sd C.int) *server {
	servers.mu.Lock()
	defer servers.mu.Unlock()
	return servers.m[sd]
}

// listeners tracks all the tsnet_listener objects allocated via tsnet_listen.
var listeners struct {
	mu sync.Mutex
	m  map[C.int]*listener
}

type listener struct {
	s  *server
	ln net.Listener
	fd int // go side fd of socketpair sent to C
	mu sync.Mutex
	m  map[C.int]net.Addr //maps fds to remote addresses for lookup
}

// conns tracks all the pipe(2)s allocated via tsnet_dial.
var conns struct {
	mu sync.Mutex
	m  map[C.int]*conn // keyed by the FD given to C (w)
}

type conn struct {
	s *tsnet.Server
	c net.Conn
	r *os.File // r is the local socket to the C client
}

func (s *server) recErr(err error) C.int {
	if err == nil {
		s.lastErr = ""
		return 0
	}
	s.lastErr = err.Error()
	return -1
}

//export TsnetNewServer
func TsnetNewServer() C.int {
	servers.mu.Lock()
	defer servers.mu.Unlock()

	if servers.m == nil {
		servers.m = map[C.int]*server{}
		hostinfo.SetApp("libtailscale")
	}
	if servers.next == 0 {
		servers.next = 42<<16 + 1
	}
	sd := servers.next
	servers.next++
	s := &server{s: &tsnet.Server{}}
	servers.m[sd] = s
	return (C.int)(sd)
}

//export TsnetStart
func TsnetStart(sd C.int) C.int {
	s := getServer(sd)
	if s == nil {
		return C.EBADF
	}
	err := s.s.Start()
	if err == nil {
		s.started = true
		s.primeLocalClient() // before anything can issue a LocalAPI request
	}
	return s.recErr(err)
}

//export TsnetUp
func TsnetUp(sd C.int) C.int {
	s := getServer(sd)
	if s == nil {
		return C.EBADF
	}
	// Up starts the server itself if it isn't already, then issues LocalAPI
	// requests on the shared client. Prime first — this also starts it.
	s.primeLocalClient()
	_, err := s.s.Up(context.Background()) // cancellation is via TsnetClose
	if err == nil {
		s.started = true
	}
	return s.recErr(err)
}

//export TsnetClose
func TsnetClose(sd C.int) C.int {
	servers.mu.Lock()
	s := servers.m[sd]
	if s != nil {
		delete(servers.m, sd)
	}
	servers.mu.Unlock()

	if s == nil {
		return C.EBADF
	}

	// TODO: cancel Up
	// TODO: close related listeners / conns.
	if !s.started {
		// Server was never started, nothing to close.
		return 0
	}
	if err := s.s.Close(); err != nil {
		s.s.Logf("tailscale_close: failed with %v", err)
		return -1
	}

	return 0
}

//export TsnetGetIps
func TsnetGetIps(sd C.int, buf *C.char, buflen C.size_t) C.int {
	if buf == nil {
		panic("errmsg passed nil buf")
	} else if buflen == 0 {
		panic("errmsg passed buflen of 0")
	}

	servers.mu.Lock()
	s := servers.m[sd]
	servers.mu.Unlock()

	out := unsafe.Slice((*byte)(unsafe.Pointer(buf)), buflen)

	if s == nil {
		out[0] = '\x00'
		return C.EBADF
	}

	ip4, ip6 := s.s.TailscaleIPs()
	joined := strings.Join([]string{ip4.String(), ip6.String()}, ",")
	n := copy(out, joined)
	if n >= len(out) {
		out[len(out)-1] = '\x00' // always NUL-terminate
		return C.ERANGE
	}
	out[n] = '\x00'
	return 0
}

//export TsnetErrmsg
func TsnetErrmsg(sd C.int, buf *C.char, buflen C.size_t) C.int {
	if buf == nil {
		panic("errmsg passed nil buf")
	} else if buflen == 0 {
		panic("errmsg passed buflen of 0")
	}

	servers.mu.Lock()
	s := servers.m[sd]
	servers.mu.Unlock()

	out := unsafe.Slice((*byte)(unsafe.Pointer(buf)), buflen)
	if s == nil {
		out[0] = '\x00'
		return C.EBADF
	}
	n := copy(out, s.lastErr)
	if n >= len(out) {
		out[len(out)-1] = '\x00' // always NUL-terminate
		return C.ERANGE
	}
	out[n] = '\x00'
	return 0
}

//export TsnetListen
func TsnetListen(sd C.int, network, addr *C.char, listenerOut *C.int) C.int {
	s := getServer(sd)
	if s == nil {
		return C.EBADF
	}

	ln, err := s.s.Listen(C.GoString(network), C.GoString(addr))
	if err != nil {
		return s.recErr(err)
	}
	s.started = true

	// The tailscale_listener we return to C is one side of a socketpair(2).
	// We do this so we can proactively call ln.Accept in a goroutine and
	// feed an fd for the connection through the listener. This lets C use
	// epoll on the tailscale_listener to know if it should call
	// tailscale_accept, which avoids a blocking call on the far side.
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		return s.recErr(err)
	}
	sp := fds[1]
	fdC := C.int(fds[0])

	listeners.mu.Lock()
	if listeners.m == nil {
		listeners.m = map[C.int]*listener{}
	}
	listener := &listener{s: s, ln: ln, fd: sp, m: map[C.int]net.Addr{}}
	listeners.m[fdC] = listener
	listeners.mu.Unlock()

	cleanup := func() {
		// If fdC is closed on the C side, then we end up calling
		// into cleanup twice. Be careful to avoid syscall.Close
		// twice as the FD may have been reallocated.
		listeners.mu.Lock()
		if tsLn, ok := listeners.m[fdC]; ok && tsLn.ln == ln {
			delete(listeners.m, fdC)
			syscall.Close(sp)
		}
		listeners.mu.Unlock()

		ln.Close()
	}
	go func() {
		// fdC is never written to, so trying to read from sp blocks
		// until fdC is closed. We use this as a signal that C is
		// done with the listener, and we can tear it down.
		//
		// TODO: would using os.NewFile avoid a locked up thread?
		var buf [256]byte
		syscall.Read(sp, buf[:])
		cleanup()
	}()
	go func() {
		defer cleanup()
		for {
			netConn, err := ln.Accept()
			if err != nil {
				return
			}
			var connFd C.int
			if err := newConn(s, netConn, &connFd); err != nil {
				if s.s.Logf != nil {
					s.s.Logf("libtailscale.accept: newConn: %v", err)
				}
				netConn.Close()
				continue
			}
			rights := syscall.UnixRights(int(connFd))
			err = syscall.Sendmsg(sp, nil, rights, nil, 0)
			if err != nil {
				// We handle sp being closed in the read goroutine above.
				if s.s.Logf != nil {
					s.s.Logf("libtailscale.accept: sendmsg failed: %v", err)
				}
				netConn.Close()
				// fallthrough to close connFd, then continue Accept()ing
			}

			// map the connection to the remote address
			listener.mu.Lock()
			listener.m[connFd] = netConn.RemoteAddr()
			listener.mu.Unlock()

			syscall.Close(int(connFd)) // now owned by recvmsg
		}
	}()

	*listenerOut = fdC
	return 0
}

//export TsnetAccept
func TsnetAccept(listenerFd C.int, connOut *C.int) C.int {
	listeners.mu.Lock()
	ln := listeners.m[listenerFd]
	listeners.mu.Unlock()

	if ln == nil {
		return C.EBADF
	}

	buf := make([]byte, unix.CmsgLen(int(unsafe.Sizeof((C.int)(0)))))
	_, oobn, _, _, err := syscall.Recvmsg(int(listenerFd), nil, buf, 0)
	if err != nil {
		return ln.s.recErr(err)
	}

	scms, err := syscall.ParseSocketControlMessage(buf[:oobn])
	if err != nil {
		return ln.s.recErr(err)
	}
	if len(scms) != 1 {
		return ln.s.recErr(fmt.Errorf("libtailscale: got %d control messages, want 1", len(scms)))
	}
	fds, err := syscall.ParseUnixRights(&scms[0])
	if err != nil {
		return ln.s.recErr(err)
	}
	if len(fds) != 1 {
		return ln.s.recErr(fmt.Errorf("libtailscale: got %d FDs, want 1", len(fds)))
	}
	*connOut = (C.int)(fds[0])

	return 0
}

func newConn(s *server, netConn net.Conn, connOut *C.int) error {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		return err
	}
	r := os.NewFile(uintptr(fds[1]), "socketpair-r")
	c := &conn{s: s.s, c: netConn, r: r}
	fdC := C.int(fds[0])

	conns.mu.Lock()
	if conns.m == nil {
		conns.m = make(map[C.int]*conn)
	}
	conns.m[fdC] = c
	conns.mu.Unlock()

	connCleanup := func() {
		var inCleanup bool
		conns.mu.Lock()
		if tsConn, ok := conns.m[fdC]; ok && tsConn.c == netConn {
			delete(conns.m, fdC)
			inCleanup = true
		}
		conns.mu.Unlock()

		if !inCleanup {
			return
		}

		r.Close()
		netConn.Close()
	}
	go func() {
		defer connCleanup()
		var b [1 << 16]byte
		io.CopyBuffer(r, netConn, b[:])
		syscall.Shutdown(int(r.Fd()), syscall.SHUT_WR)
		if cr, ok := netConn.(interface{ CloseRead() error }); ok {
			cr.CloseRead()
		}
	}()
	go func() {
		defer connCleanup()
		var b [1 << 16]byte
		io.CopyBuffer(netConn, r, b[:])
		syscall.Shutdown(int(r.Fd()), syscall.SHUT_RD)
		if cw, ok := netConn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()

	*connOut = fdC
	return nil
}

//export TsnetGetRemoteAddr
func TsnetGetRemoteAddr(listener C.int, conn C.int, buf *C.char, buflen C.size_t) C.int {
	if buf == nil {
		panic("errmsg passed nil buf")
	} else if buflen == 0 {
		panic("errmsg passed buflen of 0")
	}
	out := unsafe.Slice((*byte)(unsafe.Pointer(buf)), buflen)

	listeners.mu.Lock()
	defer listeners.mu.Unlock()
	l := listeners.m[listener]
	if l == nil {
		out[0] = '\x00'
		return C.EBADF
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	addr, ok := l.m[conn]
	if !ok {
		out[0] = '\x00'
		return C.EBADF
	}

	ip := extractIP(addr.String())

	n := copy(out, ip)
	if n >= len(out) {
		out[len(out)-1] = '\x00' // always NUL-terminate
		return C.ERANGE
	}
	out[n] = '\x00'
	return 0
}

// Strips the port from connection IPs
func extractIP(ipWithPort string) string {
	re := regexp.MustCompile(`(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})|\[([0-9a-fA-F:]+)\]`)
	match := re.FindString(ipWithPort)
	return match
}

//export TsnetDial
func TsnetDial(sd C.int, network, addr *C.char, connOut *C.int) C.int {
	s := getServer(sd)
	if s == nil {
		return C.EBADF
	}
	netConn, err := s.s.Dial(context.Background(), C.GoString(network), C.GoString(addr))
	if err != nil {
		return s.recErr(err)
	}
	s.started = true
	if err := newConn(s, netConn, connOut); err != nil {
		return s.recErr(err)
	}
	return 0
}

//export TsnetSetDir
func TsnetSetDir(sd C.int, str *C.char) C.int {
	s := getServer(sd)
	if s == nil {
		return C.EBADF
	}
	s.s.Dir = C.GoString(str)
	return 0
}

//export TsnetSetHostname
func TsnetSetHostname(sd C.int, str *C.char) C.int {
	s := getServer(sd)
	if s == nil {
		return C.EBADF
	}
	s.s.Hostname = C.GoString(str)
	return 0
}

//export TsnetSetAuthKey
func TsnetSetAuthKey(sd C.int, str *C.char) C.int {
	s := getServer(sd)
	if s == nil {
		return C.EBADF
	}
	s.s.AuthKey = C.GoString(str)
	return 0
}

//export TsnetSetControlURL
func TsnetSetControlURL(sd C.int, str *C.char) C.int {
	s := getServer(sd)
	if s == nil {
		return C.EBADF
	}
	s.s.ControlURL = C.GoString(str)
	return 0
}

//export TsnetSetEphemeral
func TsnetSetEphemeral(sd C.int, e int) C.int {
	s := getServer(sd)
	if s == nil {
		return C.EBADF
	}
	if e == 0 {
		s.s.Ephemeral = false
	} else {
		s.s.Ephemeral = true
	}
	return 0
}

//export TsnetSetLogFD
func TsnetSetLogFD(sd, fd C.int) C.int {
	s := getServer(sd)
	if s == nil {
		return C.EBADF
	}
	if fd == -1 {
		s.s.Logf = logger.Discard
		return 0
	}
	f := os.NewFile(uintptr(fd), "logfd")
	s.s.Logf = func(format string, args ...any) {
		fmt.Fprintf(f, format, args...)
		fmt.Fprintf(f, "\n")
	}
	return 0
}

//export TsnetLoopback
func TsnetLoopback(sd C.int, addrOut *C.char, addrLen C.size_t, proxyOut *C.char, localOut *C.char) C.int {
	// Panic here to ensure we always leave the out values NUL-terminated.
	if addrOut == nil {
		panic("loopback_api passed nil addr_out")
	} else if addrLen == 0 {
		panic("loopback_api passed addrlen of 0")
	} else if proxyOut == nil {
		panic("loopback_api passed nil proxy_cred_out")
	} else if localOut == nil {
		panic("loopback_api passed nil local_api_cred_out")
	}

	// Start out NUL-termianted to cover error conditions.
	*addrOut = '\x00'
	*localOut = '\x00'
	*proxyOut = '\x00'

	s := getServer(sd)
	if s == nil {
		return C.EBADF
	}
	addr, proxyCred, localAPICred, err := s.s.Loopback()
	if err != nil {
		return s.recErr(err)
	}
	if len(proxyCred) != 32 {
		return s.recErr(fmt.Errorf("libtailscale: len(proxyCred)=%d, want 32", len(proxyCred)))
	}
	if len(localAPICred) != 32 {
		return s.recErr(fmt.Errorf("libtailscale: len(localAPICred)=%d, want 32", len(localAPICred)))
	}

	out := unsafe.Slice((*byte)(unsafe.Pointer(addrOut)), addrLen)
	n := copy(out, addr)
	if n >= len(out) {
		out[len(out)-1] = '\x00' // always NUL-terminate
		return C.ERANGE
	}
	out[n] = '\x00'

	// proxyOut and localOut are non-nil and 33 bytes long because
	// they are defined in C as char cred_out[static 33].
	out = unsafe.Slice((*byte)(unsafe.Pointer(proxyOut)), 33)
	copy(out, proxyCred)
	out[32] = '\x00'
	out = unsafe.Slice((*byte)(unsafe.Pointer(localOut)), 33)
	copy(out, localAPICred)
	out[32] = '\x00'

	return 0
}

//export TsnetStatusJSON
func TsnetStatusJSON(sd C.int, jsonOut **C.char) C.int {
	if jsonOut == nil {
		panic("status_json passed nil json_out")
	}
	*jsonOut = nil
	s := getServer(sd)
	if s == nil {
		return C.EBADF
	}
	// LocalClient rides tsnet's in-memory LocalAPI listener — unlike
	// Loopback()'s TCP listener it cannot be reclaimed by the OS while
	// the process is suspended (iOS), so status reads keep working on
	// long-lived nodes.
	lc, err := s.localClient()
	if err != nil {
		return s.recErr(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := lc.Status(ctx)
	if err != nil {
		return s.recErr(err)
	}
	b, err := json.Marshal(st)
	if err != nil {
		return s.recErr(err)
	}
	*jsonOut = C.CString(string(b))
	return 0
}

//export TsnetEnableFunnelToLocalhostPlaintextHttp1
func TsnetEnableFunnelToLocalhostPlaintextHttp1(sd C.int, localhostPort C.int) C.int {
	s := getServer(sd)
	if s == nil {
		return C.EBADF
	}

	ctx := context.Background()
	lc, err := s.localClient()
	if err != nil {
		return s.recErr(err)
	}

	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return s.recErr(err)
	}
	// Guard against indexing an empty slice: CertDomains is empty until the node
	// is up and has been issued a Funnel cert (login + funnel-attribute approval).
	// Without this, enabling Funnel too early panics and takes down the Go
	// runtime (and the embedding app) — return an error instead.
	if len(st.CertDomains) == 0 {
		return s.recErr(fmt.Errorf("libtailscale: no Funnel cert domain yet (node not up, or Funnel not approved for this tailnet)"))
	}
	domain := st.CertDomains[0]

	hp := ipn.HostPort(net.JoinHostPort(domain, strconv.Itoa(443)))
	tcpForward := fmt.Sprintf("127.0.0.1:%d", localhostPort)
	sc := &ipn.ServeConfig{
		TCP: map[uint16]*ipn.TCPPortHandler{
			443: {
				TCPForward:   tcpForward,
				TerminateTLS: domain,
			},
		},
		AllowFunnel: map[ipn.HostPort]bool{
			hp: true,
		},
	}

	// SetServeConfig's error is the only signal that Funnel was refused —
	// tailnet policy, Funnel not approved for the node, or an If-Match/ETag
	// conflict. This used to be discarded and replaced by a read-back of
	// sc.AllowFunnel[hp], which is our own map literal set to true a few lines
	// up and which SetServeConfig never mutates: the branch was dead and the
	// function returned success unconditionally, so a refused Funnel looked
	// like a working one to the C caller.
	if err := lc.SetServeConfig(ctx, sc); err != nil {
		return s.recErr(fmt.Errorf("libtailscale: failed to enable funnel: %w", err))
	}

	return 0
}

// writeCString copies str into the C buffer out, always NUL-terminating.
// Returns 0 on success or C.ERANGE if the buffer was too small.
func writeCString(str string, buf *C.char, buflen C.size_t) C.int {
	out := unsafe.Slice((*byte)(unsafe.Pointer(buf)), buflen)
	n := copy(out, str)
	if n >= len(out) {
		out[len(out)-1] = '\x00' // always NUL-terminate
		return C.ERANGE
	}
	out[n] = '\x00'
	return 0
}

//export TsnetGetCertDomain
func TsnetGetCertDomain(sd C.int, buf *C.char, buflen C.size_t) C.int {
	if buf == nil || buflen == 0 {
		panic("TsnetGetCertDomain passed nil buf or buflen of 0")
	}
	s := getServer(sd)
	if s == nil {
		out := unsafe.Slice((*byte)(unsafe.Pointer(buf)), buflen)
		out[0] = '\x00'
		return C.EBADF
	}
	// tsnet.Server.CertDomains() is the direct accessor (no LocalClient/status
	// round-trip). Empty until the node is up with a Funnel cert.
	domains := s.s.CertDomains()
	domain := ""
	if len(domains) > 0 {
		domain = domains[0]
	}
	return writeCString(domain, buf, buflen)
}

//export TsnetGetAuthURL
func TsnetGetAuthURL(sd C.int, buf *C.char, buflen C.size_t) C.int {
	if buf == nil || buflen == 0 {
		panic("TsnetGetAuthURL passed nil buf or buflen of 0")
	}
	s := getServer(sd)
	if s == nil {
		out := unsafe.Slice((*byte)(unsafe.Pointer(buf)), buflen)
		out[0] = '\x00'
		return C.EBADF
	}
	lc, err := s.localClient()
	if err != nil {
		return s.recErr(err)
	}
	st, err := lc.StatusWithoutPeers(context.Background())
	if err != nil {
		return s.recErr(err)
	}
	return writeCString(st.AuthURL, buf, buflen)
}

//export TsnetGetBackendState
func TsnetGetBackendState(sd C.int, buf *C.char, buflen C.size_t) C.int {
	if buf == nil || buflen == 0 {
		panic("TsnetGetBackendState passed nil buf or buflen of 0")
	}
	s := getServer(sd)
	if s == nil {
		out := unsafe.Slice((*byte)(unsafe.Pointer(buf)), buflen)
		out[0] = '\x00'
		return C.EBADF
	}
	lc, err := s.localClient()
	if err != nil {
		return s.recErr(err)
	}
	st, err := lc.StatusWithoutPeers(context.Background())
	if err != nil {
		return s.recErr(err)
	}
	return writeCString(st.BackendState, buf, buflen)
}
