package fleet

import (
	"context"
	"io"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// Instance is a candidate Host as discovered (FL-001 to FL-005). For the
// static provider the EC2-specific fields are empty and Type is "static".
type Instance struct {
	// ID is the EC2 instance id, or the configured name for static hosts.
	ID string
	// Type is the EC2 instance type; it has to end in ".metal" (FL-003).
	Type string
	// Arch is from the instance attributes (FL-004).
	Arch config.Architecture
	// PrivateIP is the Host endpoint address unless overridden (FL-005).
	PrivateIP string
	// State is the EC2 instance state; only "running" is provisioned
	// (FL-001).
	State string
	// Tags are the EC2 tags, for the Host name and labels.
	Tags map[string]string
	// VCPU and MemoryMB are the raw instance capacity before the Host
	// reserve (FL-061).
	VCPU     int
	MemoryMB int
}

// Discovery finds candidate instances. Implementations: EC2 by tag or id
// (FL-001, FL-002) and static from configuration (TD-044). Filtering out
// non-metal instances (FL-003) happens in the caller so that every provider
// is treated alike.
type Discovery interface {
	Discover(ctx context.Context) ([]Instance, error)
}

// Script is one unit of remote work. Secrets travel in Stdin, never in the
// content or the command line (SE-014, SE-015).
type Script struct {
	// Name identifies the script in logs and in the fake SSM's record.
	Name string
	// Content is a bash script that passes `bash -n` and shellcheck
	// (TD-043).
	Content string
	// Stdin is fed to the script; nil means empty.
	Stdin io.Reader
	// Timeout bounds the run.
	Timeout time.Duration
}

// RunResult is the outcome of one Script on one instance.
type RunResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Remote executes Scripts on instances. Implementations: Systems Manager Run
// Command (FL-010) and SSH (FL-011). Output is streamed to out prefixed with
// the instance id as it arrives (FL-014); the result carries the full text.
type Remote interface {
	Run(ctx context.Context, inst Instance, s Script, out io.Writer) (*RunResult, error)
}

// DescribeFilter is the narrowed DescribeInstances request (FL-001, FL-002).
type DescribeFilter struct {
	TagKey      string
	TagValue    string
	InstanceIDs []string
	// States filters by instance state; empty means running only.
	States []string
}

// EC2 is the narrow EC2 interface (TD-040). The IAM policy it needs is
// ec2:DescribeInstances and, only for teardown --terminate,
// ec2:TerminateInstances (SE-040, FL-082).
type EC2 interface {
	DescribeInstances(ctx context.Context, f DescribeFilter) ([]Instance, error)
	TerminateInstances(ctx context.Context, ids []string) error
}

// SendCommandInput is the narrowed SendCommand request. The fake records it
// whole so that tests assert on the script content (TD-042).
type SendCommandInput struct {
	InstanceIDs []string
	// Script is the rendered script content. Stdin is not available over
	// Run Command, so secrets for the SSM path come from Parameters on the
	// Host side (SE-015, FL-091).
	Script  string
	Comment string
	Timeout time.Duration
}

// Invocation is the narrowed GetCommandInvocation response.
type Invocation struct {
	CommandID  string
	InstanceID string
	// Status is the SSM status string: Pending, InProgress, Success, Failed,
	// TimedOut, Cancelled.
	Status       string
	ResponseCode int
	Stdout       string
	Stderr       string
}

// SSM is the narrow Systems Manager command interface (TD-040, TD-041,
// SE-040).
type SSM interface {
	SendCommand(ctx context.Context, in SendCommandInput) (commandID string, err error)
	GetCommandInvocation(ctx context.Context, commandID, instanceID string) (*Invocation, error)
	ListCommandInvocations(ctx context.Context, commandID string) ([]Invocation, error)
}

// Parameters reads Systems Manager parameters, always decrypted (TD-041,
// FL-091, FL-104, FL-113, SE-040). Values are secrets (SE-010).
type Parameters interface {
	GetParameter(ctx context.Context, name string) (string, error)
}

// SSMRecorder is implemented by the fake SSM so that tests read back every
// SendCommand (TD-042).
type SSMRecorder interface {
	SentCommands() []SendCommandInput
}

// Step is one provisioning step, each backed by one script (FL-020 to
// FL-046, FL-050, FL-100 to FL-112). Steps are idempotent (FL-023, FL-030).
type Step string

// Provisioning steps in execution order.
const (
	StepDetect       Step = "detect"        // FL-029, FL-030: installed versions, thin pool presence
	StepFlintlock    Step = "flintlock"     // FL-020, FL-021, FL-027: host provisioner unattended
	StepThinPool     Step = "thin_pool"     // FL-022, FL-023
	StepNetworking   Step = "networking"    // FL-040 to FL-046, FL-108, SE-030 to SE-032
	StepFlintlockd   Step = "flintlockd"    // FL-024 to FL-026
	StepPoolAgent    Step = "pool_agent"    // FL-050
	StepHostServices Step = "host_services" // FL-100 to FL-107, FL-111, SE-050 to SE-052
	StepPrepull      Step = "prepull"       // FL-028
	StepPrewarm      Step = "prewarm"       // FL-112
	StepVerifyActive Step = "verify_active" // FL-053
	StepControlNode  Step = "control_node"  // FL-051, FL-055: Pool Manager daemon on the Control Node
	StepDrain        Step = "drain"         // FL-080, FL-084
	StepTeardown     Step = "teardown"      // FL-081 to FL-083
	StepUserData     Step = "user_data"     // FL-090, FL-091
	StepGuestVerify  Step = "guest_verify"  // FL-109: runs inside a verification MicroVM
)

// RenderInput is everything a script template may read. Secrets are not in
// it; they go through Script.Stdin or Parameters (SE-014, SE-015).
type RenderInput struct {
	Fleet        config.Fleet
	HostServices config.HostServices
	Profiles     []config.Profile
	Instance     Instance
	// Inventory is the current Inventory, for peer addresses (FL-046) and
	// the Pool Manager host list (FL-051).
	Inventory Inventory
	// Options are step-specific flags such as terminate and purge (FL-082,
	// FL-083).
	Options map[string]string
}

// Scripts renders the provisioning scripts. All lists every Step so that CI
// renders each with a representative input and runs `bash -n` and
// shellcheck (TD-043).
type Scripts interface {
	Render(step Step, in RenderInput) (Script, error)
	All() []Step
}

// StepResult is the outcome of one Step on one instance.
type StepResult struct {
	Step     Step
	Skipped  bool
	Duration time.Duration
	Err      error
}

// HostResult is the outcome of provisioning one instance (FL-013).
type HostResult struct {
	Instance Instance
	// Entry is the Inventory entry produced, nil on failure (FL-060,
	// FL-110).
	Entry *config.HostEntry
	// UpToDate is true when no step changed anything (FL-030).
	UpToDate bool
	Steps    []StepResult
	Err      error
}

// Provisioner provisions one instance end to end. The caller runs it over
// many instances with the configured parallelism and collects every failure
// (FL-012, FL-013).
type Provisioner interface {
	Provision(ctx context.Context, inst Instance) HostResult
}

// Inventory is the file the Fleet Controller writes and the Runner reads
// (FL-060, FL-063, FL-064).
type Inventory struct {
	Hosts       []config.HostEntry
	GeneratedAt time.Time
}

// InventoryStore reads and writes the Inventory file (FL-060) and the
// generated Runner configuration (FL-062).
type InventoryStore interface {
	Load(ctx context.Context, path string) (*Inventory, error)
	Save(ctx context.Context, path string, inv *Inventory) error
	// SaveRunnerConfig writes the Runner configuration that references the
	// Inventory, names the Pool Manager endpoint and carries the Profiles
	// (FL-062).
	SaveRunnerConfig(ctx context.Context, path string, cfg *config.Config) error
}

// ControlNode installs and reloads the Pool Manager daemon on the machine
// running the Fleet Controller (FL-051, FL-055).
type ControlNode interface {
	// InstallPoolManager installs the pinned daemon with a host list
	// generated from inv, naming every Host as the Inventory does (FL-052).
	InstallPoolManager(ctx context.Context, inv *Inventory) error
	// ReloadPoolManager regenerates the host list and reloads the daemon
	// without interrupting Leases (FL-055).
	ReloadPoolManager(ctx context.Context, inv *Inventory) error
}

// HostCert is one Host's certificate and key (SE-023).
type HostCert struct {
	CertFile string
	KeyFile  string
}

// CertBundle is the CA and per-Host certificates (FL-024, SE-023).
type CertBundle struct {
	CAFile string
	Hosts  map[string]HostCert
}

// CertificateAuthority supplies TLS material: the configured files when
// given, otherwise generated with owner-only permissions (SE-023).
type CertificateAuthority interface {
	Ensure(ctx context.Context, dir string, hosts []Instance) (*CertBundle, error)
}

// ServiceCheck is the outcome of one Host Service check from inside a
// verification MicroVM (FL-109).
type ServiceCheck struct {
	Service string
	Err     error
}

// HostVerification is one Host's verification (FL-070, FL-072, FL-073,
// FL-109).
type HostVerification struct {
	Host string
	// Info is ServerInfo; ExecEnabled is what FL-070 checks.
	Info        *flintlock.HostInfo
	ExecEnabled bool
	// Exercised is true when a MicroVM on this Host ran the trivial command
	// within the verification timeout (FL-072, FL-073).
	Exercised    bool
	ClaimToReady time.Duration
	Services     []ServiceCheck
}

// PoolVerification is one Pool's verification (FL-070).
type PoolVerification struct {
	Pool         poolmgr.PoolRef
	TargetSize   int32
	Available    int32
	AtTargetSize bool
}

// Failure is one named failure for the non-zero exit summary (FL-013,
// FL-071).
type Failure struct {
	Host string
	Step Step
	Err  error
}

// VerifyReport is the output of `fleet verify`.
type VerifyReport struct {
	Hosts    []HostVerification
	Pools    []PoolVerification
	Failures []Failure
}

// Verifier is `fleet verify` (FL-070 to FL-073, FL-109, FL-054). It claims
// MicroVMs through poolmgr.Lease and runs commands through
// transport.Factory, the same paths the Runner uses.
type Verifier interface {
	Verify(ctx context.Context, inv *Inventory) (*VerifyReport, error)
}

// Drainer is `fleet drain` (FL-080, FL-084).
type Drainer interface {
	// Drain removes host from every Pool's flintlock_hosts and waits until
	// no leased MicroVM remains on it or timeout elapses. It never stops
	// flintlockd while a Lease remains.
	Drain(ctx context.Context, host string, timeout time.Duration) error
}

// TeardownOptions are the explicit flags of `fleet teardown` (FL-082,
// FL-083).
type TeardownOptions struct {
	Terminate bool
	Purge     bool
}

// Teardown is `fleet teardown` (FL-081 to FL-083).
type Teardown interface {
	Teardown(ctx context.Context, inv *Inventory, opts TeardownOptions) error
}

// UserDataEmitter is `fleet emit-userdata` (FL-090, FL-091). The script
// reads its secrets from the configured Systems Manager parameters at first
// boot (SE-014).
type UserDataEmitter interface {
	Emit(ctx context.Context) ([]byte, error)
}

// Deps are the Fleet Controller's dependencies. Every field has a fake.
type Deps struct {
	Discovery  Discovery
	Remote     Remote
	EC2        EC2
	SSM        SSM
	Parameters Parameters
	Scripts    Scripts
	Inventory  InventoryStore
	Certs      CertificateAuthority
	Control    ControlNode
	// PoolManager, Hosts and Transports are used by verify, drain and
	// teardown. Hosts is an AdminDialer because verification is the one
	// Runner-side command allowed to hold a PoolHostClient.
	PoolManager poolmgr.Client
	Hosts       flintlock.AdminDialer
	Transports  transport.Factory
	// Out receives streamed command output (FL-014).
	Out io.Writer
}
