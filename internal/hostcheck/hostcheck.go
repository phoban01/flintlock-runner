// Package hostcheck holds the checks a component running on a Host of a
// cluster fleet makes of the Host itself: that a flintlockd endpoint is
// local (HI-042), what the units of the Host Image report through the
// not-ready-reason contract (HI-011), and whether a Host Service accepts
// connections. The Pod Provider (internal/kubelet) and the Exec Agent
// (internal/agent) both make them, and they are here so that the two agree
// on the contract with the Host Image while both exist.
package hostcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultNotReadyDir is the directory of the not-ready-reason contract; see
// ReadNotReadyReasons.
const DefaultNotReadyDir = "/run/flr/not-ready.d"

// maxReasonLength caps one not ready reason.
const maxReasonLength = 256

// DialTimeout bounds one Host Service probe. The service is on the Host's
// own bridge, so anything slower than this is not working.
const DialTimeout = time.Second

// unixScheme prefixes a unix socket endpoint, as gRPC writes them.
const unixScheme = "unix://"

// ValidateLocalEndpoint accepts exactly the two shapes HI-042 allows: a unix
// socket, written `unix://` and an absolute path, or a literal loopback IP
// address and a port. A host name is refused even when it is `localhost`,
// because what it resolves to is not this function's to know; so is every
// other scheme, the unspecified address and any address of another machine.
// The flintlockd of a Host has no authentication in a cluster fleet, so a
// client pointed anywhere else would be talking to it in the clear.
func ValidateLocalEndpoint(endpoint string) error {
	if endpoint == "" {
		return errors.New("is required")
	}
	if path, ok := strings.CutPrefix(endpoint, unixScheme); ok {
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("%q: a unix socket endpoint needs an absolute path", endpoint)
		}
		return nil
	}
	if strings.Contains(endpoint, "://") || strings.HasPrefix(endpoint, "unix:") || strings.HasPrefix(endpoint, "dns:") {
		return fmt.Errorf("%q: only unix:// and a loopback address:port are local endpoints", endpoint)
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return fmt.Errorf("%q: not unix:// and not an address:port: %w", endpoint, err)
	}
	if port == "" {
		return fmt.Errorf("%q: the port is missing", endpoint)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%q: the host has to be a literal loopback address, not a name", endpoint)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("%q: %s is not a loopback address; flintlockd is reached only on the Host itself", endpoint, host)
	}
	return nil
}

// DialTCP succeeds when addr accepts a connection within DialTimeout.
func DialTCP(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// NotReadyReason is one reason a unit of the Host Image reported.
type NotReadyReason struct {
	// Unit is the name of the file, by convention the reporting unit.
	Unit string
	// Reason is the first line of the file.
	Reason string
}

// ReadNotReadyReasons reads the not-ready-reason contract between the Host
// Image and the component that reports the Host's readiness (HI-011):
//
//   - The directory is by default /run/flr/not-ready.d. It is on a tmpfs, so
//     a reboot clears it, and the Host Agent mounts it read-only.
//   - Every regular file in it is one reason for the Host not being ready.
//     The file's name says who reports it, by convention the systemd unit,
//     for example `flintlock-kvm-check.service`; names starting with a dot
//     are ignored so that a writer can create a temporary file and rename it
//     into place.
//   - The first line of the file is the reason in words, for example `KVM is
//     unavailable: /dev/kvm is absent`. An empty file reports `not ready`.
//   - A unit removes its file when the condition clears. An absent or empty
//     directory means no unit has anything to report.
//
// A directory that exists and cannot be read is an error, which the caller
// reports as not ready: nothing guesses that a Host is fine.
func ReadNotReadyReasons(dir string) ([]NotReadyReason, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the not ready reasons: %w", err)
	}
	var out []NotReadyReason
	for _, entry := range entries {
		if !entry.Type().IsRegular() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue // removed between the listing and the read
		}
		if err != nil {
			return nil, fmt.Errorf("reading the not ready reason of %s: %w", entry.Name(), err)
		}
		line, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
		line = strings.TrimSpace(line)
		if line == "" {
			line = "not ready"
		}
		if len(line) > maxReasonLength {
			line = line[:maxReasonLength]
		}
		out = append(out, NotReadyReason{Unit: entry.Name(), Reason: line})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Unit < out[j].Unit })
	return out, nil
}
