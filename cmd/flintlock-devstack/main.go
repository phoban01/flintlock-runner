// Command flintlock-devstack runs flintlock-runner end to end on a laptop,
// without KVM, EC2 or a GitLab instance. It starts the fake GitLab, the fake
// Pool Manager and fake flintlock Hosts on loopback ports, runs the real
// flintlock-runner binary against them, queues one or more Jobs, streams each
// Job's log as the Runner sends it, prints the final state and shuts
// everything down, checking that no Lease is held and no MicroVM sandbox is
// left behind (TD-054). It is the end-to-end harness
// (internal/testing/harness) with a human watching.
//
// Usage:
//
//	flintlock-devstack [flags]
//
//	make demo                                    # one hello-world job
//	make demo DEMO_ARGS='-jobs 3'                # three jobs
//	make demo DEMO_ARGS="-script 'exit 3'"       # a failing job
//	make demo DEMO_ARGS='-keep'                  # leave the stack up to poke at
//
// The exit status is 0 when every Job succeeded and the shutdown was clean,
// 1 otherwise, and 2 for a usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
	"github.com/phoban01/flintlock-runner/internal/testing/harness"
)

// fakesLog is the file in the stack's directory the fakes log to.
const fakesLog = "fakes.log"

// defaultScript is the hello-world Job.
var defaultScript = []string{
	`echo "Hello from job $CI_JOB_ID, running in a flintlock MicroVM (a fake one: this is a local process)"`,
	`echo "kernel: $(uname -sr), user: $(id -un), cwd: $(pwd)"`,
	`for i in 1 2 3; do echo "working... step $i of 3"; sleep 1; done`,
	`echo "done"`,
}

// options is the parsed command line.
type options struct {
	jobs       int
	script     []string
	hosts      int
	poolSize   int
	runnerBin  string
	keep       bool
	timeout    time.Duration
	runnerLogs bool
	raw        bool
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func parseFlags(args []string, stderr io.Writer) (*options, error) {
	fs := flag.NewFlagSet("flintlock-devstack", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := &options{}
	var script, scriptFile string
	fs.IntVar(&o.jobs, "jobs", 1, "number of jobs to run")
	fs.StringVar(&script, "script", "", "the job script, one shell command per line (default: a hello-world script)")
	fs.StringVar(&scriptFile, "script-file", "", "read the job script from this file")
	fs.IntVar(&o.hosts, "hosts", harness.DefaultHosts, "number of fake flintlock Hosts")
	fs.IntVar(&o.poolSize, "pool-size", harness.DefaultPoolSize, "warm MicroVMs in the pool")
	fs.StringVar(&o.runnerBin, "runner-bin", "", "prebuilt flintlock-runner binary (default: go build it)")
	fs.BoolVar(&o.keep, "keep", false, "leave the stack running after the jobs, until Ctrl-C")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Minute, "give up on a job after this long")
	fs.BoolVar(&o.runnerLogs, "runner-logs", false, "copy the runner's own log to stderr as well as to its log file")
	fs.BoolVar(&o.raw, "raw", false, "print job logs exactly as GitLab receives them (timestamps, ANSI, section markers)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: flintlock-devstack [flags]\n\n"+
			"Runs flintlock-runner against a local fake GitLab, fake Pool Manager and fake\n"+
			"flintlock Hosts, queues jobs and streams their logs. No KVM needed: the fake\n"+
			"Hosts run each job's script as a local process.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if o.jobs < 0 || o.hosts < 1 || o.poolSize < 1 {
		return nil, errors.New("-jobs must be >= 0, -hosts and -pool-size >= 1")
	}
	switch {
	case script != "" && scriptFile != "":
		return nil, errors.New("-script and -script-file are mutually exclusive")
	case scriptFile != "":
		data, err := os.ReadFile(scriptFile)
		if err != nil {
			return nil, err
		}
		script = string(data)
	}
	if script != "" {
		o.script = scriptLines(script)
	}
	if len(o.script) == 0 {
		o.script = defaultScript
	}
	return o, nil
}

// scriptLines splits a script into its non-empty lines.
func scriptLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// ui prints progress lines. Its methods are safe for concurrent use, so
// that the traces of concurrent jobs interleave by line, never mid-line.
type ui struct {
	mu    sync.Mutex
	out   io.Writer
	color bool
}

func (u *ui) step(format string, args ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	prefix := "==> "
	if u.color {
		prefix = "\x1b[1;34m==>\x1b[0m "
	}
	fmt.Fprintf(u.out, prefix+format+"\n", args...)
}

func (u *ui) line(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprintln(u.out, s)
}

func (u *ui) paint(code, s string) string {
	if !u.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// isTerminal reports whether f is a character device other than the null
// device, which is close enough to "a terminal" to decide on colour and on
// reading commands without a terminal library.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(info, null)
}

func run(args []string, stdout, stderr io.Writer) int {
	o, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "flintlock-devstack: %v\n", err)
		return 2
	}
	tty := false
	if f, ok := stdout.(*os.File); ok {
		tty = isTerminal(f)
	}
	u := &ui{out: stdout, color: tty && os.Getenv("NO_COLOR") == ""}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	u.step("flintlock-devstack: fake GitLab, fake Pool Manager and %d fake flintlock Host(s), with the real flintlock-runner", o.hosts)
	// The fakes log through slog's default logger; their lines go to a
	// file in the stack's directory so that the terminal shows the jobs.
	root, err := os.MkdirTemp("", "flintlock-devstack-")
	if err != nil {
		fmt.Fprintf(stderr, "flintlock-devstack: %v\n", err)
		return 1
	}
	logFile, err := os.Create(filepath.Join(root, fakesLog))
	if err != nil {
		_ = os.RemoveAll(root)
		fmt.Fprintf(stderr, "flintlock-devstack: %v\n", err)
		return 1
	}
	defer func() { _ = logFile.Close() }()
	slog.SetDefault(slog.New(slog.NewTextHandler(logFile, nil)))

	opts := harness.Options{
		Hosts:        o.hosts,
		PoolSize:     o.poolSize,
		BootDelay:    harness.DefaultBootDelay,
		RunnerBinary: o.runnerBin,
		Root:         root,
		Logf:         u.step,
	}
	if o.runnerLogs {
		opts.RunnerOutput = stderr
	}
	s, err := harness.Start(ctx, opts)
	if err != nil {
		fmt.Fprintf(stderr, "flintlock-devstack: %v\n", err)
		return 1
	}
	ok := session(ctx, s, u, o, stderr)

	u.step("shutting down: flintlock-runner first (SIGTERM), then the fakes")
	if err := s.Shutdown(context.Background()); err != nil {
		u.line(u.paint("1;31", "shutdown check failed:"))
		u.line(err.Error())
		ok = false
	}
	if !ok {
		return 1
	}
	return 0
}

// session runs the Runner and the jobs on a started stack and reports
// whether every job succeeded. The caller shuts the stack down.
func session(ctx context.Context, s *harness.Stack, u *ui, o *options, stderr io.Writer) bool {
	if err := s.StartRunner(ctx); err != nil {
		fmt.Fprintf(stderr, "flintlock-devstack: %v\n", err)
		return false
	}
	u.step("waiting for flintlock-runner to declare its pool and ask for a job")
	if err := s.WaitReady(ctx); err != nil {
		fmt.Fprintf(stderr, "flintlock-devstack: %v\n", err)
		return false
	}

	ok := summarise(u, runJobs(ctx, s, u, o, o.jobs))
	if ctx.Err() != nil {
		u.step("interrupted")
		return false
	}
	if o.keep {
		// Ctrl-C is how keep mode ends, so it is not a failure here.
		keep(ctx, s, u, o)
	}
	return ok
}

// result is how one job ended.
type result struct {
	id      int64
	status  string
	reason  string
	exit    int
	elapsed time.Duration
	err     error
}

// runJobs queues n jobs at once, so they run as concurrently as the pool
// allows, streams their logs and waits for all of them.
func runJobs(ctx context.Context, s *harness.Stack, u *ui, o *options, n int) []result {
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := range n {
		name := "hello-world"
		if len(o.script) != len(defaultScript) || o.script[0] != defaultScript[0] {
			name = "custom-script"
		}
		id, err := s.Enqueue(harness.Job{Name: name, Script: o.script})
		if err != nil {
			results[i] = result{err: err}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = follow(ctx, s, u, o, id, n > 1)
		}()
	}
	wg.Wait()
	return results
}

// follow streams one job's log and reports how it ended.
func follow(ctx context.Context, s *harness.Stack, u *ui, o *options, id int64, prefixed bool) result {
	start := time.Now()
	prefix := ""
	if prefixed {
		prefix = u.paint("2", fmt.Sprintf("job %d | ", id))
	}
	tw := newTraceWriter(u.line, prefix, o.raw, !u.color)
	jobCtx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	rec, err := s.Follow(jobCtx, id, tw.write)
	tw.flush()
	r := result{id: id, elapsed: time.Since(start).Round(100 * time.Millisecond), err: err}
	if rec != nil {
		r.status, r.reason, r.exit = rec.Status, rec.FailureReason, rec.ExitCode
	}
	switch {
	case err != nil:
		u.step("job %d: %s", id, u.paint("1;31", "did not finish: "+err.Error()))
	case r.status == fakegitlab.StatusSuccess:
		u.step("job %d: %s in %s", id, u.paint("1;32", "success"), r.elapsed)
	default:
		u.step("job %d: %s (%s, exit code %d) in %s", id, u.paint("1;31", r.status), r.reason, r.exit, r.elapsed)
	}
	return r
}

// summarise prints the tally and reports whether every job succeeded.
func summarise(u *ui, results []result) bool {
	ok := 0
	for _, r := range results {
		if r.err == nil && r.status == fakegitlab.StatusSuccess {
			ok++
		}
	}
	if len(results) > 0 {
		u.step("%d of %d job(s) succeeded", ok, len(results))
	}
	return ok == len(results)
}

// keep leaves the stack up until ctx ends, printing how to reach it, and on
// a terminal runs each line typed as a new job.
func keep(ctx context.Context, s *harness.Stack, u *ui, o *options) {
	u.step("the stack is still running; Ctrl-C stops it and checks for leaks")
	u.line("    runner config   " + s.ConfigPath)
	u.line("    runner log      " + s.RunnerLog)
	u.line("    working dir     " + s.Root)
	u.line("    GitLab (fake)   " + s.GitLab.URL() + "   runner token " + harness.RunnerToken)
	u.line("    Pool Manager    " + s.PoolManagerAddr())
	for _, h := range s.Inventory {
		u.line(fmt.Sprintf("    Host %-10s %s   basic auth token %s", h.Name, h.Endpoint, string(h.Token)))
	}
	u.line("    runner metrics  http://" + s.Config.Observability.ListenAddress + "/metrics")
	u.line("    fakes' log      " + filepath.Join(s.Root, fakesLog))
	u.line("    try             flintlock-runner --config " + s.ConfigPath + " config show")
	if !isTerminal(os.Stdin) {
		<-ctx.Done()
		return
	}
	u.step("type a shell command and press Enter to run it as a new job (Ctrl-D or Ctrl-C to stop)")
	lines := make(chan string)
	go func() {
		defer close(lines)
		buf := make([]byte, 64*1024)
		var pending string
		for {
			n, err := os.Stdin.Read(buf)
			pending += string(buf[:n])
			for {
				i := strings.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				select {
				case lines <- pending[:i]:
				case <-ctx.Done():
					return
				}
				pending = pending[i+1:]
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-lines:
			if !ok {
				return
			}
			if strings.TrimSpace(line) == "" {
				continue
			}
			id, err := s.Enqueue(harness.Job{Name: "interactive", Script: []string{line}})
			if err != nil {
				u.step("could not queue the job: %v", err)
				continue
			}
			follow(ctx, s, u, o, id, false)
			u.step("next command (Ctrl-D or Ctrl-C to stop):")
		}
	}
}
