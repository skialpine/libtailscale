// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build android

package main

import (
	"net"
	"strconv"

	"tailscale.com/net/netmon"
)

// Android (API 30+) forbids ordinary apps from reading the OS routing table
// over a netlink socket (RTM_GETLINK / RTM_GETADDR). Go's net.Interfaces()
// relies on exactly that, so tsnet's bring-up path
//
//	tsnet.Server.Start() -> netmon.New() -> getState() -> net.Interfaces()
//
// fails hard on-device with:
//
//	tsnet: route ip+net: netlinkrib: permission denied
//
// and the node never reaches NeedsLogin — no login URL, Mode A dead.
//
// Decenza runs tsnet purely in userspace / netstack mode (no VpnService, no
// route management); it only needs an outbound tailnet connection for the MCP
// Funnel, so the kernel routing table is genuinely unnecessary here.
//
// netmon exposes the officially-intended escape hatch for this exact situation
// (see the comment on netmon.netInterfaces / RegisterInterfaceGetter, which
// says it "lets us the Android app register an alternate implementation"):
// RegisterInterfaceGetter installs an alternate interface enumerator that
// netmon uses in place of net.Interfaces(). We register one that discovers the
// local address(es) without ever touching netlink — a UDP "connect" performs no
// I/O; it only asks the kernel to pick a source address for a route
// (connect(2) + getsockname(2)) — plus loopback. That is enough for the node to
// come up and reach control/DERP and expose the Funnel; direct connectivity
// still works when a primary address is discovered, and Funnel ingress relays
// through Tailscale regardless.
//
// This file is compiled only for GOOS=android (see the build tag above), so
// macOS/iOS/desktop keep the stock netlink/BSD-route path and are unaffected.
func init() {
	netmon.RegisterInterfaceGetter(androidInterfaces)
}

func androidInterfaces() ([]netmon.Interface, error) {
	// Loopback is always present. Reporting it keeps AnyInterfaceUp() true and
	// lets proxy detection run, matching a normal machine's baseline.
	ifaces := []netmon.Interface{{
		Interface: &net.Interface{
			Index: 1,
			Name:  "lo",
			Flags: net.FlagUp | net.FlagLoopback,
		},
		AltAddrs: []net.Addr{
			&net.IPNet{IP: net.IPv4(127, 0, 0, 1), Mask: net.CIDRMask(8, 32)},
			&net.IPNet{IP: net.IPv6loopback, Mask: net.CIDRMask(128, 128)},
		},
	}}

	// Discover the primary outbound source address(es) without netlink.
	idx := 2
	if ip := primaryOutboundIP("udp4", "8.8.8.8:53"); ip != nil {
		ifaces = append(ifaces, synthIface(idx, ip, net.CIDRMask(24, 32)))
		idx++
	}
	if ip := primaryOutboundIP("udp6", "[2001:4860:4860::8888]:53"); ip != nil {
		ifaces = append(ifaces, synthIface(idx, ip, net.CIDRMask(64, 128)))
		idx++
	}

	return ifaces, nil
}

// synthIface builds a synthetic "up" interface carrying ip. AltAddrs is set so
// netmon never calls (*net.Interface).Addrs(), which would hit netlink.
func synthIface(index int, ip net.IP, mask net.IPMask) netmon.Interface {
	return netmon.Interface{
		Interface: &net.Interface{
			Index: index,
			Name:  "net" + strconv.Itoa(index),
			Flags: net.FlagUp | net.FlagBroadcast | net.FlagMulticast,
		},
		AltAddrs: []net.Addr{&net.IPNet{IP: ip, Mask: mask}},
	}
}

// primaryOutboundIP returns the local source IP the kernel would use to reach
// remote, or nil if there's no route (e.g. no IPv6). The UDP "connect" sends no
// packets and never reads the netlink routing table, so it works on locked-down
// Android.
func primaryOutboundIP(network, remote string) net.IP {
	c, err := net.Dial(network, remote)
	if err != nil {
		return nil
	}
	defer c.Close()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || ua.IP == nil || ua.IP.IsUnspecified() {
		return nil
	}
	return ua.IP
}
