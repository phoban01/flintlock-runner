package awsclient

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// StateRunning is the EC2 instance state DescribeFilter defaults to (FL-001).
const StateRunning = "running"

// EC2 implements fleet.EC2 with the SDK. It calls only DescribeInstances and
// TerminateInstances (SE-040).
type EC2 struct {
	api *ec2.Client
}

// NewEC2 returns an EC2 client for cfg, normally from LoadConfig.
func NewEC2(cfg aws.Config, optFns ...func(*ec2.Options)) *EC2 {
	return &EC2{api: ec2.NewFromConfig(cfg, optFns...)}
}

// DescribeInstances implements fleet.EC2, following every page.
func (c *EC2) DescribeInstances(ctx context.Context, f fleet.DescribeFilter) ([]fleet.Instance, error) {
	p := ec2.NewDescribeInstancesPaginator(c.api, describeInput(f))
	var out []fleet.Instance
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("ec2 DescribeInstances: %w", err)
		}
		for _, r := range page.Reservations {
			for _, in := range r.Instances {
				out = append(out, instanceFromEC2(in))
			}
		}
	}
	return out, nil
}

// TerminateInstances implements fleet.EC2. It is only called by teardown
// with the terminate flag (FL-082).
func (c *EC2) TerminateInstances(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := c.api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids}); err != nil {
		return fmt.Errorf("ec2 TerminateInstances: %w", err)
	}
	return nil
}

// describeInput translates the narrow filter into EC2 filters: the tag as
// tag:<key>, the ids as InstanceIds and the states as instance-state-name,
// running when none are given (FL-001, FL-002).
func describeInput(f fleet.DescribeFilter) *ec2.DescribeInstancesInput {
	in := &ec2.DescribeInstancesInput{}
	if len(f.InstanceIDs) > 0 {
		in.InstanceIds = append([]string(nil), f.InstanceIDs...)
	}
	if f.TagKey != "" {
		if f.TagValue != "" {
			in.Filters = append(in.Filters, ec2types.Filter{Name: aws.String("tag:" + f.TagKey), Values: []string{f.TagValue}})
		} else {
			in.Filters = append(in.Filters, ec2types.Filter{Name: aws.String("tag-key"), Values: []string{f.TagKey}})
		}
	}
	states := f.States
	if len(states) == 0 {
		states = []string{StateRunning}
	}
	in.Filters = append(in.Filters, ec2types.Filter{Name: aws.String("instance-state-name"), Values: append([]string(nil), states...)})
	return in
}

// instanceFromEC2 converts one described instance.
func instanceFromEC2(in ec2types.Instance) fleet.Instance {
	out := fleet.Instance{
		ID:        aws.ToString(in.InstanceId),
		Type:      string(in.InstanceType),
		Arch:      archFromEC2(in.Architecture),
		PrivateIP: aws.ToString(in.PrivateIpAddress),
		MemoryMB:  MetalMemoryMB(string(in.InstanceType)),
	}
	if in.State != nil {
		out.State = string(in.State.Name)
	}
	if len(in.Tags) > 0 {
		out.Tags = make(map[string]string, len(in.Tags))
		for _, t := range in.Tags {
			out.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
	}
	if in.CpuOptions != nil {
		cores, threads := aws.ToInt32(in.CpuOptions.CoreCount), aws.ToInt32(in.CpuOptions.ThreadsPerCore)
		if threads == 0 {
			threads = 1
		}
		out.VCPU = int(cores * threads)
	}
	return out
}

//= docs/requirements/06-fleet.md#discovery
//# The Fleet Controller SHALL determine each instance's
//# architecture from the EC2 instance attributes and record it in the
//# Inventory.

// archFromEC2 maps the instance's architecture attribute to the project's
// Architecture. Anything other than x86_64 and arm64 (i386 and the mac
// variants) maps to empty, which discovery reports as unsupported.
func archFromEC2(a ec2types.ArchitectureValues) config.Architecture {
	switch a {
	case ec2types.ArchitectureValuesX8664:
		return config.ArchAMD64
	case ec2types.ArchitectureValuesArm64:
		return config.ArchARM64
	default:
		return ""
	}
}

// metalMemoryGiB is the memory of the bare-metal instance types, because
// DescribeInstances does not report memory and DescribeInstanceTypes is
// outside the IAM policy of SE-040.
var metalMemoryGiB = map[string]int{
	"a1.metal":     32,
	"c5.metal":     192,
	"c5d.metal":    192,
	"c5n.metal":    192,
	"c6a.metal":    384,
	"c6g.metal":    128,
	"c6gd.metal":   128,
	"c6i.metal":    256,
	"c6id.metal":   256,
	"c6in.metal":   256,
	"c7g.metal":    128,
	"c7gd.metal":   128,
	"c7gn.metal":   128,
	"g4dn.metal":   384,
	"i3.metal":     512,
	"i3en.metal":   768,
	"i4i.metal":    1024,
	"m5.metal":     384,
	"m5d.metal":    384,
	"m5dn.metal":   384,
	"m5n.metal":    384,
	"m5zn.metal":   192,
	"m6a.metal":    768,
	"m6g.metal":    256,
	"m6gd.metal":   256,
	"m6i.metal":    512,
	"m6id.metal":   512,
	"m6idn.metal":  512,
	"m6in.metal":   512,
	"m7g.metal":    256,
	"m7gd.metal":   256,
	"r5.metal":     768,
	"r5b.metal":    768,
	"r5d.metal":    768,
	"r5dn.metal":   768,
	"r5n.metal":    768,
	"r6a.metal":    1536,
	"r6g.metal":    512,
	"r6gd.metal":   512,
	"r6i.metal":    1024,
	"r6id.metal":   1024,
	"r7g.metal":    512,
	"r7gd.metal":   512,
	"x2gd.metal":   1024,
	"x2idn.metal":  2048,
	"x2iedn.metal": 4096,
	"x2iezn.metal": 1536,
	"z1d.metal":    384,
}

// MetalMemoryMB returns the memory in MiB of a bare-metal instance type, or
// zero when the type is not in the table.
func MetalMemoryMB(instanceType string) int {
	return metalMemoryGiB[instanceType] * 1024
}
