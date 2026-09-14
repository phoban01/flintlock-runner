// Package awsfake holds the fake AWS services the Fleet Controller is tested
// against (docs/requirements/10-test-doubles.md#fake-aws): EC2
// DescribeInstances and TerminateInstances, Systems Manager SendCommand,
// GetCommandInvocation and ListCommandInvocations, and GetParameter. Each
// records every call and returns what the test configures (TD-041, TD-042).
package awsfake
