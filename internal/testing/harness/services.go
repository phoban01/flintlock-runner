package harness

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

// serviceHost is the address the Host Service stand-ins listen on.
const serviceHost = "127.0.0.1"

// harnessServices are the Host Services that Options.HostServices makes
// available, by the name the Host Agent publishes them under (KF-194).
var harnessServices = []string{kubelabels.HostServiceBuildkit, kubelabels.HostServiceGoProxy}

// startServiceBackends listens on a loopback port for each Host Service,
// accepting and closing connections until Shutdown. Nothing is served: a
// Job only has to be told the address (EX-060, KF-189).
func (s *Stack) startServiceBackends() error {
	s.serviceBackends = map[string]int{}
	var listeners []net.Listener
	s.serviceClose = func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}
	for _, name := range harnessServices {
		l, err := net.Listen("tcp", net.JoinHostPort(serviceHost, "0"))
		if err != nil {
			return fmt.Errorf("harness: listening for host service %s: %w", name, err)
		}
		listeners = append(listeners, l)
		go func() {
			for {
				conn, err := l.Accept()
				if errors.Is(err, net.ErrClosed) {
					return
				}
				if err == nil {
					_ = conn.Close()
				}
			}
		}()
		s.serviceBackends[name] = l.Addr().(*net.TCPAddr).Port
		s.logf("host service %s stands in on %s", name, l.Addr())
	}
	return nil
}

// ServiceAddr is the address, host:port, of the named Host Service's
// stand-in, or empty when the Stack has none.
func (s *Stack) ServiceAddr(name string) string {
	port, ok := s.serviceBackends[name]
	if !ok {
		return ""
	}
	return net.JoinHostPort(serviceHost, strconv.Itoa(port))
}

// inventoryServices are the Host Service addresses of an Inventory entry,
// as the Fleet Controller writes them (FL-110).
func (s *Stack) inventoryServices() config.HostServiceAddresses {
	var out config.HostServiceAddresses
	if a := s.ServiceAddr(kubelabels.HostServiceBuildkit); a != "" {
		out.Buildkit = "tcp://" + a
	}
	if a := s.ServiceAddr(kubelabels.HostServiceGoProxy); a != "" {
		out.GoProxy = "http://" + a
	}
	return out
}
