package awsclient

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/phoban01/flintlock-runner/internal/config"
)

// IAM actions the clients in this package issue, one per API call.
const (
	ActionDescribeInstances      = "ec2:DescribeInstances"
	ActionTerminateInstances     = "ec2:TerminateInstances"
	ActionSendCommand            = "ssm:SendCommand"
	ActionGetCommandInvocation   = "ssm:GetCommandInvocation"
	ActionListCommandInvocations = "ssm:ListCommandInvocations"
	ActionGetParameter           = "ssm:GetParameter"
)

// Policy is an IAM policy document.
type Policy struct {
	Version   string      `json:"Version"`
	Statement []Statement `json:"Statement"`
}

// Statement is one IAM policy statement.
type Statement struct {
	Sid      string   `json:"Sid"`
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource string   `json:"Resource"`
}

// Actions returns every action the policy allows, in document order.
func (p Policy) Actions() []string {
	var out []string
	for _, s := range p.Statement {
		out = append(out, s.Action...)
	}
	return out
}

// JSON renders the policy as the indented document published under
// docs/fleet/.
func (p Policy) JSON() []byte {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		// A Policy is strings and slices of strings; marshalling cannot fail.
		panic(err)
	}
	return append(b, '\n')
}

const policyVersion = "2012-10-17"

//= docs/requirements/09-security.md#least-privilege
//# The Fleet Controller SHALL operate with an IAM policy limited to
//# `ec2:DescribeInstances`, `ssm:SendCommand`, `ssm:GetCommandInvocation`,
//# `ssm:ListCommandInvocations`, `ssm:GetParameter` and, only when the
//# terminate flag is used, `ec2:TerminateInstances`.

// FleetControllerPolicy is the IAM policy the Fleet Controller runs with,
// published as docs/fleet/iam-policy.json. It is every action the clients in
// this package issue except TerminateInstances, which is in TerminatePolicy.
var FleetControllerPolicy = Policy{
	Version: policyVersion,
	Statement: []Statement{
		{Sid: "DiscoverInstances", Effect: "Allow", Action: []string{ActionDescribeInstances}, Resource: "*"},
		{Sid: "RunCommands", Effect: "Allow", Action: []string{ActionSendCommand, ActionGetCommandInvocation, ActionListCommandInvocations}, Resource: "*"},
		{Sid: "ReadSecretParameters", Effect: "Allow", Action: []string{ActionGetParameter}, Resource: "*"},
	},
}

// TerminatePolicy is attached in addition to FleetControllerPolicy only for
// `fleet teardown --terminate` (FL-082), published as
// docs/fleet/iam-policy-terminate.json.
var TerminatePolicy = Policy{
	Version: policyVersion,
	Statement: []Statement{
		{Sid: "TerminateOnTeardown", Effect: "Allow", Action: []string{ActionTerminateInstances}, Resource: "*"},
	},
}

// RunnerRefreshPolicy is the only AWS permission the Runner needs, and only
// when launch template mode's Inventory refresh is enabled (SE-041),
// published as docs/fleet/runner-iam-policy.json.
var RunnerRefreshPolicy = Policy{
	Version: policyVersion,
	Statement: []Statement{
		{Sid: "RefreshInventory", Effect: "Allow", Action: []string{ActionDescribeInstances}, Resource: "*"},
	},
}

// SecurityGroupRule is one rule an operator has to put in place; the Fleet
// Controller never creates or changes security groups (SE-042).
type SecurityGroupRule struct {
	// Group is the security group the rule belongs to: the Hosts' or the
	// Control Node's.
	Group string
	// Direction is inbound or outbound.
	Direction string
	Protocol  string
	Port      string
	// Peer is the other end of the rule.
	Peer string
	// When says when the rule is needed.
	When string
	// Purpose says what uses it.
	Purpose string
}

// Security group names used in SecurityGroupRule.Group and Peer.
const (
	HostGroup        = "Host"
	ControlNodeGroup = "Control Node"
)

//= docs/requirements/09-security.md#least-privilege
//# The Fleet Controller SHALL document the security group rules it
//# needs and SHALL NOT modify security groups itself.

// SecurityGroupRules returns the rules a fleet configured as f needs, with
// the configured ports. docs/fleet/security-groups.md carries the table for
// the default configuration, and the Fleet Controller names the rule when a
// Host is unreachable (FL-047).
func SecurityGroupRules(f *config.Fleet) []SecurityGroupRule {
	flintlockd := config.DefaultFlintlockdPort
	sshPort := config.DefaultSSHPort
	if f != nil {
		if f.Flintlockd.Port != 0 {
			flintlockd = f.Flintlockd.Port
		}
		if f.Remote.SSH.Port != 0 {
			sshPort = f.Remote.SSH.Port
		}
	}
	fl := strconv.Itoa(flintlockd)
	ssh := strconv.Itoa(sshPort)
	return []SecurityGroupRule{
		{
			Group: HostGroup, Direction: "inbound", Protocol: "tcp", Port: fl, Peer: ControlNodeGroup + " security group",
			When:    "always",
			Purpose: "flintlockd API: the Runner's Guest Transport, the Pool Manager daemon and `fleet verify`",
		},
		{
			Group: HostGroup, Direction: "inbound", Protocol: "tcp", Port: ssh, Peer: ControlNodeGroup + " security group",
			When:    "remote mode ssh",
			Purpose: "remote execution over SSH instead of Systems Manager",
		},
		{
			Group: HostGroup, Direction: "outbound", Protocol: "tcp", Port: "443", Peer: "Systems Manager endpoints and release downloads",
			When:    "always",
			Purpose: "Systems Manager agent, pinned releases and images; guests' NAT'd egress also leaves through the Host's outbound rules",
		},
		{
			Group: ControlNodeGroup, Direction: "outbound", Protocol: "tcp", Port: fl, Peer: HostGroup + " security group",
			When:    "always",
			Purpose: "flintlockd API on every Host",
		},
		{
			Group: ControlNodeGroup, Direction: "outbound", Protocol: "tcp", Port: ssh, Peer: HostGroup + " security group",
			When:    "remote mode ssh",
			Purpose: "remote execution over SSH",
		},
		{
			Group: ControlNodeGroup, Direction: "outbound", Protocol: "tcp", Port: "443", Peer: "AWS API endpoints and GitLab",
			When:    "always",
			Purpose: "EC2 and Systems Manager APIs, and the Runner's GitLab connection",
		},
	}
}

// SecurityGroupRulesMarkdown renders rules as the Markdown table in
// docs/fleet/security-groups.md.
func SecurityGroupRulesMarkdown(rules []SecurityGroupRule) string {
	var b strings.Builder
	b.WriteString("| Security group | Direction | Protocol | Port | Peer | Needed | Purpose |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, r := range rules {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n", r.Group, r.Direction, r.Protocol, r.Port, r.Peer, r.When, r.Purpose)
	}
	return b.String()
}
