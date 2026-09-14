package scripts

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// EncodeSecrets encodes secrets for a script's standard input: one
// "name base64(value)" line per secret, in name order. The scripts read the
// bundle before doing anything else (SE-015).
func EncodeSecrets(secrets map[string][]byte) io.Reader {
	names := make([]string, 0, len(secrets))
	for n := range secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	var b bytes.Buffer
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(' ')
		b.WriteString(base64.StdEncoding.EncodeToString(secrets[n]))
		b.WriteByte('\n')
	}
	return &b
}

// Output is what a script reported on standard output through its
// "::kind:: ..." lines.
type Output struct {
	// Changed lists what the script changed; empty means it was a no-op
	// (FL-030).
	Changed []string
	// Versions are the installed component versions from detect (FL-029).
	Versions config.InstalledVersions
	// ThinPool is true when detect found the thin pool (FL-023).
	ThinPool bool
	// Skipped lists components a step did not install, such as a host agent
	// the pinned release does not ship.
	Skipped []string
	// Inactive lists the units verify_active found not running (FL-053).
	Inactive []string
	// Services are guest_verify's per-service outcomes (FL-109).
	Services []fleet.ServiceCheck
}

// ParseOutput reads the report lines of a script's standard output and
// ignores everything else.
func ParseOutput(stdout string) Output {
	var o Output
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "::") {
			continue
		}
		kind, rest, ok := strings.Cut(strings.TrimPrefix(line, "::"), ":: ")
		if !ok {
			continue
		}
		switch kind {
		case "changed":
			o.Changed = append(o.Changed, rest)
		case "skipped":
			o.Skipped = append(o.Skipped, rest)
		case "inactive":
			name, _, _ := strings.Cut(rest, " ")
			o.Inactive = append(o.Inactive, name)
		case "thinpool":
			o.ThinPool = rest == "present"
		case "version":
			comp, v, _ := strings.Cut(rest, " ")
			if v == "none" {
				v = ""
			}
			switch comp {
			case "flintlock":
				o.Versions.Flintlock = v
			case "firecracker":
				o.Versions.Firecracker = v
			case "cloud_hypervisor":
				o.Versions.CloudHypervisor = v
			case "containerd":
				o.Versions.Containerd = v
			}
		case "service":
			name, verdict, _ := strings.Cut(rest, " ")
			check := fleet.ServiceCheck{Service: name}
			if verdict != "ok" {
				reason := strings.TrimSpace(strings.TrimPrefix(verdict, "fail"))
				if reason == "" {
					reason = "failed"
				}
				check.Err = errors.New(reason)
			}
			o.Services = append(o.Services, check)
		}
	}
	return o
}
