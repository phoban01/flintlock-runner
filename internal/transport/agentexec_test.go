package transport_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The `agent-exec` Guest Transport SHALL treat a response that
//# ends without an exit status frame as a stream failure, as EX-023 requires.

// TestAgentExecWithoutAnExitStatusIsAStreamFailure builds the agent-exec
// transport over a scripted exchange that ends cleanly, as a cut session
// through a relay would, after output and after nothing at all. Neither is
// a finished command: both are stream failures with no status, and the
// output that did arrive is still written. The Exec Agent's own tests run
// the same cuts through the real agent.
func TestAgentExecWithoutAnExitStatusIsAStreamFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	for name, stream := range map[string]*scriptedStream{
		"after output":   newScriptedStream(stdout("partial")),
		"with no output": newScriptedStream(),
	} {
		stub := &stubHost{name: "agent", exec: func(context.Context) (flintlock.ExecStream, error) { return stream, nil }}
		tr := newExecTransport(t, transport.Target{Kind: transport.KindAgentExec, Host: stub, VMUID: "vm"})
		var out bytes.Buffer
		status, err := tr.Run(ctx, transport.Command{Path: "sh", Stdout: &out})
		if !errors.Is(err, transport.ErrStreamFailed) || status != -1 {
			t.Errorf("%s: Run = (%d, %v), want a stream failure with no status", name, status, err)
		}
		if name == "after output" && out.String() != "partial" {
			t.Errorf("%s: output %q was not written", name, out.String())
		}
	}
}

//= docs/requirements/12-cluster-fleet.md#agent-exec-transport
//= type=test
//# The `agent-exec` Guest Transport SHALL authenticate to the Exec
//# Agent with the Runner's ServiceAccount token and SHALL verify the agent's
//# serving certificate against the configured certificate authority.

// TestAgentExecCredentials checks that the token is sent as a bearer token,
// read afresh on every call so that a rotated token is used at once, that
// the credentials require TLS, and that the client pool refuses to be built
// without a certificate authority or a token, and shares one client per
// agent until its last user releases it.
func TestAgentExecCredentials(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	creds := transport.TokenFileCredentials(path)
	if !creds.RequireTransportSecurity() {
		t.Error("the token may be sent without TLS")
	}
	if _, err := creds.GetRequestMetadata(context.Background()); err == nil {
		t.Error("a missing token file gave credentials")
	}
	for _, token := range []string{"first", "rotated"} {
		if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		md, err := creds.GetRequestMetadata(context.Background())
		if err != nil || md["authorization"] != "Bearer "+token {
			t.Errorf("metadata = (%v, %v), want the bearer token %q", md, err, token)
		}
	}

	if _, err := transport.NewAgentHosts(transport.AgentExecConfig{TokenFile: path}); err == nil {
		t.Error("the pool was built without a certificate authority")
	}
	certs, err := fake.WriteTestCerts(filepath.Join(dir, "certs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.NewAgentHosts(transport.AgentExecConfig{CAFile: certs.CAFile}); err == nil {
		t.Error("the pool was built without a token")
	}
	agents, err := transport.NewAgentHosts(transport.AgentExecConfig{CAFile: certs.CAFile, TokenFile: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agents.Close() }()
	if _, _, err := agents.Lease("host-1", ""); err == nil {
		t.Error("a claim with no agent address gave a client")
	}
	a, releaseA, err := agents.Lease("host-1", "127.0.0.1:10270")
	if err != nil {
		t.Fatal(err)
	}
	b, releaseB, err := agents.Lease("host-1", "127.0.0.1:10270")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("two jobs on one host got two connections to its agent")
	}
	releaseA()
	releaseA()
	c, releaseC, err := agents.Lease("host-1", "127.0.0.1:10270")
	if err != nil {
		t.Fatal(err)
	}
	if c != b {
		t.Error("the client was closed while a job still held it")
	}
	releaseB()
	releaseC()
}
