# Security groups and IAM for the Fleet Controller

The Fleet Controller never creates, changes or deletes a security group
(SE-042); it has no permission to, since its IAM policy holds no security
group action. The operator puts the rules below in place before running
`flintlock-runner fleet provision`. When a Host's `flintlockd` port is
unreachable after provisioning, the Fleet Controller names the rule from this
table that is missing (FL-047).

Two security groups are assumed: one shared by every Host (the EC2 instances
the Fleet Controller provisions; see `instance-types.md`) and one on the
Control Node (the machine running the Runner, the
Pool Manager daemon and the Fleet Controller). Where the Control Node is
outside the VPC, replace "Control Node security group" with its address.

## Rules

The table is for the default configuration: `flintlockd` on port 9090 and,
where `fleet.remote.mode` is `ssh`, SSH on port 22. The Fleet Controller
substitutes the configured ports when it names a rule. The table is generated
from `internal/fleet/awsclient.SecurityGroupRules` and checked by that
package's tests; do not edit it by hand.

<!-- BEGIN generated rules -->
| Security group | Direction | Protocol | Port | Peer | Needed | Purpose |
|---|---|---|---|---|---|---|
| Host | inbound | tcp | 9090 | Control Node security group | always | flintlockd API: the Runner's Guest Transport, the Pool Manager daemon and `fleet verify` |
| Host | inbound | tcp | 22 | Control Node security group | remote mode ssh | remote execution over SSH instead of Systems Manager |
| Host | outbound | tcp | 443 | Systems Manager endpoints and release downloads | always | Systems Manager agent, pinned releases and images; guests' NAT'd egress also leaves through the Host's outbound rules |
| Control Node | outbound | tcp | 9090 | Host security group | always | flintlockd API on every Host |
| Control Node | outbound | tcp | 22 | Host security group | remote mode ssh | remote execution over SSH |
| Control Node | outbound | tcp | 443 | AWS API endpoints and GitLab | always | EC2 and Systems Manager APIs, and the Runner's GitLab connection |
<!-- END generated rules -->

Nothing else needs to be open. In particular:

- The Host Services (`buildkitd`, the Go module proxy, the registry mirror
  and the HTTP cache) bind to the guest bridge gateway address only and need
  no security group rule; they are closed to other Hosts by the Host
  firewall, not by the security group (FL-101, SE-051).
- The Pool Manager daemon listens on the Control Node for the Runner on the
  same machine; nothing outside the Control Node connects to it.
- Guests have no inbound path: the Guest Transport runs over vsock through
  `flintlockd` (see `docs/architecture.md`).
- Remote execution through Systems Manager (the default) needs no inbound
  rule on the Hosts, only the outbound HTTPS rule for the agent.

## IAM

- `iam-policy.json` is the Fleet Controller's policy: `ec2:DescribeInstances`
  for discovery, `ssm:SendCommand`, `ssm:GetCommandInvocation` and
  `ssm:ListCommandInvocations` for remote execution, and `ssm:GetParameter`
  for the secrets named in the configuration (SE-040).
- `iam-policy-terminate.json` adds `ec2:TerminateInstances`. Attach it only
  for `fleet teardown --terminate` (FL-082).
- `runner-iam-policy.json` is the Runner's policy: none at all, unless launch
  template mode's Inventory refresh is enabled, in which case this one, with
  `ec2:DescribeInstances` only (SE-041).
- The Hosts' instance profile needs the managed policy
  `AmazonSSMManagedInstanceCore` for the Systems Manager agent and, in launch
  template mode, `ssm:GetParameter` on the parameters that hold the
  `flintlockd` token and TLS material, which the first-boot script reads
  (FL-091).

The three policy files are generated from the declarations in
`internal/fleet/awsclient/policy.go`, whose tests fail if the files drift or if
the AWS clients issue any action the policies do not list.
