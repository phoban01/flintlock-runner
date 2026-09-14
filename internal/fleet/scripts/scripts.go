package scripts

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/phoban01/flintlock-runner/internal/fleet"
)

//go:embed templates/*.sh.tmpl
var templateFS embed.FS

// DefaultTimeout bounds a script run when a step sets nothing longer.
const DefaultTimeout = 30 * time.Minute

// templateFor names the template that renders each Step. Every Step has one,
// so that All can hand CI the full list (TD-043).
var templateFor = map[fleet.Step]string{
	fleet.StepDetect:       detectTemplate,
	fleet.StepFlintlock:    flintlockTemplate,
	fleet.StepThinPool:     thinPoolTemplate,
	fleet.StepNetworking:   networkingTemplate,
	fleet.StepFlintlockd:   flintlockdTemplate,
	fleet.StepHostServices: hostServicesTemplate,
	fleet.StepPrepull:      prepullTemplate,
	fleet.StepPrewarm:      prewarmTemplate,
	fleet.StepVerifyActive: verifyActiveTemplate,
	fleet.StepControlNode:  controlNodeTemplate,
	fleet.StepDrain:        drainTemplate,
	fleet.StepTeardown:     teardownTemplate,
	fleet.StepUserData:     userDataTemplate,
	fleet.StepGuestVerify:  guestVerifyTemplate,
	fleet.StepRunner:       runnerTemplate,
}

// allSteps is every Step in the order the Provisioner runs the host steps,
// followed by the steps other commands run.
var allSteps = []fleet.Step{
	fleet.StepDetect,
	fleet.StepThinPool,
	fleet.StepFlintlock,
	fleet.StepNetworking,
	fleet.StepFlintlockd,
	fleet.StepHostServices,
	fleet.StepPrepull,
	fleet.StepPrewarm,
	fleet.StepVerifyActive,
	fleet.StepControlNode,
	fleet.StepDrain,
	fleet.StepTeardown,
	fleet.StepUserData,
	fleet.StepGuestVerify,
	fleet.StepRunner,
}

// Set is the fleet.Scripts implementation: the embedded templates, parsed
// once.
type Set struct {
	tmpl *template.Template
}

var _ fleet.Scripts = (*Set)(nil)

// New parses the embedded templates.
func New() (*Set, error) {
	t, err := template.New("scripts").Option("missingkey=error").Funcs(funcs).ParseFS(templateFS, "templates/*.sh.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parse script templates: %w", err)
	}
	for step, name := range templateFor {
		if t.Lookup(name) == nil {
			return nil, fmt.Errorf("step %s: template %s missing", step, name)
		}
	}
	return &Set{tmpl: t}, nil
}

//= docs/requirements/10-test-doubles.md#fake-aws
//# Every provisioning script the Fleet Controller sends to a Host
//# SHALL pass `bash -n` and `shellcheck` in continuous integration.

// All lists every Step, so that CI renders each and runs `bash -n` and
// shellcheck over the result (TD-043).
func (s *Set) All() []fleet.Step {
	return append([]fleet.Step(nil), allSteps...)
}

// Render renders the script for step. The content never carries a secret:
// secrets reach the Host on the script's standard input (see
// EncodeSecrets) or through the Systems Manager parameters it names.
func (s *Set) Render(step fleet.Step, in fleet.RenderInput) (fleet.Script, error) {
	content, err := s.render(step, in, false)
	if err != nil {
		return fleet.Script{}, err
	}
	return fleet.Script{
		Name:    string(step),
		Content: content,
		Timeout: DefaultTimeout,
	}, nil
}

// userDataSteps are the Host provisioning steps the user-data script runs
// at first boot, in order.
var userDataSteps = []fleet.Step{
	fleet.StepThinPool,
	fleet.StepFlintlock,
	fleet.StepNetworking,
	fleet.StepFlintlockd,
	fleet.StepHostServices,
	fleet.StepPrepull,
	fleet.StepPrewarm,
	fleet.StepVerifyActive,
}

// render renders one step. atBoot is set for the steps embedded in
// user-data, where the instance is not known yet and its architecture and
// address are detected on the Host.
func (s *Set) render(step fleet.Step, in fleet.RenderInput, atBoot bool) (string, error) {
	name, ok := templateFor[step]
	if !ok {
		return "", fmt.Errorf("unknown step %q", step)
	}
	d, err := newData(step, in, atBoot)
	if err != nil {
		return "", fmt.Errorf("step %s: %w", step, err)
	}
	if step == fleet.StepUserData {
		for _, sub := range userDataSteps {
			c, err := s.render(sub, in, true)
			if err != nil {
				return "", err
			}
			if strings.Contains(c, "\nFLR_STEP_EOF\n") {
				return "", fmt.Errorf("step %s contains the user-data delimiter", sub)
			}
			d.Steps = append(d.Steps, renderedStep{Name: string(sub), Content: strings.TrimSuffix(c, "\n")})
		}
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, d); err != nil {
		return "", fmt.Errorf("render step %s: %w", step, err)
	}
	return buf.String(), nil
}

// funcs are the template helpers. Every value interpolated into shell goes
// through q, and every value written into a configuration file goes through
// line, so that no configuration value can break out of its context.
var funcs = template.FuncMap{
	"q":    shellQuote,
	"line": oneLine,
	"join": strings.Join,
	// trimv drops a leading "v" from a version.
	"trimv": func(v string) string { return strings.TrimPrefix(v, "v") },
	// heredoc escapes what an unquoted here-document would expand.
	"heredoc": func(v string) string {
		v = strings.ReplaceAll(v, `\`, `\\`)
		v = strings.ReplaceAll(v, "$", `\$`)
		return strings.ReplaceAll(v, "`", "\\`")
	},
	"qjoin": func(vs []string) string {
		out := make([]string, len(vs))
		for i, v := range vs {
			out[i] = shellQuote(v)
		}
		return strings.Join(out, " ")
	},
}

// shellQuote quotes v as one bash word.
func shellQuote(v any) string {
	s := fmt.Sprint(v)
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// oneLine returns v unchanged, or an error if it could end the line or the
// quoted string it is written into.
func oneLine(v any) (string, error) {
	s := fmt.Sprint(v)
	if strings.ContainsAny(s, "\n\r\"'`$\\") {
		return "", fmt.Errorf("value %q contains a newline, quote, backslash or $", s)
	}
	return s, nil
}
