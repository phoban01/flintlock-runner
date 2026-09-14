package awsclient

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

var update = flag.Bool("update", false, "rewrite docs/fleet from the Go declarations")

const docsDir = "../../../docs/fleet"

//= docs/requirements/06-fleet.md#discovery
//= type=test
//# The Fleet Controller SHALL obtain AWS credentials through the
//# default credential chain of the AWS SDK.

// TestLoadConfigUsesDefaultCredentialChain checks two links of the chain,
// the environment and a named profile in the shared credentials file, with
// the instance metadata service disabled so nothing leaves the process.
func TestLoadConfigUsesDefaultCredentialChain(t *testing.T) {
	dir := t.TempDir()
	credsFile := filepath.Join(dir, "credentials")
	if err := os.WriteFile(credsFile, []byte("[fleet]\naws_access_key_id = AKIDFILE\naws_secret_access_key = file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credsFile)
	for _, k := range []string{"AWS_SESSION_TOKEN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"} {
		t.Setenv(k, "")
	}
	ctx := context.Background()

	t.Run("environment", func(t *testing.T) {
		t.Setenv("AWS_ACCESS_KEY_ID", "AKIDENV")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "env-secret")
		t.Setenv("AWS_PROFILE", "")
		cfg, err := LoadConfig(ctx, "eu-west-1")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Region != "eu-west-1" {
			t.Errorf("region = %q", cfg.Region)
		}
		creds, err := cfg.Credentials.Retrieve(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if creds.AccessKeyID != "AKIDENV" {
			t.Errorf("access key = %q, want the environment's", creds.AccessKeyID)
		}
	})
	t.Run("shared credentials profile", func(t *testing.T) {
		t.Setenv("AWS_ACCESS_KEY_ID", "")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "")
		t.Setenv("AWS_PROFILE", "fleet")
		cfg, err := LoadConfig(ctx, "eu-west-1")
		if err != nil {
			t.Fatal(err)
		}
		creds, err := cfg.Credentials.Retrieve(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if creds.AccessKeyID != "AKIDFILE" {
			t.Errorf("access key = %q, want the profile's", creds.AccessKeyID)
		}
	})
}

// TestDescribeInstancesSendsTagIDAndStateFilters checks the request the SDK
// client builds from a DescribeFilter.
func TestDescribeInstancesSendsTagIDAndStateFilters(t *testing.T) {
	t.Parallel()
	stub := newStubAWS()
	stub.reply(ActionDescribeInstances, 200, describeInstancesXML)
	c := NewEC2(stubConfig(t, stub))
	ctx := context.Background()

	if _, err := c.DescribeInstances(ctx, fleet.DescribeFilter{TagKey: "flintlock-runner", TagValue: "ci"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DescribeInstances(ctx, fleet.DescribeFilter{InstanceIDs: []string{"i-arm", "i-x86"}}); err != nil {
		t.Fatal(err)
	}
	sent := stub.sent()
	if len(sent) != 2 {
		t.Fatalf("sent %d requests", len(sent))
	}
	byTag := sent[0].Form
	for k, want := range map[string]string{
		"Filter.1.Name": "tag:flintlock-runner", "Filter.1.Value.1": "ci",
		"Filter.2.Name": "instance-state-name", "Filter.2.Value.1": "running",
	} {
		if got := byTag.Get(k); got != want {
			t.Errorf("tag request %s = %q, want %q", k, got, want)
		}
	}
	if byTag.Has("InstanceId.1") {
		t.Errorf("tag request names instance ids: %v", byTag)
	}
	byID := sent[1].Form
	for k, want := range map[string]string{
		"InstanceId.1": "i-arm", "InstanceId.2": "i-x86",
		"Filter.1.Name": "instance-state-name", "Filter.1.Value.1": "running",
	} {
		if got := byID.Get(k); got != want {
			t.Errorf("id request %s = %q, want %q", k, got, want)
		}
	}
}

//= docs/requirements/06-fleet.md#discovery
//= type=test
//# The Fleet Controller SHALL determine each instance's
//# architecture from the EC2 instance attributes and record it in the
//# Inventory.

// TestDescribeInstancesReadsArchitectureAttribute parses a DescribeInstances
// response and checks each instance's architecture attribute becomes its
// Arch, with an unsupported one left empty.
func TestDescribeInstancesReadsArchitectureAttribute(t *testing.T) {
	t.Parallel()
	stub := newStubAWS()
	stub.reply(ActionDescribeInstances, 200, describeInstancesXML)
	got, err := NewEC2(stubConfig(t, stub)).DescribeInstances(context.Background(), fleet.DescribeFilter{TagKey: "flintlock-runner", TagValue: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	want := []fleet.Instance{
		{ID: "i-arm", Type: "m7g.metal", Arch: config.ArchARM64, PrivateIP: "10.0.1.10", State: "running", Tags: map[string]string{"flintlock-runner": "ci"}, VCPU: 64, MemoryMB: 256 * 1024},
		{ID: "i-x86", Type: "c5.metal", Arch: config.ArchAMD64, PrivateIP: "10.0.1.11", State: "running", VCPU: 96, MemoryMB: 192 * 1024},
		{ID: "i-386", Type: "c5.metal", Arch: "", PrivateIP: "10.0.1.12", State: "running", MemoryMB: 192 * 1024},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("instances:\n got %+v\nwant %+v", got, want)
	}
}

// TestRunShellScriptCommandRunsScriptWithBash runs the wrapper the way
// AWS-RunShellScript does, with sh, and checks the script runs under bash
// verbatim, cannot read its own remainder from standard input and passes
// its exit status through.
func TestRunShellScriptCommandRunsScriptWithBash(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	script := strings.Join([]string{
		`#!/bin/bash`,
		`set -euo pipefail`,
		`[[ -n "${BASH_VERSION:-}" ]] || exit 90`,
		`echo 'literal $HOME and $(date)'`,
		`if read -r line; then echo "read: $line"; fi`,
		`echo FLINTLOCK_RUNNER_SCRIPT_EOF`,
		`exit 7`,
	}, "\n")
	cmd := exec.Command("sh", "-c", RunShellScriptCommand(script))
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("exit = %v, want status 7; stdout %q", err, stdout.String())
	}
	want := "literal $HOME and $(date)\nFLINTLOCK_RUNNER_SCRIPT_EOF\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestSSMClientMapsInvocations checks the Systems Manager client's request
// and response translation, including InvocationDoesNotExist as Pending.
func TestSSMClientMapsInvocations(t *testing.T) {
	t.Parallel()
	stub := newStubAWS()
	stub.reply(ActionSendCommand, 200, `{"Command":{"CommandId":"c-1"}}`)
	stub.reply(ActionGetCommandInvocation, 400, `{"__type":"InvocationDoesNotExist","message":"not yet"}`)
	stub.reply(ActionGetCommandInvocation, 200, `{"CommandId":"c-1","InstanceId":"i-a","Status":"Failed","ResponseCode":3,"StandardOutputContent":"out","StandardErrorContent":"err"}`)
	c := NewSSM(stubConfig(t, stub))
	ctx := context.Background()
	id, err := c.SendCommand(ctx, fleet.SendCommandInput{InstanceIDs: []string{"i-a"}, Script: "echo hi", Comment: strings.Repeat("c", 150), Timeout: 90 * time.Second})
	if err != nil || id != "c-1" {
		t.Fatalf("SendCommand = %q, %v", id, err)
	}
	body := stub.sent()[0].JSON
	for _, want := range []string{`"DocumentName":"AWS-RunShellScript"`, `"executionTimeout":["90"]`, `"InstanceIds":["i-a"]`, `"Comment":"` + strings.Repeat("c", 100) + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("SendCommand body lacks %s: %s", want, body)
		}
	}
	pending, err := c.GetCommandInvocation(ctx, "c-1", "i-a")
	if err != nil || pending.Status != StatusPending {
		t.Errorf("first poll = %+v, %v; want Pending", pending, err)
	}
	done, err := c.GetCommandInvocation(ctx, "c-1", "i-a")
	if err != nil {
		t.Fatal(err)
	}
	want := fleet.Invocation{CommandID: "c-1", InstanceID: "i-a", Status: "Failed", ResponseCode: 3, Stdout: "out", Stderr: "err"}
	if *done != want {
		t.Errorf("final poll = %+v, want %+v", *done, want)
	}
}

// exerciseEveryClientCall drives every method of every client in this
// package through stub and returns the actions the requests needed.
func exerciseEveryClientCall(t *testing.T) []string {
	t.Helper()
	stub := newStubAWS()
	stub.reply(ActionDescribeInstances, 200, describeInstancesXML)
	stub.reply(ActionTerminateInstances, 200, terminateInstancesXML)
	stub.reply(ActionSendCommand, 200, `{"Command":{"CommandId":"c-1"}}`)
	stub.reply(ActionGetCommandInvocation, 200, `{"CommandId":"c-1","InstanceId":"i-a","Status":"Success","ResponseCode":0}`)
	stub.reply(ActionListCommandInvocations, 200, `{"CommandInvocations":[{"CommandId":"c-1","InstanceId":"i-a","Status":"Success","CommandPlugins":[{"ResponseCode":0,"Output":"ok"}]}]}`)
	stub.reply(ActionGetParameter, 200, `{"Parameter":{"Name":"/fleet/token","Value":"s3cret"}}`)
	cfg := stubConfig(t, stub)
	ctx := context.Background()

	e := NewEC2(cfg)
	if _, err := e.DescribeInstances(ctx, fleet.DescribeFilter{TagKey: "k", TagValue: "v"}); err != nil {
		t.Fatal(err)
	}
	if err := e.TerminateInstances(ctx, []string{"i-a"}); err != nil {
		t.Fatal(err)
	}
	s := NewSSM(cfg)
	if _, err := s.SendCommand(ctx, fleet.SendCommandInput{InstanceIDs: []string{"i-a"}, Script: "true"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCommandInvocation(ctx, "c-1", "i-a"); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListCommandInvocations(ctx, "c-1"); err != nil || len(list) != 1 || list[0].Stdout != "ok" {
		t.Fatalf("ListCommandInvocations = %+v, %v", list, err)
	}
	p := NewParameters(cfg)
	if v, err := p.GetParameter(ctx, "/fleet/token"); err != nil || v != "s3cret" {
		t.Fatalf("GetParameter = %q, %v", v, err)
	}
	var actions []string
	for _, r := range stub.sent() {
		actions = append(actions, r.Action)
		if r.Action == ActionGetParameter && !strings.Contains(r.JSON, `"WithDecryption":true`) {
			t.Errorf("GetParameter without decryption: %s", r.JSON)
		}
	}
	return actions
}

// checkDoc compares the published document with the Go declaration, or
// rewrites it with -update.
func checkDoc(t *testing.T, name string, want []byte) {
	t.Helper()
	path := filepath.Join(docsDir, name)
	if *update {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is out of date with the Go declaration; run go test ./internal/fleet/awsclient -update\n got:\n%s\nwant:\n%s", path, got, want)
	}
}

//= docs/requirements/09-security.md#least-privilege
//= type=test
//# The Fleet Controller SHALL operate with an IAM policy limited to
//# `ec2:DescribeInstances`, `ssm:SendCommand`, `ssm:GetCommandInvocation`,
//# `ssm:ListCommandInvocations`, `ssm:GetParameter` and, only when the
//# terminate flag is used, `ec2:TerminateInstances`.

// TestClientsNeedOnlyThePublishedPolicy drives every call the AWS clients
// can make and checks that the actions they need are exactly the published
// policy plus the separate terminate policy, and that the published
// documents match the declarations.
func TestClientsNeedOnlyThePublishedPolicy(t *testing.T) {
	t.Parallel()
	used := exerciseEveryClientCall(t)
	slices.Sort(used)
	used = slices.Compact(used)

	base := FleetControllerPolicy.Actions()
	wantBase := []string{ActionDescribeInstances, ActionSendCommand, ActionGetCommandInvocation, ActionListCommandInvocations, ActionGetParameter}
	if !reflect.DeepEqual(base, wantBase) {
		t.Errorf("FleetControllerPolicy actions = %v, want exactly %v", base, wantBase)
	}
	if got := TerminatePolicy.Actions(); !reflect.DeepEqual(got, []string{ActionTerminateInstances}) {
		t.Errorf("TerminatePolicy actions = %v", got)
	}
	allowed := append(slices.Clone(base), TerminatePolicy.Actions()...)
	slices.Sort(allowed)
	if !reflect.DeepEqual(used, allowed) {
		t.Errorf("clients issue %v, policies allow %v", used, allowed)
	}
	checkDoc(t, "iam-policy.json", FleetControllerPolicy.JSON())
	checkDoc(t, "iam-policy-terminate.json", TerminatePolicy.JSON())
}

//= docs/requirements/09-security.md#least-privilege
//= type=test
//# The Runner SHALL NOT require any AWS permission at run time
//# unless launch template mode's Inventory refresh is enabled, in which case
//# it SHALL require only `ec2:DescribeInstances`.

// TestRunnerPolicyIsDescribeInstancesOnly pins the Runner's published policy;
// the discovery package checks that the refresh uses nothing more.
func TestRunnerPolicyIsDescribeInstancesOnly(t *testing.T) {
	t.Parallel()
	if got := RunnerRefreshPolicy.Actions(); !reflect.DeepEqual(got, []string{ActionDescribeInstances}) {
		t.Errorf("RunnerRefreshPolicy actions = %v", got)
	}
	checkDoc(t, "runner-iam-policy.json", RunnerRefreshPolicy.JSON())
}

//= docs/requirements/09-security.md#least-privilege
//= type=test
//# The Fleet Controller SHALL document the security group rules it
//# needs and SHALL NOT modify security groups itself.

// TestSecurityGroupRulesAreDocumentedAndNeverModified checks that
// docs/fleet/security-groups.md carries the rule table for the default
// configuration, and that neither the clients nor the policy touch security
// groups.
func TestSecurityGroupRulesAreDocumentedAndNeverModified(t *testing.T) {
	t.Parallel()
	f := &config.Fleet{Remote: config.Remote{Mode: config.RemoteSSH}}
	table := SecurityGroupRulesMarkdown(SecurityGroupRules(f))
	for _, want := range []string{"| Host | inbound | tcp | 9090 |", "| Host | inbound | tcp | 22 |", "| Control Node | outbound | tcp | 9090 |"} {
		if !strings.Contains(table, want) {
			t.Errorf("rules lack %q:\n%s", want, table)
		}
	}
	custom := SecurityGroupRulesMarkdown(SecurityGroupRules(&config.Fleet{Flintlockd: config.Flintlockd{Port: 9443}, Remote: config.Remote{SSH: config.SSHRemote{Port: 2222}}}))
	if !strings.Contains(custom, "| Host | inbound | tcp | 9443 |") || !strings.Contains(custom, "| Host | inbound | tcp | 2222 |") {
		t.Errorf("configured ports not used:\n%s", custom)
	}

	path := filepath.Join(docsDir, "security-groups.md")
	doc, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const begin, end = "<!-- BEGIN generated rules -->\n", "<!-- END generated rules -->"
	i, j := bytes.Index(doc, []byte(begin)), bytes.Index(doc, []byte(end))
	if i < 0 || j < i {
		t.Fatalf("%s lacks the generated rules markers", path)
	}
	if *update {
		doc = slices.Concat(doc[:i+len(begin)], []byte(table), doc[j:])
		if err := os.WriteFile(path, doc, 0o644); err != nil {
			t.Fatal(err)
		}
		i, j = bytes.Index(doc, []byte(begin)), bytes.Index(doc, []byte(end))
	}
	if got := string(doc[i+len(begin) : j]); got != table {
		t.Errorf("%s rules are out of date; run go test ./internal/fleet/awsclient -update\n got:\n%s\nwant:\n%s", path, got, table)
	}

	for _, a := range append(exerciseEveryClientCall(t), append(FleetControllerPolicy.Actions(), TerminatePolicy.Actions()...)...) {
		if strings.Contains(strings.ToLower(a), "securitygroup") {
			t.Errorf("action %s modifies security groups", a)
		}
	}
}

