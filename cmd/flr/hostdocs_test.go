package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/config"
)

// readHostServicesDoc reads docs/host-services.md from the repository root.
func readHostServicesDoc(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", hostServicesDoc))
	if err != nil {
		t.Fatalf("the Host Services document is missing: %v", err)
	}
	// Markdown may wrap; compare with whitespace collapsed.
	return strings.Join(strings.Fields(string(data)), " ")
}

//= docs/requirements/09-security.md#host-services-security
//= type=test
//# Where a Job builds images with `buildkitd`, the Runner SHALL
//# document that the layer cache is shared between Jobs on the same Host and
//# that a Job can read layers another Job on that Host produced.

func TestHostServicesDocStatesTheSharedLayerCache(t *testing.T) {
	t.Parallel()
	doc := readHostServicesDoc(t)
	if !strings.Contains(doc, noteSharedLayerCache) {
		t.Errorf("%s does not state: %q", hostServicesDoc, noteSharedLayerCache)
	}
	for _, want := range []string{"shared between Jobs on the same Host", "can read the layers another Job on that Host produced"} {
		if !strings.Contains(noteSharedLayerCache, want) {
			t.Errorf("the note no longer says %q", want)
		}
	}
}

//= docs/requirements/09-security.md#host-services-security
//= type=test
//# The Runner SHALL document that private modules cached by a
//# Host's Go module proxy are readable by every Job that runs on that Host.

func TestHostServicesDocStatesTheSharedPrivateModules(t *testing.T) {
	t.Parallel()
	doc := readHostServicesDoc(t)
	if !strings.Contains(doc, noteSharedPrivateModules) {
		t.Errorf("%s does not state: %q", hostServicesDoc, noteSharedPrivateModules)
	}
	if !strings.Contains(noteSharedPrivateModules, "readable by every Job that runs on that Host") {
		t.Errorf("the note no longer says private modules are readable by every Job on the Host")
	}
}

func TestPrintHostServiceNotesFollowsTheEnabledServices(t *testing.T) {
	t.Parallel()
	off := false
	var buf bytes.Buffer
	printHostServiceNotes(&buf, config.HostServices{GoProxy: config.GoProxy{PrivatePatterns: []string{"gitlab.example.com/acme/*"}}})
	if !strings.Contains(buf.String(), noteSharedLayerCache) || !strings.Contains(buf.String(), noteSharedPrivateModules) {
		t.Errorf("notes = %q, want both", buf.String())
	}
	buf.Reset()
	printHostServiceNotes(&buf, config.HostServices{Buildkit: config.Buildkit{Service: config.Service{Enabled: &off}}})
	if buf.Len() != 0 {
		t.Errorf("notes with buildkit disabled and no private patterns = %q, want none", buf.String())
	}
}
