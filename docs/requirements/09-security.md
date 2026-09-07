# Security {#security}

This document specifies the isolation and secret-handling properties of the
Runner. The threat model assumes that Job scripts are untrusted: a Job runs
arbitrary code inside its MicroVM and has network access through the Host.
The Runner, the Hosts and the Control Node are trusted.

## Isolation {#isolation}

- **SE-001** The Runner SHALL NOT execute any part of a Job script on the
  Host operating system or on the Control Node.
- **SE-002** The Runner SHALL run every Job in a MicroVM that has not run a
  Job before and SHALL destroy or release that MicroVM when the Job ends.
- **SE-003** The Runner SHALL NOT mount any Host path into a MicroVM other
  than the volumes declared by the Profile.
- **SE-004** The Runner SHALL NOT expose the Guest Transport to any party
  other than the Runner process itself.

## Secrets {#secrets}

- **SE-010** The Runner SHALL NOT write the runner authentication token, Job
  tokens, Host tokens or Pool Manager credentials to any
  log, metric label, Job log or MicroVM metadata.
- **SE-011** The Runner SHALL deliver the Job token to the guest only inside
  the Stage scripts sent over the Guest Transport.
- **SE-012** The Runner SHALL NOT persist Job payloads, Job tokens or Stage
  scripts on the Control Node's filesystem.
- **SE-013** The Runner SHALL store its system identifier and any cached
  state in a directory readable only by the Runner's user.
- **SE-014** The Fleet Controller SHALL NOT embed secrets in cloud-init
  user-data, command lines visible to other processes or log output.
- **SE-015** The Fleet Controller SHALL pass tokens and certificates to Hosts
  through the remote execution channel's standard input or through Systems
  Manager parameters rather than as command arguments.

## Transport security {#transport-security}

- **SE-020** The Runner SHALL refuse a GitLab URL that is not `https` unless
  the configuration explicitly allows insecure GitLab access.
- **SE-021** The Runner SHALL refuse plaintext connections to Hosts and the
  Pool Manager unless each endpoint is explicitly marked insecure.
- **SE-022** The Runner SHALL verify server certificates for every TLS
  connection and SHALL NOT offer an option to skip verification.
- **SE-023** The Fleet Controller SHALL generate a certificate authority and
  per-Host certificates when none are supplied and SHALL store the private
  keys with owner-only permissions on the Control Node.

## Guest egress {#guest-egress}

- **SE-030** The Fleet Controller SHALL ensure that guests cannot reach the
  EC2 instance metadata service, so that Jobs cannot obtain the Host's
  instance role credentials.
- **SE-031** The Fleet Controller SHALL ensure that guests cannot reach the
  control ports of their own Host or of any other Host.
- **SE-032** Where an egress allow-list is configured, the Fleet Controller
  SHALL restrict guest outbound traffic to the listed destinations.

## Least privilege {#least-privilege}

- **SE-040** The Fleet Controller SHALL operate with an IAM policy limited to
  `ec2:DescribeInstances`, `ssm:SendCommand`, `ssm:GetCommandInvocation`,
  `ssm:ListCommandInvocations`, `ssm:GetParameter` and, only when the
  terminate flag is used, `ec2:TerminateInstances`.
- **SE-041** The Runner SHALL NOT require any AWS permission at run time
  unless launch template mode's Inventory refresh is enabled, in which case
  it SHALL require only `ec2:DescribeInstances`.
- **SE-042** The Fleet Controller SHALL document the security group rules it
  needs and SHALL NOT modify security groups itself.

## Host services {#host-services-security}

- **SE-050** The Fleet Controller SHALL run `buildkitd` as an unprivileged
  user so that a build escaping its sandbox does not gain root on the Host.
- **SE-051** Every Host Service SHALL be reachable only from the guest
  subnet of its own Host and from the Host itself, and SHALL NOT be
  reachable from other Hosts or from outside the Host.
- **SE-052** The Fleet Controller SHALL NOT place upstream registry or proxy
  credentials where a guest can read them, and SHALL configure the registry
  mirror and Go module proxy to hold them on the Host side only.
- **SE-053** The Executor SHALL NOT inject Host Service variables that would
  route a Job's traffic to a Host other than the one running its MicroVM.
- **SE-054** Where a Job builds images with `buildkitd`, the Runner SHALL
  document that the layer cache is shared between Jobs on the same Host and
  that a Job can read layers another Job on that Host produced.
- **SE-055** The credential the Go module proxy uses for private modules
  SHALL be read-only and SHALL grant access to no groups or projects beyond
  those covered by the configured private module patterns.
- **SE-056** The Runner SHALL document that private modules cached by a
  Host's Go module proxy are readable by every Job that runs on that Host.
- **SE-057** The Go module proxy SHALL NOT use a Job's token to fetch
  modules.

The shared layer cache in SE-054 and the shared private module cache in
SE-056 are the same trust boundary as a shared runner host with a Docker
socket, narrowed to one bare-metal host; teams that need per-job isolation
run `buildkitd` inside their job image or set `GOPRIVATE` themselves, and
lose the warm cache for that job.
