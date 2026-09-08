// Package transport is the Guest Transport
// (docs/requirements/02-executor.md#guest-transport): the mechanism by which
// the Executor runs a command inside a MicroVM and streams its input and
// output. Two implementations exist, exec over flintlock's MicroVMExec
// (EX-043 to EX-046) and ssh over the MicroVMSSHProxy RPC or direct TCP
// (EX-047 to EX-049), selectable per Profile (EX-042).
//
// The Executor depends only on Transport and Factory, so its unit tests use a
// recording fake and never open a stream. The exec implementation depends
// only on flintlock.ExecStream, so its framing tests use a scripted stream
// and never open a connection.
package transport
