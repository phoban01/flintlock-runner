package main

import (
	"regexp"
	"strings"
)

// A Job log as the Runner sends it to GitLab is meant for GitLab's log
// viewer: each line carries a timestamp and a stream tag, collapsible
// sections are delimited by section_start/section_end markers followed by
// a carriage return and an erase-line escape, and output is coloured with
// ANSI escapes. traceWriter turns that into lines a terminal shows well.
var (
	// timestampTag matches the per-line prefix of gitlab-runner's
	// timestamped log format: an RFC 3339 UTC time, then a two-digit
	// stream number, O or E for stdout or stderr, and '+' when the line
	// continues the one before it or a space when it starts a new one.
	timestampTag = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z [0-9A-Fa-f]{2}[OE]([+ ]?)`)
	// sectionMarker matches a section marker with the carriage return that
	// hides it in GitLab's viewer.
	sectionMarker = regexp.MustCompile(`section_(?:start|end):\d+:[^\r\n]*\r?`)
	// eraseLine is the erase-line escape that follows a section marker.
	eraseLine = regexp.MustCompile(`\x1b\[0?K`)
	// ansi matches every other CSI escape, which is colour in practice.
	ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
)

// traceWriter renders a Job log arriving in arbitrary chunks as whole
// lines, each passed to emit with prefix in front.
type traceWriter struct {
	emit    func(string)
	prefix  string
	raw     bool
	plain   bool
	pending string
}

// newTraceWriter returns a traceWriter. raw emits lines exactly as
// received; plain also strips colour.
func newTraceWriter(emit func(string), prefix string, raw, plain bool) *traceWriter {
	return &traceWriter{emit: emit, prefix: prefix, raw: raw, plain: plain}
}

// write takes the next piece of the log and emits every line it completes.
func (t *traceWriter) write(chunk string) {
	t.pending += chunk
	i := strings.LastIndexByte(t.pending, '\n')
	if i < 0 {
		return
	}
	complete := t.pending[:i]
	t.pending = t.pending[i+1:]
	t.render(strings.Split(complete, "\n"))
}

// flush emits whatever is left, at the end of the log.
func (t *traceWriter) flush() {
	if t.pending != "" {
		t.render([]string{t.pending})
		t.pending = ""
	}
}

// render emits records, joining a continuation record onto the one before
// it when both arrived together. A continuation arriving in a later chunk
// is shown on a line of its own rather than holding every line back.
func (t *traceWriter) render(records []string) {
	var lines []string
	for _, rec := range records {
		if t.raw {
			lines = append(lines, strings.TrimSuffix(rec, "\r"))
			continue
		}
		continues := false
		if m := timestampTag.FindStringSubmatchIndex(rec); m != nil {
			continues = rec[m[2]:m[3]] == "+"
			rec = rec[m[1]:]
		}
		cleaned := sectionMarker.ReplaceAllString(rec, "")
		cleaned = eraseLine.ReplaceAllString(cleaned, "")
		if t.plain {
			cleaned = ansi.ReplaceAllString(cleaned, "")
		}
		cleaned = strings.TrimRight(cleaned, "\r")
		if continues && len(lines) > 0 {
			lines[len(lines)-1] += cleaned
			continue
		}
		// A line that held nothing but a section marker is noise; a blank
		// line the Job printed is not.
		if cleaned == "" && rec != "" {
			continue
		}
		lines = append(lines, cleaned)
	}
	for _, l := range lines {
		t.emit(t.prefix + l)
	}
}
