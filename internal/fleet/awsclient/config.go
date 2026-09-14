// Package awsclient holds the real AWS clients behind the Fleet Controller's
// narrow interfaces (docs/requirements/10-test-doubles.md#fake-aws, TD-040):
// fleet.EC2, fleet.SSM and fleet.Parameters implemented with the AWS SDK for
// Go v2. Nothing else in the project imports the SDK service packages, so the
// set of AWS actions the Fleet Controller can issue is the set these clients
// call, which is the IAM policy of SE-040 (Policy).
//
// No test calls AWS: the tests drive these clients through an aws.HTTPClient
// that answers in process.
package awsclient

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/phoban01/flintlock-runner/internal/fleet"
)

//= docs/requirements/10-test-doubles.md#fake-aws
//# The Fleet Controller SHALL access EC2 and Systems Manager
//# through narrow interfaces of its own so that fakes can stand in for the
//# AWS SDK.

// The SDK clients are reached only through the fleet interfaces; awsfake
// implements the same ones.
var (
	_ fleet.EC2        = (*EC2)(nil)
	_ fleet.SSM        = (*SSM)(nil)
	_ fleet.Parameters = (*Parameters)(nil)
)

//= docs/requirements/06-fleet.md#discovery
//# The Fleet Controller SHALL obtain AWS credentials through the
//# default credential chain of the AWS SDK.

// LoadConfig loads the AWS configuration for region through the SDK's
// default chain: environment, shared config and credentials files, web
// identity, container and instance role credentials (FL-006). There is no
// setting to pass keys directly. optFns are applied after the region, for
// example to set an HTTP client.
func LoadConfig(ctx context.Context, region string, optFns ...func(*awsconfig.LoadOptions) error) (aws.Config, error) {
	opts := append([]func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}, optFns...)
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load AWS configuration: %w", err)
	}
	return cfg, nil
}
