package collections

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureLog redirects the standard logger for the duration of the test and returns what
// was written to it. These tests are deliberately NOT parallel: the logger is process-wide.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return &buf
}

// TestDictFaultLogsByDefault is why the default is a log line and not silence. Every path
// that reports a dictionary fault exists because the failure used to produce no sign at
// all until the store was restarted -- and a callback nobody wires up is the same silence
// wearing a different hat.
func TestDictFaultLogsByDefault(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14}) // no OnDictFault
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	path := filepath.Join(dir, "dicts", "1.zst")
	if rmErr := os.Remove(path); rmErr != nil {
		t.Fatal(rmErr)
	}

	logged := captureLog(t)
	faults := c.VerifyDictFiles()
	if len(faults) != 1 {
		t.Fatalf("want one fault, got %d", len(faults))
	}
	out := logged.String()
	if out == "" {
		t.Fatal("a store with no callback reported the fault to nobody")
	}
	for _, want := range []string{path, "rewritten", "deleting dictionaries"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log line does not mention %q: %s", want, out)
		}
	}
}

// TestOnDictFaultReplacesTheLog: a daemon that wires the callback gets the fault once, in
// its own hands, and not also on the standard logger. Reporting twice would push a store
// with its own logging into duplicating every line.
func TestOnDictFaultReplacesTheLog(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	var got []error
	c, err := Open(Options{
		Dir: dir, Shards: 1, SegmentSize: 1 << 14,
		OnDictFault: func(e error) { got = append(got, e) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if rmErr := os.Remove(filepath.Join(dir, "dicts", "1.zst")); rmErr != nil {
		t.Fatal(rmErr)
	}

	logged := captureLog(t)
	c.VerifyDictFiles()
	if len(got) != 1 {
		t.Fatalf("the callback fired %d times; want 1", len(got))
	}
	if out := logged.String(); out != "" {
		t.Errorf("the fault was logged as well as handed to the callback: %s", out)
	}
}

// TestDictFaultHookCanBeSilenced: nil is how a caller that genuinely wants nothing says
// so, and it must not panic on the way.
func TestDictFaultHookCanBeSilenced(t *testing.T) {
	prev := DictFaultHook
	DictFaultHook = nil
	t.Cleanup(func() { DictFaultHook = prev })

	dir, _ := buildTwoDictStore(t)
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if rmErr := os.Remove(filepath.Join(dir, "dicts", "1.zst")); rmErr != nil {
		t.Fatal(rmErr)
	}

	logged := captureLog(t)
	if faults := c.VerifyDictFiles(); len(faults) != 1 {
		t.Fatalf("want one fault, got %d", len(faults))
	}
	if out := logged.String(); out != "" {
		t.Errorf("a nil hook still logged: %s", out)
	}
}

// TestOpenReportsOneLinePerDictionaryNotPerSegment is the difference between a report and
// a flood. A dictionary that is not the one its records were written with disagrees with
// EVERY segment naming it, so a line apiece would bury the one fact an operator needs
// under hundreds of repetitions of it -- and the count is the useful part, because it says
// how much of the store is affected.
func TestOpenReportsOneLinePerDictionaryNotPerSegment(t *testing.T) {
	dir, _ := fingerprintedStore(t)
	if werr := os.WriteFile(filepath.Join(dir, "dicts", "1.zst"), imposterDictionary(t, 1), 0o644); werr != nil {
		t.Fatal(werr)
	}

	var got []error
	c, err := Open(Options{
		Dir: dir, Shards: 1, SegmentSize: 1 << 20,
		OnDictFault: func(e error) { got = append(got, e) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if len(got) != 1 {
		t.Fatalf("reported %d faults for one wrong dictionary; want exactly 1:\n%v", len(got), got)
	}
	var df *DictFault
	if !errors.As(got[0], &df) {
		t.Fatalf("not a *DictFault: %v", got[0])
	}
	if df.Segments < 2 {
		t.Errorf("Segments = %d; the fixture has several segments under dictionary 1", df.Segments)
	}
	msg := got[0].Error()
	if !strings.Contains(msg, "segment(s) were written with") || !strings.Contains(msg, "e.g.") {
		t.Errorf("the line does not say how many or give an example: %s", msg)
	}
}
