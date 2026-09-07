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
  tokens, Host tokens, Orchestrator tokens or Pool Manager credentials to any
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
- **SE-021** The Runner SHALL refuse plaintext connections to Hosts, the
  Orchestrator and the Pool Manager unless each endpoint is explicitly marked
  insecure.
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
