package awsclient

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// RunShellScriptDocument is the Systems Manager document SendCommand runs.
const RunShellScriptDocument = "AWS-RunShellScript"

// StatusPending is reported for an invocation Systems Manager does not know
// yet: GetCommandInvocation answers InvocationDoesNotExist for a short time
// after SendCommand, which callers should treat as not started.
const StatusPending = "Pending"

// maxComment is the SendCommand Comment limit.
const maxComment = 100

// SSM implements fleet.SSM with the SDK. It calls only SendCommand,
// GetCommandInvocation and ListCommandInvocations (SE-040).
type SSM struct {
	api *ssm.Client
}

// NewSSM returns a Systems Manager command client for cfg.
func NewSSM(cfg aws.Config, optFns ...func(*ssm.Options)) *SSM {
	return &SSM{api: ssm.NewFromConfig(cfg, optFns...)}
}

// SendCommand implements fleet.SSM. The script runs through
// AWS-RunShellScript wrapped by RunShellScriptCommand, and in.Timeout becomes
// the document's executionTimeout.
func (c *SSM) SendCommand(ctx context.Context, in fleet.SendCommandInput) (string, error) {
	params := map[string][]string{"commands": {RunShellScriptCommand(in.Script)}}
	if secs := int(in.Timeout.Seconds()); secs > 0 {
		params["executionTimeout"] = []string{strconv.Itoa(secs)}
	}
	comment := in.Comment
	if len(comment) > maxComment {
		comment = comment[:maxComment]
	}
	out, err := c.api.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName: aws.String(RunShellScriptDocument),
		InstanceIds:  append([]string(nil), in.InstanceIDs...),
		Comment:      aws.String(comment),
		Parameters:   params,
	})
	if err != nil {
		return "", fmt.Errorf("ssm SendCommand: %w", err)
	}
	if out.Command == nil || aws.ToString(out.Command.CommandId) == "" {
		return "", errors.New("ssm SendCommand: response has no command id")
	}
	return aws.ToString(out.Command.CommandId), nil
}

// GetCommandInvocation implements fleet.SSM. InvocationDoesNotExist is
// reported as StatusPending rather than as an error.
func (c *SSM) GetCommandInvocation(ctx context.Context, commandID, instanceID string) (*fleet.Invocation, error) {
	out, err := c.api.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
		CommandId:  aws.String(commandID),
		InstanceId: aws.String(instanceID),
	})
	var notYet *ssmtypes.InvocationDoesNotExist
	if errors.As(err, &notYet) {
		return &fleet.Invocation{CommandID: commandID, InstanceID: instanceID, Status: StatusPending, ResponseCode: -1}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ssm GetCommandInvocation: %w", err)
	}
	return &fleet.Invocation{
		CommandID:    commandID,
		InstanceID:   instanceID,
		Status:       string(out.Status),
		ResponseCode: int(out.ResponseCode),
		Stdout:       aws.ToString(out.StandardOutputContent),
		Stderr:       aws.ToString(out.StandardErrorContent),
	}, nil
}

// ListCommandInvocations implements fleet.SSM, following every page. The
// output of an entry is the combined plugin output Systems Manager keeps,
// reported as Stdout.
func (c *SSM) ListCommandInvocations(ctx context.Context, commandID string) ([]fleet.Invocation, error) {
	p := ssm.NewListCommandInvocationsPaginator(c.api, &ssm.ListCommandInvocationsInput{
		CommandId: aws.String(commandID),
		Details:   true,
	})
	var out []fleet.Invocation
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("ssm ListCommandInvocations: %w", err)
		}
		for _, ci := range page.CommandInvocations {
			inv := fleet.Invocation{
				CommandID:    commandID,
				InstanceID:   aws.ToString(ci.InstanceId),
				Status:       string(ci.Status),
				ResponseCode: -1,
			}
			for _, pl := range ci.CommandPlugins {
				inv.ResponseCode = int(pl.ResponseCode)
				inv.Stdout += aws.ToString(pl.Output)
			}
			out = append(out, inv)
		}
	}
	return out, nil
}

// Parameters implements fleet.Parameters with the SDK. It calls only
// GetParameter, always with decryption (SE-040).
type Parameters struct {
	api *ssm.Client
}

// NewParameters returns a parameter client for cfg.
func NewParameters(cfg aws.Config, optFns ...func(*ssm.Options)) *Parameters {
	return &Parameters{api: ssm.NewFromConfig(cfg, optFns...)}
}

// GetParameter implements fleet.Parameters. The value is a secret and is not
// included in any error.
func (c *Parameters) GetParameter(ctx context.Context, name string) (string, error) {
	out, err := c.api.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name), WithDecryption: aws.Bool(true)})
	if err != nil {
		return "", fmt.Errorf("ssm GetParameter %s: %w", name, err)
	}
	if out.Parameter == nil {
		return "", fmt.Errorf("ssm GetParameter %s: response has no parameter", name)
	}
	return aws.ToString(out.Parameter.Value), nil
}

// RunShellScriptCommand wraps a bash script for AWS-RunShellScript, which
// hands its commands to sh. The script is written to a private temporary
// file through a quoted here-document, so nothing in it is expanded by sh,
// and run by bash with standard input from /dev/null so that a command in
// the script cannot read the rest of it. The exit status is the script's.
func RunShellScriptCommand(script string) string {
	delim := "FLINTLOCK_RUNNER_SCRIPT_EOF"
	for n := 0; containsLine(script, delim); n++ {
		delim = "FLINTLOCK_RUNNER_SCRIPT_EOF_" + strconv.Itoa(n)
	}
	if !strings.HasSuffix(script, "\n") {
		script += "\n"
	}
	var b strings.Builder
	b.WriteString("f=$(mktemp) || exit 125\n")
	b.WriteString("cat >\"$f\" <<'" + delim + "'\n")
	b.WriteString(script)
	b.WriteString(delim + "\n")
	b.WriteString("bash \"$f\" </dev/null\n")
	b.WriteString("rc=$?\n")
	b.WriteString("rm -f \"$f\"\n")
	b.WriteString("exit \"$rc\"\n")
	return b.String()
}

func containsLine(s, line string) bool {
	for l := range strings.Lines(s) {
		if strings.TrimSuffix(l, "\n") == line {
			return true
		}
	}
	return false
}
