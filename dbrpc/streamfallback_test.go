package dbrpc

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A server REJECTS AN OPCODE IT DOES NOT KNOW with stBadReq, and that rejection is the
// fallback signal for every op introduced after a peer was built. The streaming readers have
// to surface it.
//
// Two of the three did not. streamEachCursor handled `stErr, stBadReq`; streamCtx and
// streamEachStats handled only stErr and dropped stBadReq on the floor -- so the reader went
// on waiting for a stream terminator the server was never going to send. An older server did
// not make the client fall back; it made the client HANG, which is worse than the missing
// feature and much harder to attribute.
//
// QueryRawProjectRefs is the case that mattered: it documents ErrProjectRefsUnsupported as its
// older-server signal, it runs on both of the broken readers, and it is on the hot read path
// of anything serving projected rows.
func TestStreamingOpsFallBackRatherThanHangOnAnUnknownOpcode(t *testing.T) {
	// Everything EXCEPT the refs projection, which is then the op this server does not know.
	knows := func(o op) bool { return o != opQueryRawProjRefs }

	cases := []struct {
		name string
		call func(context.Context, *Client) error
	}{
		{"collecting", func(ctx context.Context, c *Client) error {
			_, err := c.QueryRawProjectRefs(ctx, "", "true", []string{"Owner"}, 0)
			return err
		}},
		{"streaming", func(ctx context.Context, c *Client) error {
			return c.QueryRawProjectRefsStream(ctx, "", "true", []string{"Owner"}, 0,
				func(string) bool { return true })
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cleanup := testPairOps(t, knows)
			defer cleanup()

			// A deadline rather than an open-ended wait: the failure being tested is a hang,
			// and a test that hangs to prove it reports as a timeout on the whole package
			// instead of as this case failing.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- tc.call(ctx, c) }()
			select {
			case err := <-done:
				if !errors.Is(err, ErrProjectRefsUnsupported) {
					t.Errorf("error = %v, want ErrProjectRefsUnsupported", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("the call did not return: an unknown opcode must be reported, not " +
					"waited on -- the reader is dropping stBadReq and waiting for a stream " +
					"terminator that will never arrive")
			}
		})
	}
}
