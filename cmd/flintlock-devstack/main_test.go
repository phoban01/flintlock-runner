package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// sampleTrace is the start and end of a real Job log from flintlock-runner,
// as the fake GitLab assembled it: timestamped records, a collapsed
// section, a continuation record after a section end, and colour.
const sampleTrace = "" +
	"2026-09-11T11:17:01.923460Z 00O \x1b[0KRunning with gitlab-runner v0.0.0 (c82c5912)\x1b[0;m\n" +
	"2026-09-11T11:17:01.923494Z 00O section_start:1789125421:flintlock_prepare[collapsed=true]\r\x1b[0K\x1b[36;1mPreparing the flintlock microvm\x1b[0;m\n" +
	"2026-09-11T11:17:01.962557Z 00O \x1b[0KHost services: none\x1b[0;m\n" +
	"2026-09-11T11:17:01.962575Z 00O section_end:1789125421:flintlock_prepare\r\x1b[0K\n" +
	"2026-09-11T11:17:01.962840Z 00O+\x1b[0K\x1b[36;1mPreparing environment\x1b[0;m\x1b[0;m\n" +
	"2026-09-11T11:17:02.145771Z 01O Hello from job 1\n" +
	"2026-09-11T11:17:02.145772Z 01O \n" +
	"2026-09-11T11:17:05.251435Z 00O \x1b[32;1mJob succeeded\x1b[0;m\n"

func renderAll(t *testing.T, raw, plain bool, chunks ...string) []string {
	t.Helper()
	var got []string
	tw := newTraceWriter(func(s string) { got = append(got, s) }, "job 1 | ", raw, plain)
	for _, c := range chunks {
		tw.write(c)
	}
	tw.flush()
	return got
}

func TestTraceWriterPlain(t *testing.T) {
	want := []string{
		"job 1 | Running with gitlab-runner v0.0.0 (c82c5912)",
		"job 1 | Preparing the flintlock microvm",
		"job 1 | Host services: none",
		"job 1 | Preparing environment",
		"job 1 | Hello from job 1",
		"job 1 | ",
		"job 1 | Job succeeded",
	}
	if got := renderAll(t, false, true, sampleTrace); !slices.Equal(got, want) {
		t.Errorf("rendered\n%q\nwant\n%q", got, want)
	}
	// The log arrives in pieces cut anywhere; a line is only shown whole.
	var chunks []string
	for rest := sampleTrace; rest != ""; {
		n := min(7, len(rest))
		chunks = append(chunks, rest[:n])
		rest = rest[n:]
	}
	got := renderAll(t, false, true, chunks...)
	for _, l := range got {
		if strings.Contains(l, "2026-09-11T") || strings.Contains(l, "section_") || strings.Contains(l, "\x1b") {
			t.Errorf("chunked rendering left markup in %q", l)
		}
	}
	if !slices.Contains(got, "job 1 | Hello from job 1") || !slices.Contains(got, "job 1 | Job succeeded") {
		t.Errorf("chunked rendering lost lines: %q", got)
	}
}

func TestTraceWriterKeepsColourOnATerminal(t *testing.T) {
	got := renderAll(t, false, false, sampleTrace)
	if want := "job 1 | \x1b[32;1mJob succeeded\x1b[0;m"; got[len(got)-1] != want {
		t.Errorf("last line %q, want %q", got[len(got)-1], want)
	}
	for _, l := range got {
		if strings.Contains(l, "section_") || strings.Contains(l, "\r") || strings.Contains(l, "\x1b[0K") {
			t.Errorf("section markup left in %q; its carriage return would erase the prefix", l)
		}
	}
}

func TestTraceWriterRaw(t *testing.T) {
	got := renderAll(t, true, false, sampleTrace)
	if len(got) != strings.Count(sampleTrace, "\n") || !strings.HasPrefix(got[0], "job 1 | 2026-09-11T11:17:01.923460Z 00O ") {
		t.Errorf("raw rendering changed the records: %q", got)
	}
}

func TestParseFlags(t *testing.T) {
	o, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.jobs != 1 || !slices.Equal(o.script, defaultScript) || o.keep {
		t.Errorf("defaults = %+v, want one hello-world job", o)
	}

	o, err = parseFlags([]string{"-jobs", "3", "-script", "echo a\n\nexit 3"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.jobs != 3 || !slices.Equal(o.script, []string{"echo a", "exit 3"}) {
		t.Errorf("-jobs 3 -script: %+v", o)
	}

	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte("make test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if o, err := parseFlags([]string{"-script-file", path}, io.Discard); err != nil || !slices.Equal(o.script, []string{"make test"}) {
		t.Errorf("-script-file: %+v, %v", o, err)
	}
	for _, bad := range [][]string{{"-script", "x", "-script-file", path}, {"-hosts", "0"}, {"extra"}} {
		if _, err := parseFlags(bad, io.Discard); err == nil {
			t.Errorf("parseFlags(%q) accepted a bad command line", bad)
		}
	}
}
