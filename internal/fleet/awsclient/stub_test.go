package awsclient

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// stubReply is one canned AWS response.
type stubReply struct {
	status int
	body   string
}

// stubRequest is one request the SDK sent, reduced to the IAM action and the
// decoded parameters.
type stubRequest struct {
	Action string
	// Form holds an EC2 query request's parameters.
	Form url.Values
	// JSON holds a Systems Manager request body.
	JSON string
}

// stubAWS is an aws.HTTPClient that answers in process, so that the real
// clients are exercised without any network. It maps each request to the IAM
// action it needs: EC2's query protocol names it in the Action parameter and
// Systems Manager's JSON protocol in the X-Amz-Target header.
type stubAWS struct {
	mu       sync.Mutex
	requests []stubRequest
	replies  map[string][]stubReply
}

func newStubAWS() *stubAWS { return &stubAWS{replies: map[string][]stubReply{}} }

// reply queues a response for action; the last queued response repeats.
func (s *stubAWS) reply(action string, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies[action] = append(s.replies[action], stubReply{status: status, body: body})
}

func (s *stubAWS) Do(r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	req := stubRequest{}
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		svc, op, _ := strings.Cut(target, ".")
		if svc != "AmazonSSM" {
			return nil, fmt.Errorf("unexpected target %q", target)
		}
		req.Action = "ssm:" + op
		req.JSON = string(body)
	} else {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		req.Action = "ec2:" + form.Get("Action")
		req.Form = form
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	queue := s.replies[req.Action]
	var rep stubReply
	switch len(queue) {
	case 0:
		s.mu.Unlock()
		return nil, fmt.Errorf("no stub reply for %s", req.Action)
	case 1:
		rep = queue[0]
	default:
		rep = queue[0]
		s.replies[req.Action] = queue[1:]
	}
	s.mu.Unlock()
	h := http.Header{}
	if req.JSON != "" || strings.HasPrefix(req.Action, "ssm:") {
		h.Set("Content-Type", "application/x-amz-json-1.1")
	} else {
		h.Set("Content-Type", "text/xml;charset=UTF-8")
	}
	return &http.Response{
		StatusCode:    rep.status,
		Status:        http.StatusText(rep.status),
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader([]byte(rep.body))),
		ContentLength: int64(len(rep.body)),
		Request:       r,
	}, nil
}

func (s *stubAWS) sent() []stubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubRequest(nil), s.requests...)
}

// stubConfig is an aws.Config that sends through stub with fixed
// credentials and no retries.
func stubConfig(t *testing.T, stub *stubAWS) aws.Config {
	t.Helper()
	return aws.Config{
		Region:      "eu-west-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDSTUB", "stub-secret", ""),
		HTTPClient:  stub,
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	}
}

const describeInstancesXML = `<?xml version="1.0" encoding="UTF-8"?>
<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <requestId>req-1</requestId>
  <reservationSet>
    <item>
      <reservationId>r-1</reservationId>
      <instancesSet>
        <item>
          <instanceId>i-arm</instanceId>
          <instanceType>m7g.metal</instanceType>
          <architecture>arm64</architecture>
          <privateIpAddress>10.0.1.10</privateIpAddress>
          <instanceState><code>16</code><name>running</name></instanceState>
          <tagSet><item><key>flintlock-runner</key><value>ci</value></item></tagSet>
          <cpuOptions><coreCount>64</coreCount><threadsPerCore>1</threadsPerCore></cpuOptions>
        </item>
        <item>
          <instanceId>i-x86</instanceId>
          <instanceType>c5.metal</instanceType>
          <architecture>x86_64</architecture>
          <privateIpAddress>10.0.1.11</privateIpAddress>
          <instanceState><code>16</code><name>running</name></instanceState>
          <cpuOptions><coreCount>48</coreCount><threadsPerCore>2</threadsPerCore></cpuOptions>
        </item>
        <item>
          <instanceId>i-386</instanceId>
          <instanceType>c5.metal</instanceType>
          <architecture>i386</architecture>
          <privateIpAddress>10.0.1.12</privateIpAddress>
          <instanceState><code>16</code><name>running</name></instanceState>
        </item>
      </instancesSet>
    </item>
  </reservationSet>
</DescribeInstancesResponse>`

const terminateInstancesXML = `<?xml version="1.0" encoding="UTF-8"?>
<TerminateInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <requestId>req-2</requestId>
  <instancesSet/>
</TerminateInstancesResponse>`
