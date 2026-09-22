// Package harness is the end-to-end harness
// (docs/requirements/10-test-doubles.md#end-to-end-harness). It brings up
// the fake GitLab, the fake Pool Manager and a number of fake Hosts on real
// loopback ports, writes a runner configuration pointing at them, runs the
// real flr binary against that configuration as a subprocess,
// and lets the caller submit Jobs and wait for their trace and final state.
//
// A Stack is started in two steps, Start for the fakes and the
// configuration and StartRunner for the binary, so that a test can check the
// configuration with `flr config show` before anything runs.
// Shutdown stops the Runner with SIGTERM, then checks that no Lease is held,
// then stops the Pool Manager and the Hosts and checks that no sandbox
// directory is left behind (TD-054). Every exit path kills the Runner's
// process group, and on Linux the Runner is also killed if the process that
// started it dies.
//
// Two environment variables move the same scenarios from fakes to metal
// (TD-052, TD-053): EnvHardwareInventory names an Inventory file of real
// flintlockd Hosts to use instead of fake Hosts, and EnvPoolManager names a
// real Pool Manager endpoint to use instead of the fake one. OptionsFromEnv
// reads them; nothing else in the package looks at the environment.
//
// Options.Backend selects the stack the Runner runs over. The zero value is
// the battery stack above. BackendKubernetes is the cluster stack (KF-122):
// the Runner on the Kubernetes pool backend and the kube-exec Guest
// Transport, with nothing but a kubeconfig for a user bound to its shipped
// Role; a real kube-apiserver and etcd (Cluster, shared by many Stacks, each
// in a namespace of its own) with a test reconciler for the ReplicaSet
// controller and a stand-in for the scheduler; and on each fake Host the
// real Pod Provider, running in the test process under the Host Agent's
// shipped RBAC and admission policy, which the API server's exec proxy
// reaches as it reaches a kubelet. The same scenarios run on both stacks.
// On the cluster stack Shutdown checks for claimed pods instead of Leases,
// then deletes the Pools, which the Runner leaves behind by design, so that
// a sandbox still on a fake Host afterwards is a MicroVM no Pod Provider
// deleted.
//
// The fake Host runs every command as a local process on the machine the
// harness runs on, rooted in the MicroVM's sandbox directory but not
// confined to it. gitlab-runner's generated scripts use absolute paths for
// the builds and cache directories, so the harness places both under a
// temporary root it owns and removes, and never lets a Job write to /builds.
package harness
