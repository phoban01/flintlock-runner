# Instance types and KVM

The Fleet Controller accepts an EC2 instance of any type (FL-116). What
decides whether an instance can be a Host is whether it has a usable KVM,
and that is a property of the running instance rather than of its type's
name: bare-metal types (`*.metal`) always have it, and some virtualized
types have it when nested virtualization is enabled on the instance.

## What provisioning checks

The first provisioning step, `detect`, runs on the instance and checks that
`/dev/kvm` exists, is a character device, and can be opened for reading and
writing by root, which is the user `flintlockd` runs as. The last part fails
when the device node is there but no KVM driver is behind it. The check
installs nothing and needs no package, so it works on a stock Ubuntu or
Amazon Linux image.

An instance that fails the check is stopped there, before anything is
installed on it. It gets no Inventory entry, and `fleet provision` reports
it as unsupported because KVM is unavailable, with the reason detect gave
(FL-117):

```
i-0123456789abcdef0 (c8i.2xlarge): unsupported because KVM is unavailable: /dev/kvm does not exist; a virtualized instance type needs nested virtualization enabled
```

The same step measures the Host's capacity: its online CPUs
(`getconf _NPROCESSORS_ONLN`) and `MemTotal` from `/proc/meminfo`. The
Inventory records these minus `fleet.host_reserve` (FL-061). `MemTotal` is
somewhat less than the memory the instance type is sold with, because the
kernel keeps some for itself. No table of instance types is involved, so
nothing has to be updated when AWS adds a type, and the IAM policy stays at
`ec2:DescribeInstances` for discovery (SE-040).

## Bare-metal instances

A `*.metal` instance has KVM with no setting.

## Virtualized instances: enable nested virtualization

A virtualized instance needs nested virtualization enabled to have
`/dev/kvm`. The Fleet Controller does not launch instances and cannot enable
it; set it where the instances are launched. Everything below is quoted from
the EC2 API types of the AWS SDK for Go v2 (`service/ec2` v1.332.0, the
version in `go.mod`); the AWS CLI, CloudFormation and Terraform spellings
were not checked.

- At launch, `RunInstances` takes `CpuOptions` (`types.CpuOptionsRequest`),
  whose `NestedVirtualization` field is a
  `types.NestedVirtualizationSpecification` with the values `enabled` and
  `disabled`. The SDK documents it as:

  > Indicates whether to enable the instance for nested virtualization.
  > Nested virtualization is supported only on 8th generation Intel-based
  > instance types (c8i, m8i, r8i, and their flex variants). When nested
  > virtualization is enabled, Virtual Secure Mode (VSM) is automatically
  > disabled for the instance.

  In Go:

  ```go
  input := &ec2.RunInstancesInput{
      // ...
      CpuOptions: &types.CpuOptionsRequest{
          NestedVirtualization: types.NestedVirtualizationSpecificationEnabled,
      },
  }
  ```

- In a launch template, which is what launch template mode's auto scaling
  groups use, `RequestLaunchTemplateData.CpuOptions`
  (`types.LaunchTemplateCpuOptionsRequest`) has the same
  `NestedVirtualization` field with the same documentation.
- On an existing instance, `ModifyInstanceCpuOptions` takes
  `NestedVirtualization` ("Indicates whether to enable or disable nested
  virtualization for the instance."); the operation's documentation says the
  instance must be in a Stopped state before you make changes.
- To check an instance, `DescribeInstances` reports
  `CpuOptions.NestedVirtualization` ("Indicates whether the instance is
  enabled for nested virtualization."). To check a type,
  `DescribeInstanceTypes` lists `nested-virtualization` among
  `ProcessorInfo.SupportedFeatures`.

The Fleet Controller makes none of these calls, so its IAM policy does not
change. It does not have to: an instance launched without the setting fails
the KVM check and is reported, and nothing is installed on it.

The types the SDK names are all Intel (amd64). As long as that holds, a
virtualized Graviton type has no KVM and an arm64 Host has to be a `*.metal`
instance.
