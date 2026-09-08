// Package fleet is the Fleet Controller (docs/requirements/06-fleet.md): the
// `flintlock-runner fleet` subcommands that turn EC2 bare-metal instances
// into flintlock Hosts, install the Pool Manager and the Host Services, and
// produce the Inventory and Runner configuration.
//
// Nothing here talks to AWS directly. Discovery, Remote, EC2, SSM and
// Parameters are the narrow interfaces of TD-040; their fakes record every
// call and return what the test configures (TD-041, TD-042). The static
// discovery provider (TD-044) and the SSH Remote (FL-011) are the path that
// runs against any Linux machine without an account. Provisioning logic is
// expressed as scripts rendered by Scripts, so that every script can be
// enumerated and linted in CI (TD-043) and asserted on through the fake SSM.
package fleet
