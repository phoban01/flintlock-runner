package main

import (
	"fmt"
	"io"

	"github.com/phoban01/flintlock-runner/internal/config"
)

// hostServicesDoc is the document that states what Jobs on one Host share
// through the Host Services. The notes below are quoted from it verbatim,
// and hostdocs_test.go fails when the two drift apart.
const hostServicesDoc = "docs/host-services.md"

//= docs/requirements/09-security.md#host-services-security
//# Where a Job builds images with `buildkitd`, the Runner SHALL
//# document that the layer cache is shared between Jobs on the same Host and
//# that a Job can read layers another Job on that Host produced.

// noteSharedLayerCache is the documented statement of SE-054.
const noteSharedLayerCache = "The buildkitd layer cache is shared between Jobs on the same Host: a Job can read the layers another Job on that Host produced."

//= docs/requirements/09-security.md#host-services-security
//# The Runner SHALL document that private modules cached by a
//# Host's Go module proxy are readable by every Job that runs on that Host.

// noteSharedPrivateModules is the documented statement of SE-056.
const noteSharedPrivateModules = "Private modules cached by a Host's Go module proxy are readable by every Job that runs on that Host."

// printHostServiceNotes repeats the documented sharing notes for the Host
// Services the configuration enables, so that the operator who verifies a
// fleet sees them.
func printHostServiceNotes(w io.Writer, hs config.HostServices) {
	if hs.Buildkit.IsEnabled() {
		fmt.Fprintf(w, "note: %s (%s)\n", noteSharedLayerCache, hostServicesDoc)
	}
	if hs.GoProxy.IsEnabled() && len(hs.GoProxy.PrivatePatterns) > 0 {
		fmt.Fprintf(w, "note: %s (%s)\n", noteSharedPrivateModules, hostServicesDoc)
	}
}
