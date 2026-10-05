package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
)

// clientTimeout is the per-attempt http.Client.Timeout in these tests, and
// slowAnswer is how long the fake holds back an answer it is told to delay.
const (
	clientTimeout = 150 * time.Millisecond
	slowAnswer    = 600 * time.Millisecond
)

// countingTransport counts the attempts the client makes, whether or not
// they reach the server.
type countingTransport struct{ attempts atomic.Int32 }

func (t *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.attempts.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

// timeoutClient talks to the fake with a short per-attempt timeout and no
// backoff, and counts its attempts.
func timeoutClient(t *testing.T, fake *flightdecktest.Server, timeout time.Duration, maxRetries int) (*Client, *countingTransport) {
	t.Helper()
	transport := &countingTransport{}
	c, err := New(fake.URL, fake.Token(),
		WithHTTPClient(&http.Client{Timeout: timeout, Transport: transport}),
		WithMaxRetries(maxRetries))
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c, transport
}

// served waits for the fake to record want requests to path, then a little
// longer, and returns how many it recorded: a request whose answer the
// client gave up on is recorded once the fake notices.
func served(t *testing.T, fake *flightdecktest.Server, method, path string, want int) int {
	t.Helper()
	count := func() int {
		n := 0
		for _, r := range fake.Requests() {
			if r.Method == method && r.Path == path {
				n++
			}
		}
		return n
	}
	deadline := time.Now().Add(5 * time.Second)
	for count() < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	return count()
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// An attempt slower than the client's timeout is one attempt failing. The
// next one answers, and the call succeeds.
func TestDo_TimedOutAttemptIsRetried(t *testing.T) {
	fake := flightdecktest.New(t)
	project := fake.AddProject("Slow", "SLOW")
	path := fmt.Sprintf("/projects/%d", project.ID)
	fake.DelayResponses(http.MethodGet, path, slowAnswer, 1)
	c, transport := timeoutClient(t, fake, clientTimeout, DefaultMaxRetries)

	got, err := c.GetProject(context.Background(), project.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.ID != project.ID {
		t.Errorf("got project %d, want %d", got.ID, project.ID)
	}
	if n := transport.attempts.Load(); n != 2 {
		t.Errorf("attempts = %d, want 2 (one timed out, one answered)", n)
	}
}

// The caller's own deadline is the caller giving up: nothing is retried,
// and the caller's error comes back.
func TestDo_CallersDeadlineIsNotRetried(t *testing.T) {
	fake := flightdecktest.New(t)
	project := fake.AddProject("Slow", "SLOW")
	path := fmt.Sprintf("/projects/%d", project.ID)
	fake.DelayResponses(http.MethodGet, path, slowAnswer, 10)
	c, transport := timeoutClient(t, fake, time.Minute, DefaultMaxRetries)

	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	defer cancel()
	_, err := c.GetProject(ctx, project.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's context.DeadlineExceeded", err)
	}
	if n := transport.attempts.Load(); n != 1 {
		t.Errorf("attempts = %d, want 1: the caller gave up", n)
	}
	if n := served(t, fake, http.MethodGet, "/api/v1"+path, 1); n != 1 {
		t.Errorf("the fake served %d GETs, want 1", n)
	}
}

// A keyed create whose answer is slower than the timeout did reach the
// server. The retry under the same key replays it, so there is one label,
// and the trace says an earlier send went unanswered.
func TestDo_TimedOutKeyedCreateReplaysRatherThanDuplicates(t *testing.T) {
	fake := flightdecktest.New(t)
	project := fake.AddProject("Labels", "LBL")
	path := fmt.Sprintf("/projects/%d/labels", project.ID)
	fake.DelayResponses(http.MethodPost, path, slowAnswer, 1)
	c, transport := timeoutClient(t, fake, clientTimeout, DefaultMaxRetries)

	var trace sendTrace
	var out struct {
		ID int64 `json:"id"`
	}
	err := c.Post(context.Background(), path, map[string]any{"label": map[string]any{"name": "Slow"}}, &out,
		WithIdempotencyKey("slow-label"), withSendTrace(&trace))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if !trace.earlierSendUnanswered {
		t.Error("the timed-out first send reached the server; earlierSendUnanswered should be true")
	}
	if n := transport.attempts.Load(); n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
	var slow []int64
	for _, l := range fake.LabelsOf(project.ID) {
		if l.Name == "Slow" {
			slow = append(slow, l.ID)
		}
	}
	if len(slow) != 1 || slow[0] != out.ID {
		t.Errorf("labels named Slow = %v, want just %d", slow, out.ID)
	}
}

// A secret-bearing create whose answer timed out is the lost-response case
// CreateSecretResource recovers from: nobody holds the first record's secret,
// so it is retired and the resource made again, leaving one live webhook
// whose secret the caller has.
func TestCreateWebhook_timedOutCreateIsRecovered(t *testing.T) {
	fake := flightdecktest.New(t)
	fake.DelayResponses(http.MethodPost, "/webhooks", slowAnswer, 1)
	c, _ := timeoutClient(t, fake, clientTimeout, DefaultMaxRetries)

	fields := Fields{"url": "https://ci.example.com/hooks/slow", "events": []string{"project.updated"}}
	hook, err := c.CreateWebhook(context.Background(), fields, PayloadKey("webhook", "", fields))
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if hook.Secret == "" {
		t.Error("the recovered webhook came back without its secret")
	}
	if live := liveWebhooks(t, fake); len(live) != 1 || live[0] != hook.ID {
		t.Errorf("live webhooks = %v, want just %d: the timed-out one should be deleted", live, hook.ID)
	}
	if n := served(t, fake, http.MethodPost, "/api/v1/webhooks", 3); n != 3 {
		t.Errorf("webhook POSTs = %d, want 3 (the slow one, the refused replay, the fresh create)", n)
	}
}

// A request that is not safe to send twice is not retried after a timeout:
// the first may have taken effect.
func TestDo_NonReplayableTimeoutIsNotRetried(t *testing.T) {
	fake := flightdecktest.New(t)
	project := fake.AddProject("Labels", "LBL")
	path := fmt.Sprintf("/projects/%d/labels", project.ID)
	fake.DelayResponses(http.MethodPost, path, slowAnswer, 10)
	c, transport := timeoutClient(t, fake, clientTimeout, DefaultMaxRetries)

	err := c.Post(context.Background(), path, map[string]any{"label": map[string]any{"name": "Once"}}, nil)
	if !isTimeout(err) {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if n := transport.attempts.Load(); n != 1 {
		t.Errorf("attempts = %d, want 1: an unkeyed POST is not replayable", n)
	}
	if n := served(t, fake, http.MethodPost, "/api/v1"+path, 1); n != 1 {
		t.Errorf("the fake served %d POSTs, want 1", n)
	}
}

// Timeouts on every attempt stop after maxRetries retries, with the timeout.
func TestDo_TimeoutsOnEveryAttemptStopAfterMaxRetries(t *testing.T) {
	fake := flightdecktest.New(t)
	project := fake.AddProject("Slow", "SLOW")
	path := fmt.Sprintf("/projects/%d", project.ID)
	const maxRetries = 2
	fake.DelayResponses(http.MethodGet, path, slowAnswer, 10)
	c, transport := timeoutClient(t, fake, clientTimeout, maxRetries)

	_, err := c.GetProject(context.Background(), project.ID)
	if !isTimeout(err) {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if n := transport.attempts.Load(); n != maxRetries+1 {
		t.Errorf("attempts = %d, want %d", n, maxRetries+1)
	}
	if n := served(t, fake, http.MethodGet, "/api/v1"+path, maxRetries+1); n != maxRetries+1 {
		t.Errorf("the fake served %d GETs, want %s", n, strconv.Itoa(maxRetries+1))
	}
}

// liveWebhooks lists the webhook ids the fake holds, through a plain client.
func liveWebhooks(t *testing.T, fake *flightdecktest.Server) []int64 {
	t.Helper()
	c, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	hooks, err := c.ListWebhooks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(hooks))
	for i, w := range hooks {
		ids[i] = w.ID
	}
	return ids
}

// otherClientsWebhook has another client make a webhook with the same body,
// so a create sent under the same stable key names it.
func otherClientsWebhook(t *testing.T, fake *flightdecktest.Server, fields Fields) int64 {
	t.Helper()
	other, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	hook, err := other.CreateWebhook(context.Background(), fields, PayloadKey("webhook", "", fields))
	if err != nil {
		t.Fatal(err)
	}
	return hook.ID
}

// slowFirstDial is a standard transport whose first dial does not finish
// before the client's timeout, so that attempt times out with nothing sent.
func slowFirstDial() *http.Transport {
	var once sync.Once
	dialer := &net.Dialer{}
	return &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		slow := false
		once.Do(func() { slow = true })
		if slow {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return dialer.DialContext(ctx, network, addr)
	}}
}

// silentFirstAttempt is a RoundTripper that reports nothing to the request's
// trace, and whose first attempt never sends anything: it waits out the
// client's timeout.
type silentFirstAttempt struct{ calls atomic.Int32 }

func (s *silentFirstAttempt) RoundTrip(r *http.Request) (*http.Response, error) {
	if s.calls.Add(1) == 1 {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	return http.DefaultTransport.RoundTrip(r)
}

// A timeout before anything was sent is not a lost answer. If another client
// already made a webhook under this create's key, the create is refused and
// that webhook is left alone, both when the trace says nothing was written
// and when the transport reports nothing to the trace at all.
func TestCreateWebhook_timeoutBeforeSendingRetiresNothing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		transport http.RoundTripper
	}{
		{"traced dial timeout", slowFirstDial()},
		{"untraced transport", &silentFirstAttempt{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := flightdecktest.New(t)
			fields := Fields{"url": "https://ci.example.com/hooks/other", "events": []string{"project.updated"}}
			theirs := otherClientsWebhook(t, fake, fields)

			c, err := New(fake.URL, fake.Token(), WithHTTPClient(&http.Client{Timeout: clientTimeout, Transport: tc.transport}))
			if err != nil {
				t.Fatal(err)
			}
			c.sleep = func(context.Context, time.Duration) error { return nil }
			_, err = c.CreateWebhook(context.Background(), fields, PayloadKey("webhook", "", fields))
			apiErr, ok := AsError(err)
			if !ok || apiErr.Code != CodeIdempotencyReplayWithheld || apiErr.ID != theirs {
				t.Fatalf("err = %v, want the replay of webhook %d refused", err, theirs)
			}
			if apiErr.EarlierSendUnanswered {
				t.Error("nothing was sent before the timeout; EarlierSendUnanswered should be false")
			}
			if live := liveWebhooks(t, fake); len(live) != 1 || live[0] != theirs {
				t.Errorf("live webhooks = %v, want the other client's %d left alone", live, theirs)
			}
		})
	}
}

// An update whose answer timed out after it was applied is sent again, and
// the retry's If-Match no longer matches. The refusal says an earlier send
// went unanswered, so the provider can say the write itself may have moved
// the version, not someone outside Terraform.
func TestPatch_timedOutAfterItWasAppliedIsStaleWithAnUnansweredSend(t *testing.T) {
	fake := flightdecktest.New(t)
	project := fake.AddProject("Labels", "LBL")
	plain, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	label, err := plain.CreateLabel(context.Background(), project.ID, Fields{"name": "Before"}, "label-before")
	if err != nil {
		t.Fatal(err)
	}
	fake.DelayResponses(http.MethodPatch, fmt.Sprintf("/labels/%d", label.ID), slowAnswer, 1)
	c, _ := timeoutClient(t, fake, clientTimeout, DefaultMaxRetries)

	_, err = c.UpdateLabel(context.Background(), label.ID, Fields{"name": "After"}, label.LockVersion)
	apiErr, ok := AsError(err)
	if !ok || !IsStale(err) || !apiErr.EarlierSendUnanswered {
		t.Fatalf("err = %v, want a stale refusal after an unanswered send", err)
	}
	got, err := plain.GetLabel(context.Background(), label.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "After" {
		t.Errorf("label name = %q; the first, timed-out PATCH should have been applied", got.Name)
	}
}
