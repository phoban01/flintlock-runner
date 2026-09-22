// Package verify is `fleet verify`: it checks every Host and Pool and
// exercises a MicroVM on every Host through the Pool Manager and the Guest
// Transport (FL-070 to FL-073). For a cluster fleet, Cluster verifies every
// Virtual Node with a verification pod bound to it and runs the Host
// Service checks of FL-109 inside that pod over kube-exec (KF-100 to
// KF-102).
package verify
