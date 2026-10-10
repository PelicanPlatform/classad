package dbrpc

import (
	"context"
	"strings"
	"testing"
)

// TestNewClassAdThenSetAttributeKeepsTheAd: a client that creates an ad and sets one of its
// attributes in the same transaction must find the whole ad committed. The server creates ads
// through db.Txn.NewClassAdOld; the commit used to keep only the amended attribute.
func TestNewClassAdThenSetAttributeKeepsTheAd(t *testing.T) {
	for _, pair := range []struct {
		name string
		open func(t *testing.T) (*Client, func())
	}{
		{"memory", testPair},
		{"persistent", func(t *testing.T) (*Client, func()) { return testPairPersistent(t, false) }},
	} {
		t.Run(pair.name, func(t *testing.T) {
			c, cleanup := pair.open(t)
			defer cleanup()
			ctx := context.Background()
			tx, err := c.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = tx.NewClassAd(ctx, "1.0", "Owner = \"alice\"\nJobStatus = 1\nClusterId = 1"); err != nil {
				t.Fatal(err)
			}
			if err = tx.SetAttribute(ctx, "1.0", "ExitCode", "0"); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			rows, err := c.Query(ctx, "true")
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("got %d ads, want 1: %v", len(rows), rows)
			}
			for _, attr := range []string{"Owner", "JobStatus", "ClusterId", "ExitCode"} {
				if !strings.Contains(rows[0], attr) {
					t.Errorf("committed ad lacks %s: %s", attr, rows[0])
				}
			}
		})
	}
}
