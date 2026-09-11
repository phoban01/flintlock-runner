package executor

import (
	"fmt"
	"strings"

	"gitlab.com/gitlab-org/gitlab-runner/common/buildlogger"
	"gitlab.com/gitlab-org/gitlab-runner/helpers"

	"github.com/phoban01/flintlock-runner/internal/clock"
)

//= docs/requirements/02-executor.md#prepare
//# The Executor SHALL write a collapsible section to the Job log
//# during `Prepare` that names the Profile, the Pool, the Host name, the
//# MicroVM uid and the time taken to become ready.

// prepareSection is the flintlock_prepare section of the Job log. It opens
// collapsed when Prepare starts, takes one line per step as the step
// completes (the Profile and its Pool, the MicroVM and its Host, the time to
// readiness, the Host Services), and closes when Prepare returns, whatever
// the outcome. The report it accumulates carries no endpoint and no secret.
type prepareSection struct {
	log    *buildlogger.Logger
	clk    clock.Clock
	report PrepareReport
}

// newPrepareSection opens the section.
func newPrepareSection(log *buildlogger.Logger, clk clock.Clock) *prepareSection {
	s := &prepareSection{log: log, clk: clk}
	s.log.SendRawLog(fmt.Sprintf("section_start:%d:%s[collapsed=true]\r%s%sPreparing the flintlock microvm%s\n",
		clk.Now().Unix(), PrepareSection, helpers.ANSI_CLEAR, helpers.ANSI_BOLD_CYAN, helpers.ANSI_RESET))
	return s
}

// printf writes one line into the section.
func (s *prepareSection) printf(format string, args ...any) {
	s.log.Println(fmt.Sprintf(format, args...))
}

// errorf writes one error line into the section.
func (s *prepareSection) errorf(format string, args ...any) {
	s.log.Errorln(fmt.Sprintf(format, args...))
}

// end closes the section.
func (s *prepareSection) end() {
	s.log.SendRawLog(fmt.Sprintf("section_end:%d:%s\r%s", s.clk.Now().Unix(), PrepareSection, helpers.ANSI_CLEAR))
}

// servicesLine is the EX-064 line: the Host Services the Job can use.
func servicesLine(services []string) string {
	if len(services) == 0 {
		return "Host services: none"
	}
	return "Host services: " + strings.Join(services, ", ")
}
