package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// RequireShellcheckEnv makes TestScriptsLint fail instead of skipping the
// shellcheck half when shellcheck is not installed. CI sets it (TD-043).
const RequireShellcheckEnv = "FLINTLOCK_RUNNER_REQUIRE_SHELLCHECK"

//= docs/requirements/10-test-doubles.md#fake-aws
//= type=test
//# Every provisioning script the Fleet Controller sends to a Host
//# SHALL pass `bash -n` and `shellcheck` in continuous integration.

// TestScriptsLint renders every Step that Scripts.All lists with each
// representative input, and runs `bash -n` and shellcheck over the result.
// Nothing is executed: `bash -n` only parses. CI runs this test with
// FLINTLOCK_RUNNER_REQUIRE_SHELLCHECK=1.
func TestScriptsLint(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash is required: %v", err)
	}
	shellcheck, err := exec.LookPath("shellcheck")
	if err != nil {
		if os.Getenv(RequireShellcheckEnv) != "" {
			t.Fatalf("%s is set but shellcheck is not installed", RequireShellcheckEnv)
		}
		t.Logf("shellcheck not installed; running bash -n only")
		shellcheck = ""
	}
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	remove := runnerInput()
	remove.Options[OptionRunnerRemove] = "true"
	inputs := map[string]fleet.RenderInput{"full": fullInput(), "sparse": sparseInput(), "public": publicGoInput(),
		"runner": runnerInput(), "runner-remove": remove}
	if len(s.All()) != len(templateFor) {
		t.Fatalf("All lists %d steps, %d templates", len(s.All()), len(templateFor))
	}
	dir := t.TempDir()
	var files []string
	for name, in := range inputs {
		for _, step := range s.All() {
			sc, err := s.Render(step, in)
			if err != nil {
				t.Fatalf("%s/%s: %v", name, step, err)
			}
			p := filepath.Join(dir, name+"-"+string(step)+".sh")
			if err := os.WriteFile(p, []byte(sc.Content), 0o600); err != nil {
				t.Fatal(err)
			}
			files = append(files, p)
			if out, err := exec.Command(bash, "-n", p).CombinedOutput(); err != nil {
				t.Errorf("bash -n %s/%s: %v\n%s", name, step, err, out)
			}
		}
	}
	if shellcheck == "" {
		return
	}
	args := append([]string{"--shell=bash", "--severity=style", "--format=gcc"}, files...)
	if out, err := exec.Command(shellcheck, args...).CombinedOutput(); err != nil {
		t.Errorf("shellcheck: %v\n%s", err, strings.ReplaceAll(string(out), dir+"/", ""))
	}
}
