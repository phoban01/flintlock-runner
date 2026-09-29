package main

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"syscall"
	"time"
)

// runLoopMetricsAddr is the address gitlab-runner's metrics server listens
// on. Its port is 0, so the kernel picks a free port when the run loop binds
// it. No port is chosen ahead of the bind, so no other process can take the
// port in between (#109).
const runLoopMetricsAddr = "127.0.0.1:0"

// runLoopMetricsHost is the host of runLoopMetricsAddr.
var runLoopMetricsHost = netip.AddrFrom4([4]byte{127, 0, 0, 1})

// followRunLoopMaxInterval bounds the backoff of followRunLoop.
const followRunLoopMaxInterval = 2 * time.Second

// loopbackListeners returns the address of each TCP socket of this process
// that listens on runLoopMetricsHost.
//
// gitlab-runner keeps its metrics listener in a local variable and logs the
// address it was given, not the address it bound. The process's own file
// descriptors are the only place that shows the bound port. /dev/fd lists
// them on Linux and on macOS. Only the names are read: on macOS the listing
// includes the descriptor that reads it, which is closed by the time a stat
// of each entry would reach it, so os.ReadDir fails there (#114).
func loopbackListeners() (map[netip.AddrPort]bool, error) {
	dir, err := os.Open("/dev/fd")
	if err != nil {
		return nil, fmt.Errorf("listing the process's file descriptors: %w", err)
	}
	names, err := dir.Readdirnames(-1)
	_ = dir.Close()
	if err != nil {
		return nil, fmt.Errorf("listing the process's file descriptors: %w", err)
	}
	out := map[netip.AddrPort]bool{}
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil || !isTCPListener(fd) {
			continue
		}
		sa, err := syscall.Getsockname(fd)
		if err != nil {
			continue
		}
		in4, ok := sa.(*syscall.SockaddrInet4)
		if !ok || netip.AddrFrom4(in4.Addr) != runLoopMetricsHost {
			continue
		}
		out[netip.AddrPortFrom(runLoopMetricsHost, uint16(in4.Port))] = true //nolint:gosec // a port fits in 16 bits
	}
	return out, nil
}

// isTCPListener reports whether fd is a stream socket that listens. A file
// descriptor that is not a socket, or that closed a moment ago, is not.
func isTCPListener(fd int) bool {
	typ, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil || typ != syscall.SOCK_STREAM {
		return false
	}
	// Linux answers 1 and macOS answers the option's bit.
	accepting, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN)
	return err == nil && accepting != 0
}

// newListeners returns the listeners in now that are not in before, in
// order.
func newListeners(before, now map[netip.AddrPort]bool) []netip.AddrPort {
	var added []netip.AddrPort
	for a := range now {
		if !before[a] {
			added = append(added, a)
		}
	}
	slices.SortFunc(added, func(a, b netip.AddrPort) int { return a.Compare(b) })
	return added
}

// followRunLoop waits for gitlab-runner's metrics server to listen, and
// then gives its address to the health server. before holds this process's
// loopback listeners from before the run loop started, so the run loop's
// listener is the one listener that is new. followRunLoop polls, from every
// up to followRunLoopMaxInterval, until it finds that listener or ctx ends.
// It does not guess: while more than one listener is new, it waits.
func (h *healthServer) followRunLoop(ctx context.Context, before map[netip.AddrPort]bool, every time.Duration) {
	warned := false
	for {
		now, err := loopbackListeners()
		var added []netip.AddrPort
		if err == nil {
			added = newListeners(before, now)
		}
		switch {
		case len(added) == 1:
			h.setRunLoop(added[0].String())
			h.log.Debug("found the run loop's metrics server", "address", added[0].String())
			return
		case err != nil && !warned:
			h.log.Warn("cannot find the run loop's metrics server; /metrics is not served yet", "error", err)
			warned = true
		case len(added) > 1 && !warned:
			h.log.Warn("more than one new loopback listener; waiting to tell which one is the run loop's metrics server",
				"listeners", fmt.Sprint(added))
			warned = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
		every = min(2*every, followRunLoopMaxInterval)
	}
}
