package fleet

import "testing"

// TestStepsAreUnique guards the Step constants that Scripts.All enumerates
// for the TD-043 lint job.
func TestStepsAreUnique(t *testing.T) {
	t.Parallel()
	steps := []Step{
		StepDetect, StepFlintlock, StepThinPool, StepNetworking, StepFlintlockd,
		StepPoolAgent, StepHostServices, StepPrepull, StepPrewarm, StepVerifyActive,
		StepControlNode, StepDrain, StepTeardown, StepUserData, StepGuestVerify,
	}
	seen := map[Step]bool{}
	for _, s := range steps {
		if s == "" {
			t.Error("empty step name")
		}
		if seen[s] {
			t.Errorf("duplicate step %q", s)
		}
		seen[s] = true
	}
}
