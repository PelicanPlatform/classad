package dbrpc

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// A JOIN'S EXECUTION STATISTICS OVER A REAL CONNECTION.
//
// What these pin is not the planning -- db tests that -- but that the record of it reaches
// the caller, and that the ways it can fail to reach the caller are all distinguishable from
// the join having scanned. A report that cannot say "nobody told me" is the bug these close.

// statsFixture is a big table to hash against a small one to stream, which is the shape an
// index probe is for. index decides whether the big table has the index the probe needs, so
// one fixture produces both halves of the comparison that matters: a join that narrowed and a
// join that could not.
type statsFixture struct {
	srv *Server
	cat *db.Catalog
}

// statsProbeRows is above db's minProbeRows, below which narrowing is not considered at all.
// A fixture under it would decline for a reason that has nothing to do with the index, and
// the indexed arm would then prove nothing.
const statsProbeRows = 2048

func newStatsFixture(t *testing.T, index bool) *statsFixture {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A small segment size on purpose: postings are built when a segment seals, so a
	// fixture whose rows all fit one open segment has the index configured and none built.
	cfg := db.ArchiveConfig{SegmentSize: 4096}
	if index {
		cfg.ValueAttrs = []string{"ClusterId"}
	}
	hist, err := cat.CreateArchiveTable("hist", cfg)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.CreateArchiveTable("jobs", db.ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= statsProbeRows; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttr("ExitCode", int64(i%7))
		ad.InsertAttrString("Pad", strings.Repeat("x", 64))
		if aerr := hist.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	// The streamed side names every 128th cluster, so the probe has few keys against many
	// rows -- the regime where narrowing is supposed to win.
	for i := 128; i <= statsProbeRows; i += 128 {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttrString("Owner", "alice")
		if aerr := jobs.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	f := &statsFixture{srv: NewServerCatalog(cat), cat: cat}
	t.Cleanup(func() { f.srv.Close(); cat.Close() })
	return f
}

// client dials the fixture's server with a given handshake. attrs is what the application
// captured from the peer, so doctoring it is how a test reaches a server of another vintage.
func (f *statsFixture) client(t *testing.T, attrs map[string]string) *Client {
	t.Helper()
	cconn, sconn := netPipe()
	go func() { _ = f.srv.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	c := NewClient(cconn, WithPeerSessionAttrs(attrs))
	t.Cleanup(func() { c.Close() })
	return c
}

func statsProbeRequest() JoinRequest {
	return JoinRequest{Spec: db.JoinSpec{
		Kind:  db.JoinInner,
		Left:  db.JoinSide{Table: "jobs", Alias: "j"},
		Right: db.JoinSide{Table: "hist", Alias: "h"},
		Cond:  []string{"j.ClusterId == h.ClusterId"},
		Equi:  []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}}
}

// withoutFeature is the handshake of a peer built before one feature existed: the same
// advertisement with that name struck out. It is how a test reaches an older server without
// keeping an older build around.
func withoutFeature(attrs map[string]string, f Feature) map[string]string {
	out := make(map[string]string, len(attrs))
	for k, v := range attrs {
		out[k] = v
	}
	kept := make([]string, 0, 32)
	for _, name := range strings.Split(attrs[SessionAttrFeatures], ",") {
		if name != string(f) {
			kept = append(kept, name)
		}
	}
	out[SessionAttrFeatures] = strings.Join(kept, ",")
	return out
}

func drainJoinStats(t *testing.T, c *Client, jr JoinRequest) (int, JoinStatsReport, error) {
	t.Helper()
	var rep JoinStatsReport
	rows := 0
	err := c.JoinStats(context.Background(), jr, func(string) bool { rows++; return true }, &rep)
	return rows, rep, err
}

// THE TEST THE WHOLE CHANGE IS FOR: a join that narrowed through an index and one that did
// not report differently, and the one that did not says why.
//
// Without the reason the two replies are the same reply: ProbeKeys is 0 in both, and a reader
// cannot tell a store that declined to probe from a store with no index to probe.
func TestJoinStatsReportTheAccessPathTheJoinTook(t *testing.T) {
	for _, tc := range []struct {
		name  string
		index bool
	}{{"indexed", true}, {"unindexed", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStatsFixture(t, tc.index)
			c := f.client(t, f.srv.SessionAttrs())
			if !c.SupportsNewer(FeatJoinStats) {
				t.Fatal("the client did not see FeatJoinStats; the test proves nothing")
			}
			rows, rep, err := drainJoinStats(t, c, statsProbeRequest())
			if err != nil {
				t.Fatal(err)
			}
			if rows != statsProbeRows/128 {
				t.Fatalf("joined %d rows, want %d", rows, statsProbeRows/128)
			}
			if !rep.Reported {
				t.Fatal("the server sent no statistics for a join it ran")
			}
			if !rep.Complete {
				t.Error("a join that ran to its end reported incomplete statistics")
			}
			if tc.index {
				if rep.Stats.ProbeKeys != statsProbeRows/128 {
					t.Errorf("ProbeKeys = %d (%q), want %d",
						rep.Stats.ProbeKeys, rep.Stats.ProbeWhy, statsProbeRows/128)
				}
				if rep.Stats.ProbeAttr != "ClusterId" {
					t.Errorf("ProbeAttr = %q, want ClusterId", rep.Stats.ProbeAttr)
				}
				if rep.Stats.ProbeWhy != "" {
					t.Errorf("a join that narrowed gave a reason for not narrowing: %q",
						rep.Stats.ProbeWhy)
				}
				return
			}
			if rep.Stats.ProbeKeys != 0 {
				t.Errorf("ProbeKeys = %d against a table with no index", rep.Stats.ProbeKeys)
			}
			if rep.Stats.ProbeWhy == "" {
				t.Fatal("the join did not narrow and gave no reason, which is the state " +
					"this reports precisely to avoid")
			}
		})
	}
}

// The report must match what the store recorded. A report that changed shape on the way
// across is a different failure from one that did not arrive, and it reads as a real answer.
func TestJoinStatsMatchTheStoresOwnRecord(t *testing.T) {
	f := newStatsFixture(t, true)
	c := f.client(t, f.srv.SessionAttrs())
	_, rep, err := drainJoinStats(t, c, statsProbeRequest())
	if err != nil {
		t.Fatal(err)
	}
	var want db.JoinStats
	seq, err := f.cat.Join(statsProbeRequest().Spec, db.JoinLimits{}, &want)
	if err != nil {
		t.Fatal(err)
	}
	for range seq { //nolint:revive // the sequence must be drained for the stats to be final
	}
	if want.Err != nil {
		t.Fatal(want.Err)
	}
	// Err is deliberately not carried as an error, so compare the rest.
	if got, wanted := joinStatsKey(rep.Stats), joinStatsKey(want); got != wanted {
		t.Errorf("the wire report differs from the local one:\n got  %s\n want %s", got, wanted)
	}
}

// joinStatsKey renders the statistics for comparison. A rendering rather than
// reflect.DeepEqual, which must not be used on a struct holding an error -- and the error is
// the one field that deliberately does not cross as itself, so it is left out here and
// compared by its text where that matters.
func joinStatsKey(s db.JoinStats) string {
	s.Err = nil
	return fmt.Sprintf("%+v", s)
}

// ABSENCE IS NOT "NO INDEX". A client reaching a server that does not advertise the feature
// must be able to say so, because the alternative reading -- a join that scanned -- is a
// statement about the store that nobody made.
func TestJoinStatsAbsenceIsDistinguishableFromNoIndex(t *testing.T) {
	f := newStatsFixture(t, false) // no index: the reporting client will get a reason
	told := f.client(t, f.srv.SessionAttrs())
	silent := f.client(t, withoutFeature(f.srv.SessionAttrs(), FeatJoinStats))
	if !told.SupportsNewer(FeatJoinStats) {
		t.Fatal("the advertising client did not see FeatJoinStats; the test proves nothing")
	}
	if silent.SupportsNewer(FeatJoinStats) {
		t.Fatal("the silent client claims FeatJoinStats; the test proves nothing")
	}
	// The older server must still answer the join itself: the statistics are an addition,
	// not a precondition.
	oldRows, old, err := drainJoinStats(t, silent, statsProbeRequest())
	if err != nil {
		t.Fatal(err)
	}
	newRows, fresh, err := drainJoinStats(t, told, statsProbeRequest())
	if err != nil {
		t.Fatal(err)
	}
	if oldRows != newRows || oldRows == 0 {
		t.Fatalf("the two clients joined differently: %d and %d rows", oldRows, newRows)
	}
	if old.Reported {
		t.Fatal("a server that does not advertise the feature reported statistics")
	}
	if !old.Unsupported {
		t.Error("the report does not say the peer lacks the feature, which is the one " +
			"cause of absence a diagnostic can name")
	}
	if old.Complete {
		t.Error("a report that carries nothing says it is complete")
	}
	// The distinction itself: the same join, the same absent index, two different answers.
	if !fresh.Reported || fresh.Stats.ProbeWhy == "" {
		t.Fatalf("the advertising client got no reason to compare against: %+v", fresh)
	}
	if old.Stats.ProbeWhy != "" {
		t.Errorf("an unreported join carries a reason it was never given: %q",
			old.Stats.ProbeWhy)
	}
}

// Nothing is asked of a peer that cannot answer, so its frame is byte-for-byte the frame
// every client has always sent. A server of any vintage must read it unchanged.
func TestJoinStatsRequestByteIsOptional(t *testing.T) {
	jr := statsProbeRequest()
	plain := joinFrameOp(jr, opJoin, false, false, false)(1)
	asking := joinFrameOp(jr, opJoin, false, false, true)(1)
	if len(asking) != len(plain)+1 {
		t.Fatalf("the stats byte changed the frame by %d bytes", len(asking)-len(plain))
	}
	if !reflect.DeepEqual(asking[:len(plain)], plain) {
		t.Error("asking for statistics changed the bytes of the plan")
	}
	for _, tc := range []struct {
		name  string
		frame []byte
		want  bool
	}{{"plain", plain, false}, {"asking", asking, true}} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, body, ok := reqHeader(tc.frame)
			if !ok {
				t.Fatal("the request frame has no readable header")
			}
			if _, ok := readJoinRequest(body, false); !ok {
				t.Fatal("the frame did not decode")
			}
			if got := readJoinStatsByte(body); got != tc.want {
				t.Errorf("readJoinStatsByte = %v, want %v", got, tc.want)
			}
		})
	}
}

// A CHAIN REPORTS EVERY STEP, IN ORDER. One step's decision says nothing about another's: a
// chain can narrow its second table through an index and read its third whole, and a report
// that carried only the first would describe a plan the store did not run.
func TestJoinStatsCarryEveryChainStep(t *testing.T) {
	c, cat := chainRPCFixture(t)
	jr := chainRPCRequest(db.JoinInner, db.JoinInner)

	var rep JoinStatsReport
	if err := c.JoinStats(context.Background(), jr, func(string) bool { return true }, &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Reported || !rep.Complete {
		t.Fatalf("a chain that ran to its end reported %+v", rep)
	}
	if len(rep.Stats.ProbeSteps) != len(jr.Spec.Steps) {
		t.Fatalf("the report carries %d steps for a chain of %d",
			len(rep.Stats.ProbeSteps), len(jr.Spec.Steps))
	}
	seen := map[string]bool{}
	for i, st := range rep.Stats.ProbeSteps {
		if st.Alias == "" {
			t.Errorf("step %d came back with no alias", i)
		}
		if seen[st.Alias] {
			t.Errorf("step %d repeats alias %q, so the steps are not in order", i, st.Alias)
		}
		seen[st.Alias] = true
		// The point of the per-step record: each step says what it did or why it did not.
		if st.Keys == 0 && st.Why == "" {
			t.Errorf("step %d (%s) narrowed on nothing and gave no reason", i, st.Alias)
		}
	}
	// Order is pinned against the store's own record rather than against a hardcoded
	// sequence, because which table a chain reads FROM is the planner's choice.
	var want db.JoinStats
	seq, err := cat.Join(jr.Spec, db.JoinLimits{}, &want)
	if err != nil {
		t.Fatal(err)
	}
	for range seq { //nolint:revive // drained so the stats are final
	}
	if !reflect.DeepEqual(rep.Stats.ProbeSteps, want.ProbeSteps) {
		t.Errorf("the steps crossed in a different order or shape:\n got  %+v\n want %+v",
			rep.Stats.ProbeSteps, want.ProbeSteps)
	}
	if rep.Stats.ChainOrderWhy != want.ChainOrderWhy || rep.Stats.ChainFromAlias != want.ChainFromAlias {
		t.Errorf("the chain order decision did not cross: %q/%q, want %q/%q",
			rep.Stats.ChainFromAlias, rep.Stats.ChainOrderWhy,
			want.ChainFromAlias, want.ChainOrderWhy)
	}
}

// A JOIN THAT STOPPED AT A LIMIT STILL REPORTS, AND SAYS IT IS NOT COMPLETE. Its access path
// is exactly what a reader wants to see; its counts stopped where the limit did, and a report
// that presented them as a finished join's would overstate what was read.
func TestJoinStatsFromATruncatedJoinAreNotComplete(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()

	jr := JoinRequest{
		Spec:   db.JoinSpec{Kind: db.JoinCross, Left: db.JoinSide{Table: "jobs", Alias: "j"}, Right: db.JoinSide{Table: "hist", Alias: "h"}},
		Limits: db.JoinLimits{MaxPairs: 5}, // 12 pairs are available
	}
	_, rep, err := drainJoinStats(t, f.c, jr)
	if err == nil {
		t.Fatal("a 5-pair budget over 12 pairs returned no error")
	}
	if !rep.Reported {
		t.Fatal("a join that stopped at a limit reported nothing; its access path is " +
			"exactly what a reader wants in that case")
	}
	if rep.Complete {
		t.Error("partial statistics present themselves as a complete join")
	}
	if !strings.Contains(rep.Err, "candidate pairs") {
		t.Errorf("the report does not say what stopped the join: %q", rep.Err)
	}
}

// A consumer that stops reading abandons the rest of the reply, trailer included. The report
// must then say it has nothing rather than report the zero value as a measurement.
func TestJoinStatsFromAnAbandonedStreamAreNotReported(t *testing.T) {
	f := newStatsFixture(t, true)
	c := f.client(t, f.srv.SessionAttrs())

	var rep JoinStatsReport
	err := c.JoinStats(context.Background(), statsProbeRequest(),
		func(string) bool { return false }, &rep)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reported || rep.Complete {
		t.Errorf("a stream abandoned after one row reported %+v", rep)
	}
	if rep.Unsupported {
		t.Error("the peer HAS the feature; absence here is this call's doing, not the peer's")
	}
}

// A transport that dies mid-stream is the other way the trailer never arrives, and it must
// not read as a complete join either.
func TestJoinStatsFromABrokenStreamAreNotComplete(t *testing.T) {
	f := newStatsFixture(t, true)
	c := f.client(t, f.srv.SessionAttrs())

	var rep JoinStatsReport
	rows := 0
	err := c.JoinStats(context.Background(), statsProbeRequest(), func(string) bool {
		rows++
		if rows == 1 {
			c.Close() // the connection goes away under the stream
		}
		return true
	}, &rep)
	if err == nil {
		t.Fatal("closing the connection mid-stream was reported as a clean join; the " +
			"test is not exercising a broken stream")
	}
	if rep.Complete {
		t.Errorf("a failed stream reported complete statistics: %+v", rep)
	}
}

// The wire-form join reports the same way the text one does. It is a separate reply path with
// its own frame loop, so "the trailer is handled" has to be shown on both.
func TestJoinWireStatsReport(t *testing.T) {
	f := newStatsFixture(t, true)
	c := f.client(t, f.srv.SessionAttrs())

	jr := statsProbeRequest()
	jr.Spec.Left.Project = []string{"ClusterId"}
	jr.Spec.Right.Project = []string{"ExitCode"}

	var rep JoinStatsReport
	rows := 0
	err := c.JoinWireStats(context.Background(), jr, false, func([]byte) bool { rows++; return true }, &rep)
	if err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Fatal("the wire join produced no rows")
	}
	if !rep.Reported || !rep.Complete {
		t.Fatalf("the wire join reported %+v", rep)
	}
	if rep.Stats.ProbeAttr != "ClusterId" || rep.Stats.ProbeKeys == 0 {
		t.Errorf("the wire join's probe did not cross: %+v", rep.Stats)
	}
}

// The reduced join reports too. It is the one shape whose reply does not scale with the join's
// rows, which is exactly why nobody would otherwise notice it had stopped using an index.
func TestJoinAggregateStatsReport(t *testing.T) {
	f := newStatsFixture(t, true)
	c := f.client(t, f.srv.SessionAttrs())

	jr := JoinAggRequest{Spec: db.JoinAggSpec{
		Join:      statsProbeRequest().Spec,
		GroupCols: []db.GroupCol{{Attr: "h.ExitCode"}},
		Aggs:      []db.AggSpec{{Func: db.AggCount, Arg: "*"}},
	}}
	var rep JoinStatsReport
	rows, err := c.JoinAggregateStats(context.Background(), jr, &rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("the reduction produced no groups")
	}
	if !rep.Reported || !rep.Complete {
		t.Fatalf("the reduced join reported %+v", rep)
	}
	if rep.Stats.ProbeAttr != "ClusterId" || rep.Stats.ProbeKeys == 0 {
		t.Errorf("the reduced join's probe did not cross: %+v", rep.Stats)
	}
	if rep.Stats.Groups != len(rows) {
		t.Errorf("the report says %d groups, the reply carries %d", rep.Stats.Groups, len(rows))
	}
}

// EVERY FIELD CROSSES. The trailer marshals db.JoinStats whole rather than copying it field
// by field, so a field added upstream rides without this package being touched. This is what
// says so, and it is what fails if that ever stops being true: a statistic that quietly
// stopped crossing is the failure the trailer exists to prevent.
func TestJoinStatsTrailerCarriesEveryField(t *testing.T) {
	want := db.JoinStats{}
	fillDistinct(t, reflect.ValueOf(&want).Elem(), 1)
	if want.Err == nil || len(want.ProbeSteps) == 0 {
		t.Fatalf("the filler did not populate the stats: %+v", want)
	}

	frame := joinStatsTrailer(42, want)
	if frame == nil {
		t.Fatal("the statistics did not encode")
	}
	_, status, body, ok := respHeader(frame)
	if !ok {
		t.Fatal("the trailer frame has no readable header")
	}
	if status != stStreamJoinStats {
		t.Fatalf("the trailer went out as status %d", status)
	}
	got, errText, ok := readJoinStats(body)
	if !ok {
		t.Fatal("the trailer did not decode")
	}
	if errText != want.Err.Error() {
		t.Errorf("the error text is %q, want %q", errText, want.Err.Error())
	}
	// Err crosses as text, by design: the typed error is what the call returns.
	if gotKey, wantKey := joinStatsKey(got), joinStatsKey(want); gotKey != wantKey {
		t.Errorf("a field did not cross:\n got  %s\n want %s", gotKey, wantKey)
	}
}

// fillDistinct writes a different non-zero value into every field of a struct, recursively, so
// a field that is dropped in transit comes back as a zero that no filled field holds.
func fillDistinct(t *testing.T, v reflect.Value, seed int) int {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		switch f.Kind() {
		case reflect.Int:
			seed++
			f.SetInt(int64(seed))
		case reflect.String:
			seed++
			f.SetString(fmt.Sprintf("value-%d", seed))
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Interface:
			// The only interface here is the error.
			seed++
			f.Set(reflect.ValueOf(fmt.Errorf("failure-%d", seed)))
		case reflect.Slice:
			elem := reflect.New(f.Type().Elem()).Elem()
			seed = fillDistinct(t, elem, seed)
			f.Set(reflect.Append(reflect.MakeSlice(f.Type(), 0, 1), elem))
		default:
			t.Fatalf("field %q has kind %v, which this filler does not cover -- "+
				"extend it rather than letting the field go untested",
				v.Type().Field(i).Name, f.Kind())
		}
	}
	return seed
}

// A server with no catalog cannot join, so it has no join to report on and must not name the
// feature. A definite yes that turns out wrong is what advertisement exists to prevent, and it
// would be worse here than elsewhere: a client told that statistics are available and given
// none reads the silence as a join that used no index.
func TestJoinStatsAreNotAdvertisedWithoutACatalog(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := NewServer(d)
	defer s.Close()

	if got := s.SessionAttrs()[SessionAttrFeatures]; strings.Contains(got, string(FeatJoinStats)) {
		t.Errorf("a single-database server advertised %q in %q; it has no join to report on",
			FeatJoinStats, got)
	}
	if !strings.Contains(ServerSessionAttrs()[SessionAttrFeatures], string(FeatJoinStats)) {
		t.Errorf("the BUILD should still advertise %q; only this server cannot serve it",
			FeatJoinStats)
	}
}
