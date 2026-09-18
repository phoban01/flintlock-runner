package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/scripts"
)

// StepReachability is the Control Node's TCP check of a Host's flintlockd
// port after provisioning (FL-047). It runs no script.
const StepReachability fleet.Step = "reachability"

// DefaultDialTimeout bounds the FL-047 reachability check.
const DefaultDialTimeout = 10 * time.Second

// ErrKVMUnavailable is wrapped by the error of an instance on which the
// detect step found no usable /dev/kvm (FL-117). Provision stops such an
// instance before any other step and returns no Inventory entry for it;
// errors.Is tells it apart from a failed step.
var ErrKVMUnavailable = errors.New("unsupported because KVM is unavailable")

// kvmUnavailable is the FL-117 error with detect's reason.
func kvmUnavailable(reason string) error {
	if reason == "" {
		reason = "the detect step reported no usable /dev/kvm"
	}
	return fmt.Errorf("%w: %s; a virtualized instance type needs nested virtualization enabled", ErrKVMUnavailable, reason)
}

// withHostCapacity returns inst with the vCPU and memory the detect step
// measured on the Host, which the Inventory entry's capacity is computed
// from (FL-061). A figure detect did not report keeps the discovered value.
func withHostCapacity(inst fleet.Instance, detected scripts.Output) fleet.Instance {
	if detected.VCPU > 0 {
		inst.VCPU = detected.VCPU
	}
	if detected.MemoryMB > 0 {
		inst.MemoryMB = detected.MemoryMB
	}
	return inst
}

// hostSteps are the provisioning steps in the order they run on a Host.
// The thin pool comes before the flintlock host provisioner so that
// containerd starts with its devicemapper pool present.
var hostSteps = []fleet.Step{
	fleet.StepDetect,
	fleet.StepThinPool,
	fleet.StepFlintlock,
	fleet.StepNetworking,
	fleet.StepFlintlockd,
	fleet.StepHostServices,
	fleet.StepPrepull,
	fleet.StepPrewarm,
	fleet.StepVerifyActive,
}

// HostSteps returns the steps Provision runs on a Host, in order.
func HostSteps() []fleet.Step { return append([]fleet.Step(nil), hostSteps...) }

// Options configure a Provisioner.
type Options struct {
	// Scripts renders the step scripts; Remote runs them (FL-010, FL-011).
	Scripts fleet.Scripts
	Remote  fleet.Remote
	// Parameters reads the Host Service credentials named in the
	// configuration so that they reach the Host on standard input. When
	// nil, the scripts read the parameters on the Host instead (FL-104,
	// FL-113).
	Parameters fleet.Parameters
	// Fleet, HostServices and Profiles are the configuration sections the
	// scripts are rendered from. Fleet has had config.ApplyDefaults applied.
	Fleet        config.Fleet
	HostServices config.HostServices
	Profiles     []config.Profile
	// Inventory returns the current Inventory, for the peer addresses the
	// guest firewall blocks (FL-046). Nil means an empty Inventory.
	Inventory func() fleet.Inventory
	// Certs is the TLS material from the CertificateAuthority. Required
	// unless fleet.flintlockd.insecure is set (FL-024).
	Certs *fleet.CertBundle
	// Out receives the scripts' streamed output (FL-014).
	Out io.Writer
	// Dial is used for the reachability check; nil uses net.Dialer.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// DialTimeout bounds the reachability check; zero means
	// DefaultDialTimeout.
	DialTimeout time.Duration
	// ReadFile reads the certificate files; nil uses os.ReadFile.
	ReadFile func(name string) ([]byte, error)
}

// OptionsFrom builds Options from the configuration and the Fleet
// Controller's dependencies: deps.Scripts, deps.Remote, deps.Parameters
// (optional) and deps.Out. certs is the bundle deps.Certs.Ensure returned,
// nil for an insecure fleet; inv returns the current Inventory for the peer
// addresses (FL-046) and may be nil.
func OptionsFrom(cfg *config.Config, deps fleet.Deps, certs *fleet.CertBundle, inv func() fleet.Inventory) Options {
	o := Options{
		Scripts:      deps.Scripts,
		Remote:       deps.Remote,
		Parameters:   deps.Parameters,
		HostServices: cfg.HostServices,
		Profiles:     cfg.Profiles,
		Inventory:    inv,
		Certs:        certs,
		Out:          deps.Out,
	}
	if cfg.Fleet != nil {
		o.Fleet = *cfg.Fleet
	}
	return o
}

// Provisioner implements fleet.Provisioner over fleet.Remote.
type Provisioner struct {
	o Options
}

var _ fleet.Provisioner = (*Provisioner)(nil)

// New returns a Provisioner.
func New(o Options) (*Provisioner, error) {
	if o.Scripts == nil || o.Remote == nil {
		return nil, errors.New("provision: Scripts and Remote are required")
	}
	if o.Certs == nil && !o.Fleet.Flintlockd.Insecure {
		return nil, errors.New("provision: TLS material is required unless fleet.flintlockd.insecure is set")
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Dial == nil {
		d := &net.Dialer{}
		o.Dial = d.DialContext
	}
	if o.DialTimeout == 0 {
		o.DialTimeout = DefaultDialTimeout
	}
	if o.ReadFile == nil {
		o.ReadFile = os.ReadFile
	}
	if o.Inventory == nil {
		o.Inventory = func() fleet.Inventory { return fleet.Inventory{} }
	}
	return &Provisioner{o: o}, nil
}

// Provision runs every Host step on inst and returns its Inventory entry.
// A failed step ends the instance's provisioning; the caller carries on
// with the other instances (FL-013). The first step, detect, decides
// whether the instance can be a Host at all: without a usable /dev/kvm it
// ends there with ErrKVMUnavailable (FL-117). Its vCPU and memory replace
// the discovered ones in the result's Instance and in the entry (FL-061).
func (p *Provisioner) Provision(ctx context.Context, inst fleet.Instance) fleet.HostResult {
	res := fleet.HostResult{Instance: inst}
	in := fleet.RenderInput{
		Fleet:        p.o.Fleet,
		HostServices: p.o.HostServices,
		Profiles:     p.o.Profiles,
		Instance:     inst,
		Inventory:    p.o.Inventory(),
	}
	var detected scripts.Output
	changed := false
	flintlockRan := false
	for _, step := range hostSteps {
		start := time.Now()
		sr := fleet.StepResult{Step: step}
		switch {
		//= docs/requirements/06-fleet.md#host-provisioning
		//# If the configured thin pool already exists on an instance, then
		//# the Fleet Controller SHALL NOT recreate it or wipe its device.
		case step == fleet.StepThinPool && detected.ThinPool:
			sr.Skipped = true
		//= docs/requirements/06-fleet.md#host-provisioning
		//# When run against an instance that is already provisioned at the
		//# pinned versions, the Fleet Controller SHALL make no changes to that
		//# instance and report it as up to date.
		case step == fleet.StepFlintlock && atPinned(detected.Versions, p.o.Fleet.Versions):
			sr.Skipped = true
		default:
			out, err := p.run(ctx, step, in)
			sr.Err = err
			if step == fleet.StepDetect && err == nil {
				detected = out
				//= docs/requirements/06-fleet.md#discovery
				//# If provisioning finds no usable `/dev/kvm` on an instance, then
				//# the Fleet Controller SHALL exclude it from the Inventory and report it as
				//# unsupported because KVM is unavailable.
				if !out.KVM {
					sr.Err = kvmUnavailable(out.KVMUnavailable)
				}
				//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
				//# The Fleet Controller SHALL compute each Host's capacity as the
				//# instance's vCPU and memory minus the configured Host reserve.
				inst = withHostCapacity(inst, out)
				in.Instance, res.Instance = inst, inst
			}
			if step == fleet.StepFlintlock && err == nil {
				flintlockRan = true
			}
			if len(out.Changed) > 0 {
				changed = true
			}
		}
		sr.Duration = time.Since(start)
		res.Steps = append(res.Steps, sr)
		if errors.Is(sr.Err, ErrKVMUnavailable) {
			// Stopped before anything was installed; no entry, so the
			// instance stays out of the Inventory.
			res.Err = fmt.Errorf("%s (%s): %w", inst.ID, inst.Type, sr.Err)
			return res
		}
		if sr.Err != nil {
			res.Err = fmt.Errorf("%s: step %s: %w", inst.ID, step, sr.Err)
			return res
		}
	}

	//= docs/requirements/06-fleet.md#host-provisioning
	//# The Fleet Controller SHALL record the installed versions of
	//# flintlock, Firecracker, Cloud Hypervisor and containerd in the Inventory.
	versions := detected.Versions
	if flintlockRan {
		// The provisioner changed what is installed: detect again, so that
		// the Inventory records what is on the Host rather than the pins.
		start := time.Now()
		out, err := p.run(ctx, fleet.StepDetect, in)
		res.Steps = append(res.Steps, fleet.StepResult{Step: fleet.StepDetect, Duration: time.Since(start), Err: err})
		if err != nil {
			res.Err = fmt.Errorf("%s: step %s: %w", inst.ID, fleet.StepDetect, err)
			return res
		}
		versions = out.Versions
	}

	entry, err := p.entry(inst, versions)
	if err != nil {
		res.Err = fmt.Errorf("%s: %w", inst.ID, err)
		return res
	}

	start := time.Now()
	err = p.reachable(ctx, entry.Endpoint)
	res.Steps = append(res.Steps, fleet.StepResult{Step: StepReachability, Duration: time.Since(start), Err: err})
	if err != nil {
		res.Err = fmt.Errorf("%s: %w", inst.ID, err)
		return res
	}
	res.Entry = entry
	res.UpToDate = !changed
	return res
}

// run renders and runs one step and fails it on a non-zero exit.
func (p *Provisioner) run(ctx context.Context, step fleet.Step, in fleet.RenderInput) (scripts.Output, error) {
	sc, err := p.o.Scripts.Render(step, in)
	if err != nil {
		return scripts.Output{}, err
	}
	secrets, err := p.secrets(ctx, step, in.Instance)
	if err != nil {
		return scripts.Output{}, err
	}
	if len(secrets) > 0 {
		sc.Stdin = scripts.EncodeSecrets(secrets)
	}
	if sc.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sc.Timeout)
		defer cancel()
	}
	rr, err := p.o.Remote.Run(ctx, in.Instance, sc, p.o.Out)
	if err != nil {
		return scripts.Output{}, err
	}
	out := scripts.ParseOutput(rr.Stdout)
	if rr.ExitCode != 0 {
		//= docs/requirements/06-fleet.md#pool-manager-install
		//# The Fleet Controller SHALL verify that each installed service is
		//# active before reporting an instance as provisioned.
		if step == fleet.StepVerifyActive && len(out.Inactive) > 0 {
			return out, fmt.Errorf("services not active: %s", strings.Join(out.Inactive, ", "))
		}
		return out, fmt.Errorf("exit status %d: %s", rr.ExitCode, lastLine(rr.Stderr))
	}
	return out, nil
}

// secrets are what each step reads from standard input (SE-015). Nothing
// here is ever put in the script content or a command line.
func (p *Provisioner) secrets(ctx context.Context, step fleet.Step, inst fleet.Instance) (map[string][]byte, error) {
	switch step {
	case fleet.StepFlintlock, fleet.StepFlintlockd:
		s := map[string][]byte{}
		if tok := p.o.Fleet.Flintlockd.Token; tok != "" {
			s[scripts.SecretFlintlockdToken] = []byte(tok)
		}
		if p.o.Fleet.Flintlockd.Insecure {
			return s, nil
		}
		hc, ok := p.o.Certs.Hosts[inst.ID]
		if !ok {
			return nil, fmt.Errorf("no certificate for %s in the certificate bundle", inst.ID)
		}
		for name, file := range map[string]string{
			scripts.SecretTLSCA: p.o.Certs.CAFile, scripts.SecretTLSCert: hc.CertFile, scripts.SecretTLSKey: hc.KeyFile,
		} {
			b, err := p.o.ReadFile(file)
			if err != nil {
				return nil, fmt.Errorf("read TLS material: %w", err)
			}
			s[name] = b
		}
		return s, nil
	case fleet.StepHostServices:
		if p.o.Parameters == nil {
			return nil, nil
		}
		s := map[string][]byte{}
		hs := p.o.HostServices
		if hs.GoProxy.IsEnabled() && len(hs.GoProxy.PrivatePatterns) > 0 && hs.GoProxy.CredentialParameter != "" {
			v, err := p.o.Parameters.GetParameter(ctx, hs.GoProxy.CredentialParameter)
			if err != nil {
				return nil, fmt.Errorf("read the Go module proxy credential parameter: %w", err)
			}
			s[scripts.SecretGoProxyCredential] = []byte(v)
		}
		if hs.RegistryMirror.IsEnabled() {
			for i, u := range hs.RegistryMirror.Upstreams {
				if u.CredentialParameter == "" {
					continue
				}
				v, err := p.o.Parameters.GetParameter(ctx, u.CredentialParameter)
				if err != nil {
					return nil, fmt.Errorf("read the credential parameter for %s: %w", u.URL, err)
				}
				s[scripts.SecretRegistryPrefix+strconv.Itoa(i)] = []byte(v)
			}
		}
		return s, nil
	}
	return nil, nil
}

// entry builds the instance's Inventory entry.
func (p *Provisioner) entry(inst fleet.Instance, versions config.InstalledVersions) (*config.HostEntry, error) {
	port := p.o.Fleet.Flintlockd.Port
	if port == 0 {
		port = config.DefaultFlintlockdPort
	}
	endpoint := net.JoinHostPort(inst.PrivateIP, strconv.Itoa(port))
	if o, ok := p.o.Fleet.EndpointOverrides[inst.ID]; ok && o != "" {
		endpoint = o
		if _, _, err := net.SplitHostPort(o); err != nil {
			endpoint = net.JoinHostPort(o, strconv.Itoa(port))
		}
	}
	svc, err := scripts.ServiceAddresses(p.o.HostServices, p.o.Fleet.GuestSubnet)
	if err != nil {
		return nil, err
	}
	e := &config.HostEntry{
		// FL-052: the Host's name is its instance id, here and in the Pool
		// Manager's host list, which is generated from these entries.
		Name:     inst.ID,
		Endpoint: endpoint,
		Arch:     inst.Arch,
		VCPU:     max(inst.VCPU-p.o.Fleet.HostReserve.VCPU, 0),
		MemoryMB: max(inst.MemoryMB-p.o.Fleet.HostReserve.MemoryMB, 0),
		Services: svc,
		Versions: versions,
	}
	if len(inst.Tags) > 0 {
		e.Labels = make(map[string]string, len(inst.Tags))
		for k, v := range inst.Tags {
			e.Labels[k] = v
		}
	}
	if !p.o.Fleet.Flintlockd.Insecure && p.o.Certs != nil {
		e.TLS.CAFile = p.o.Certs.CAFile
	} else {
		e.TLS.Insecure = true
	}
	return e, nil
}

//= docs/requirements/06-fleet.md#host-networking
//# The Fleet Controller SHALL verify TCP reachability from the
//# Control Node to each Host's `flintlockd` port after provisioning and SHALL
//# report any Host that is unreachable together with the security group
//# rule that would be needed.

// reachable dials the Host's flintlockd endpoint from the Control Node.
func (p *Provisioner) reachable(ctx context.Context, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, p.o.DialTimeout)
	defer cancel()
	conn, err := p.o.Dial(ctx, "tcp", endpoint)
	if err == nil {
		return conn.Close()
	}
	host, port, _ := net.SplitHostPort(endpoint)
	source := "the Control Node's address or security group"
	if c, uerr := net.Dial("udp", endpoint); uerr == nil {
		// A UDP "dial" sends nothing; it only picks the source address.
		if la, ok := c.LocalAddr().(*net.UDPAddr); ok {
			source = la.IP.String() + "/32 (the Control Node) or its security group"
		}
		_ = c.Close()
	}
	return fmt.Errorf("flintlockd at %s is unreachable from the Control Node (%v); "+
		"the security group of %s needs an inbound rule allowing TCP port %s from %s", endpoint, err, host, port, source)
}

// atPinned reports whether every flintlock component is at its pinned
// version.
func atPinned(have config.InstalledVersions, want config.PinnedVersions) bool {
	return have.Flintlock != "" && have.Flintlock == want.Flintlock &&
		have.Firecracker == want.Firecracker &&
		cloudHypervisorAtPin(have.CloudHypervisor, want.CloudHypervisor) &&
		have.Containerd == want.Containerd
}

// cloudHypervisorAtPin reports whether the installed Cloud Hypervisor
// version matches the pinned one. Its own --version output carries one
// more version component than its release tag (tag v41.0 installs and
// self-reports as v41.0.0), so an exact match would never be true; this
// tolerates exactly that difference without masking a genuine mismatch.
func cloudHypervisorAtPin(have, want string) bool {
	return have == want || have == want+".0"
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if s == "" {
		return "no output on stderr"
	}
	return s
}
