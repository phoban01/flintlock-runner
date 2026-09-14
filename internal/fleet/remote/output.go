package remote

import (
	"bytes"
	"io"
	"sync"
)

// SyncWriter returns w guarded by a mutex, for an out shared by Runs on
// several instances at once. Each Run writes whole prefixed lines in single
// Write calls, so lines from different instances do not interleave.
func SyncWriter(w io.Writer) io.Writer {
	if _, ok := w.(*syncWriter); ok {
		return w
	}
	return &syncWriter{w: w}
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

//= docs/requirements/06-fleet.md#remote-execution
//# The Fleet Controller SHALL stream each instance's command output
//# to its log prefixed with the instance id.

// linePrefixer writes each complete line written to it to out with prefix,
// as it arrives. A trailing partial line is held until its newline or Flush.
// Several linePrefixers of one Run share mu so that stdout and stderr lines
// reach out whole.
type linePrefixer struct {
	mu     *sync.Mutex
	out    io.Writer
	prefix []byte
	buf    []byte
}

// Prefix is the text put before every line of an instance's output.
func Prefix(instanceID string) string { return "[" + instanceID + "] " }

func newLinePrefixer(mu *sync.Mutex, out io.Writer, instanceID string) *linePrefixer {
	if out == nil {
		out = io.Discard
	}
	return &linePrefixer{mu: mu, out: out, prefix: []byte(Prefix(instanceID))}
}

func (l *linePrefixer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		if err := l.emit(l.buf[:i+1]); err != nil {
			return 0, err
		}
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}

// Flush writes a held partial line, ending it with a newline.
func (l *linePrefixer) Flush() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) == 0 {
		return nil
	}
	line := append(l.buf, '\n')
	l.buf = nil
	return l.emit(line)
}

func (l *linePrefixer) emit(line []byte) error {
	msg := make([]byte, 0, len(l.prefix)+len(line))
	msg = append(msg, l.prefix...)
	msg = append(msg, line...)
	_, err := l.out.Write(msg)
	return err
}
