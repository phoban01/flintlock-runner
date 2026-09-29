// Package transport is the Guest Transport
// (docs/requirements/02-executor.md#guest-transport): the mechanism by which
// the Executor runs a command inside a MicroVM and streams its input and
// output. Two implementations exist, exec over flintlock's MicroVMExec
// (EX-043 to EX-046) and ssh over the MicroVMSSHProxy RPC (EX-047 to
// EX-049), selectable per Profile (EX-042). Neither has a mode that dials
// the guest directly: a pooled MicroVM comes from one shared template and
// flintlock reports no address the Runner could connect to (EX-048), so
// both reach the guest through the Host that runs it.
//
// A cluster fleet has a third, agent-exec
// (docs/requirements/12-cluster-fleet.md#agent-exec-transport). It is the
// exec implementation over a client of battery-operator's Exec Agent, which
// relays each session to MicroVMExec on its own Host's flintlockd. The
// Runner reaches the Exec Agent that each Job's claim names, with that
// claim's token (KF-185, KF-186).
//
// The Executor depends only on Transport and Factory, so its unit tests use a
// recording fake and never open a stream. The exec implementation depends
// only on flintlock.ExecStream, so its framing tests use a scripted stream
// and never open a connection.
package transport
