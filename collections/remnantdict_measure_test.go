package collections

import (
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// historyLikeAd returns the text of a synthetic job-history ad shaped like a real one: ~150
// attributes, mostly literals, with the expression mix condor_submit and common submit files leave
// behind -- MemoryUsage on nearly every job, the default RequestMemory/RequestDisk expressions on the
// jobs that did not set them, a per-cluster Requirements, periodic policy expressions on some
// clusters, a DAG node's OnExitRemove, and nested transfer-statistics records whose values vary.
//
// cluster and proc place the job; cl carries the per-cluster choices so every proc of a cluster
// agrees, as it does in a real queue.
type historyCluster struct {
	id                      int
	owner                   int
	reqTmpl                 int
	defMem, defDisk         bool
	periodic, release, hold bool
	dag, leave              bool
	gpus                    int
	memMB                   int
}

func newHistoryCluster(rng *rand.Rand, id int) *historyCluster {
	return &historyCluster{
		id:       id,
		owner:    rng.Intn(200),
		reqTmpl:  rng.Intn(6),
		defMem:   rng.Intn(10) < 3,
		defDisk:  rng.Intn(10) < 4,
		periodic: rng.Intn(10) < 3,
		release:  rng.Intn(10) < 2,
		hold:     rng.Intn(10) < 2,
		dag:      rng.Intn(10) < 2,
		leave:    rng.Intn(10) < 1,
		gpus:     map[bool]int{true: 1, false: 0}[rng.Intn(20) == 0],
		memMB:    []int{1024, 2048, 4096, 8192, 16384}[rng.Intn(5)],
	}
}

var historyReqTemplates = []string{
	`(TARGET.Arch == "X86_64") && (TARGET.OpSys == "LINUX") && (TARGET.Disk >= RequestDisk) && (TARGET.Memory >= RequestMemory) && (TARGET.Cpus >= RequestCpus) && (TARGET.HasFileTransfer)`,
	`(TARGET.HasSingularity == true) && (TARGET.OSG_HOST_KERNEL_VERSION >= 31000) && (TARGET.Arch == "X86_64") && (TARGET.OpSys == "LINUX") && (TARGET.Disk >= RequestDisk) && (TARGET.Memory >= RequestMemory) && (TARGET.Cpus >= RequestCpus) && (TARGET.HasFileTransfer)`,
	`((OSGVO_OS_STRING == "RHEL 8") || (OSGVO_OS_STRING == "RHEL 9")) && (TARGET.Arch == "X86_64") && (TARGET.OpSys == "LINUX") && (TARGET.Disk >= RequestDisk) && (TARGET.Memory >= RequestMemory) && (TARGET.Cpus >= RequestCpus) && ((TARGET.FileSystemDomain == MY.FileSystemDomain) || (TARGET.HasFileTransfer))`,
	`(TARGET.CUDACapability >= 7.0) && (TARGET.GPUs >= RequestGPUs) && (TARGET.Arch == "X86_64") && (TARGET.OpSys == "LINUX") && (TARGET.Disk >= RequestDisk) && (TARGET.Memory >= RequestMemory) && (TARGET.Cpus >= RequestCpus) && (TARGET.HasFileTransfer)`,
	`(TARGET.GLIDEIN_Site =!= "Bad") && stringListMember(TARGET.GLIDEIN_ResourceName, "A,B,C") == false && (TARGET.Arch == "X86_64") && (TARGET.OpSys == "LINUX") && (TARGET.Disk >= RequestDisk) && (TARGET.Memory >= RequestMemory) && (TARGET.Cpus >= RequestCpus) && (TARGET.HasFileTransfer)`,
	`(TARGET.HasCHTCStaging == true) && (TARGET.Arch == "X86_64") && (TARGET.OpSys == "LINUX") && (TARGET.Disk >= RequestDisk) && (TARGET.Memory >= RequestMemory) && (TARGET.Cpus >= RequestCpus) && ((TARGET.FileSystemDomain == MY.FileSystemDomain) || (TARGET.HasFileTransfer))`,
}

func historyLikeAd(rng *rand.Rand, cl *historyCluster, proc, seq int) string {
	var b strings.Builder
	t0 := 1780000000 + seq*7
	run := 60 + rng.Intn(20000)
	rss := 1000 + rng.Intn(4000000)
	fmt.Fprintf(&b, `[ ClusterId=%d; ProcId=%d; Owner="user%d"; User="user%d@ap.example.edu"; JobStatus=4; LastJobStatus=2; `,
		cl.id, proc, cl.owner, cl.owner)
	fmt.Fprintf(&b, `GlobalJobId="ap.example.edu#%d.%d#%d"; QDate=%d; CompletionDate=%d; JobStartDate=%d; EnteredCurrentStatus=%d; `,
		cl.id, proc, t0-run-600, t0-run-600, t0, t0-run, t0)
	fmt.Fprintf(&b, `RemoteWallClockTime=%d.0; CumulativeSlotTime=%d.0; CommittedTime=%d; CommittedSlotTime=%d.0; RemoteUserCpu=%d.0; RemoteSysCpu=%d.0; `,
		run, run, run, run, run*9/10, run/50)
	fmt.Fprintf(&b, `ResidentSetSize=%d; ResidentSetSize_RAW=%d; ImageSize=%d; ImageSize_RAW=%d; DiskUsage=%d; DiskUsage_RAW=%d; `,
		rss, rss-rng.Intn(500), rss+1000, rss+900, 5000+rng.Intn(900000), 5000+rng.Intn(900000))
	fmt.Fprintf(&b, `ExitCode=%d; ExitBySignal=false; ExitStatus=0; NumJobStarts=%d; NumShadowStarts=%d; NumJobMatches=%d; JobRunCount=%d; `,
		rng.Intn(3)/2, 1+rng.Intn(2)/1, 1+rng.Intn(2), 1+rng.Intn(3), 1+rng.Intn(2))
	fmt.Fprintf(&b, `Cmd="/home/user%d/run_%d.sh"; Iwd="/home/user%d/proj%d"; Arguments="%d %d"; Out="out.%d.%d"; Err="err.%d.%d"; UserLog="/home/user%d/log.%d"; `,
		cl.owner, cl.reqTmpl, cl.owner, cl.id%7, cl.id, proc, cl.id, proc, cl.id, proc, cl.owner, cl.id)
	fmt.Fprintf(&b, `LastRemoteHost="slot1_%d@glidein_%d@node%d.site%d.example.org"; StartdPrincipal="execute-side@matchsession/10.%d.%d.%d"; `,
		rng.Intn(64), rng.Intn(99999), rng.Intn(5000), rng.Intn(80), rng.Intn(255), rng.Intn(255), rng.Intn(255))
	fmt.Fprintf(&b, `MachineAttrGLIDEIN_ResourceName0="Resource%d"; MachineAttrGLIDEIN_Site0="Site%d"; MachineAttrCpus0=%d; MachineAttrSlotWeight0=%d; `,
		rng.Intn(150), rng.Intn(80), 1+rng.Intn(8), 1+rng.Intn(8))
	fmt.Fprintf(&b, `RequestCpus=1; RequestGPUs=%d; JobUniverse=5; JobPrio=0; NiceUser=false; MaxHosts=1; MinHosts=1; CurrentHosts=0; `, cl.gpus)
	fmt.Fprintf(&b, `CondorVersion="$CondorVersion: 24.0.%d 2025-01-01 BuildID: 1 $"; CondorPlatform="$CondorPlatform: X86_64-AlmaLinux_9.4 $"; `, cl.id%3)
	fmt.Fprintf(&b, `ProjectName="Project%d"; JobBatchName="batch-%d"; TotalSubmitProcs=%d; ShouldTransferFiles="YES"; WhenToTransferOutput="ON_EXIT"; `,
		cl.owner%40, cl.id, 1+cl.id%500)
	fmt.Fprintf(&b, `TransferInputSizeMB=%d; BytesRecvd=%d.0; BytesSent=%d.0; TransferIn=false; StreamOut=false; StreamErr=false; WantRemoteIO=true; WantRemoteSyscalls=false; WantCheckpoint=false; `,
		rng.Intn(2000), rng.Intn(1<<30), rng.Intn(1<<28))
	fmt.Fprintf(&b, `OnExitHold=false; PeriodicHold=false; PeriodicRelease=false; PeriodicRemove=false; LeaveJobInQueue=false; `)
	fmt.Fprintf(&b, `CpusProvisioned=%d; MemoryProvisioned=%d; DiskProvisioned=%d; CpusUsage=%.6f; LastMatchTime=%d; JobCurrentStartDate=%d; `,
		1+rng.Intn(2), cl.memMB+rng.Intn(64), 1000000+rng.Intn(9000000), rng.Float64(), t0-run-5, t0-run)
	for j := range 50 {
		// Literal statistics a real history ad carries in volume (Recent*, Block*, *Date, *Count).
		fmt.Fprintf(&b, "JobStat%02d=%d; ", j, rng.Intn(1+(j+1)*(j+1)*1000))
	}
	for j := range 20 {
		fmt.Fprintf(&b, `JobStr%02d="%s%d"; `, j, "value-string-", rng.Intn(3+j))
	}
	// The expressions.
	b.WriteString("MemoryUsage=((ResidentSetSize + 1023) / 1024); ")
	if cl.defMem {
		b.WriteString("RequestMemory=ifthenelse(MemoryUsage =!= undefined,MemoryUsage,(ImageSize + 1023) / 1024); ")
	} else {
		fmt.Fprintf(&b, "RequestMemory=%d; ", cl.memMB)
	}
	if cl.defDisk {
		b.WriteString("RequestDisk=DiskUsage; ")
	} else {
		fmt.Fprintf(&b, "RequestDisk=%d; ", 1048576*(1+cl.id%4))
	}
	fmt.Fprintf(&b, "Requirements=%s; ", historyReqTemplates[cl.reqTmpl])
	if cl.periodic {
		b.WriteString("PeriodicRemove=(JobStatus == 2 && (time() - EnteredCurrentStatus) > 72000) || (NumJobStarts > 10); ")
	}
	if cl.release {
		b.WriteString("PeriodicRelease=(HoldReasonCode == 12 || HoldReasonCode == 13) && (NumJobStarts < 5) && ((time() - EnteredCurrentStatus) > 600); ")
	}
	if cl.hold {
		b.WriteString("PeriodicHold=(JobStatus == 2) && ((time() - EnteredCurrentStatus) > 86400); ")
	}
	if cl.dag {
		fmt.Fprintf(&b, `DAGManJobId=%d; DAGNodeName="node%d"; OnExitRemove=(ExitSignal =?= 11 || (ExitCode =!= undefined && ExitCode >= 0 && ExitCode <= 2)); `,
			cl.id-1, proc)
	} else {
		b.WriteString("OnExitRemove=true; ")
	}
	if cl.leave {
		b.WriteString("LeaveJobInQueue=(JobStatus == 4) && ((StageOutFinish =?= undefined) || (StageOutFinish == 0)); ")
	}
	if rng.Intn(10) < 6 {
		fmt.Fprintf(&b, `TransferInputStats=[ CedarFilesCountTotal=%d; CedarSizeBytesTotal=%d; OSDFFilesCountTotal=%d; OSDFSizeBytesTotal=%d ]; `,
			1+rng.Intn(5), rng.Intn(1<<20), rng.Intn(3), rng.Intn(1<<30))
		fmt.Fprintf(&b, `TransferOutputStats=[ CedarFilesCountTotal=%d; CedarSizeBytesTotal=%d ]; `, 1+rng.Intn(3), rng.Intn(1<<24))
	}
	b.WriteString("MyType=\"Job\" ]")
	return b.String()
}

// historyLikeStream yields n history-shaped ads in completion order: a pool of clusters runs at once,
// so neighbouring records mix several clusters, and a cluster retires after its procs complete.
func historyLikeStream(seed int64, n int, fn func(text string)) {
	rng := rand.New(rand.NewSource(seed))
	type live struct {
		cl        *historyCluster
		next, end int
	}
	next := 1000
	newLive := func() *live {
		next++
		size := []int{1, 1, 5, 10, 50, 100, 500, 1000}[rng.Intn(8)]
		return &live{cl: newHistoryCluster(rng, next), end: size}
	}
	pool := make([]*live, 24)
	for i := range pool {
		pool[i] = newLive()
	}
	for seq := range n {
		i := rng.Intn(len(pool))
		l := pool[i]
		fn(historyLikeAd(rng, l.cl, l.next, seq))
		l.next++
		if l.next >= l.end {
			pool[i] = newLive()
		}
	}
}

// remnantDictTally is what the per-block expression dictionary would save on a set of columnarized
// segments, simulated: each record's remnant is rewritten with every repeated node replaced by a
// two-byte reference and recompressed under the segment's own codec, and each block pays for its
// dictionary compressed under the region codec.
type remnantDictTally struct {
	segBytes, remBytes, payloadBytes int
	newRemBytes, dictBytes           int
	records, blocks                  int
	// Raw (uncompressed) remnant bytes by node class, and how many of them repeat within a block.
	rawExpr, rawExprRep, rawNested, rawNestedRep, rawSealed, rawLit int
	// Repeating expression bytes by attribute name (names only; never values), and how many distinct
	// expressions each block's dictionary holds.
	repByName   map[string]int
	dictEntries int
}

func (t remnantDictTally) saving() int { return t.remBytes - t.newRemBytes - t.dictBytes }

// remnantDictNodeClass classifies a top-level remnant node.
func remnantDictNodeClass(node []byte) string {
	switch {
	case wire.IsEncryptedNode(node):
		return "sealed"
	case storableInColumn(node):
		return "lit"
	case len(node) > 0 && (node[0] == 0x0A || node[0] == 0x0B):
		return "nested"
	}
	return "expr"
}

func measureRemnantDict(t *testing.T, c *Collection, withNested bool) remnantDictTally {
	t.Helper()
	tl := remnantDictTally{repByName: map[string]int{}}
	rc := c.regionCodec()
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			cn := seg.colNative.Load()
			if cn == nil {
				continue
			}
			tl.segBytes += seg.used
			tl.payloadBytes += int(recTotalLen(seg.data, cn.rec))
			k := 0
			for _, blk := range cn.seg.blocks {
				tl.blocks++
				type rec struct {
					key  int
					hdr  []byte
					n    int
					ents [][4][]byte // key bytes, node, class, name
				}
				recs := make([]rec, 0, blk.n)
				count := map[string]int{}
				for i := 0; i < blk.n; i++ {
					off := cn.seg.offAt(k + i)
					tl.remBytes += int(recTotalLen(seg.data, off))
					tl.records++
					raw, err := seg.codec.Decompress(nil, recAd(seg.data, off))
					if err != nil {
						t.Fatal(err)
					}
					hdr, _, n, inline, ok := wire.Ad(raw).SplitBody()
					if !ok {
						t.Fatal("remnant does not split")
					}
					r := rec{key: len(recKey(seg.data, off)), hdr: hdr, n: n}
					wire.Ad(raw).ForEachRaw(func(id uint32, name string, node []byte) bool {
						kb := wire.AppendKey(nil, inline, id, name)
						cls := remnantDictNodeClass(node)
						nm := name
						if nm == "" {
							if d := seg.dict.Load(); d != nil {
								nm = string(d.name(id))
							} else if n2, ok := c.intern.Name(id); ok {
								nm = n2
							}
						}
						r.ents = append(r.ents, [4][]byte{kb, append([]byte(nil), node...), []byte(cls), []byte(nm)})
						if cls == "expr" || (withNested && cls == "nested") {
							count[string(node)]++
						}
						return true
					})
					recs = append(recs, r)
				}
				k += blk.n
				// Dictionary: every node that repeats in the block and is longer than its reference.
				idx := map[string]int{}
				var dict []byte
				for _, r := range recs {
					for _, e := range r.ents {
						cls := string(e[2])
						switch cls {
						case "expr":
							tl.rawExpr += len(e[1])
						case "nested":
							tl.rawNested += len(e[1])
						case "sealed":
							tl.rawSealed += len(e[1])
						default:
							tl.rawLit += len(e[1])
						}
						if count[string(e[1])] < 2 {
							continue
						}
						switch cls {
						case "expr":
							tl.rawExprRep += len(e[1])
							tl.repByName[strings.ToLower(string(e[3]))] += len(e[1])
						case "nested":
							tl.rawNestedRep += len(e[1])
						}
						if _, ok := idx[string(e[1])]; !ok && len(e[1]) > 3 {
							idx[string(e[1])] = len(idx)
							dict = binary.AppendUvarint(dict, uint64(len(e[1])))
							dict = append(dict, e[1]...)
						}
					}
				}
				tl.dictEntries += len(idx)
				if len(idx) > 0 {
					tl.dictBytes += len(rc.Compress(nil, dict)) + 8
				}
				for _, r := range recs {
					var ents []byte
					for _, e := range r.ents {
						ents = append(ents, e[0]...)
						if j, ok := idx[string(e[1])]; ok {
							ents = append(ents, 0x18)
							ents = binary.AppendUvarint(ents, uint64(j))
							continue
						}
						ents = append(ents, e[1]...)
					}
					body := wire.BuildAd(nil, r.hdr, r.n, ents)
					tl.newRemBytes += recordLen(r.key, len(seg.codec.Compress(nil, body)))
				}
			}
		}
	}
	return tl
}

// readHistoryTail parses an HTCondor history file (old-ClassAd text, each ad followed by a `***`
// banner line), gzip-compressed or not. A file cut from the middle of a history starts mid-ad, so
// everything before the first banner is skipped. Ads that do not parse are skipped.
func readHistoryTail(tb testing.TB, path string, fn func(*classad.ClassAd)) {
	tb.Helper()
	f, err := os.Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, gerr := gzip.NewReader(f)
		if gerr != nil {
			tb.Fatal(gerr)
		}
		defer gz.Close()
		r = gz
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	started := false
	var b strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "***") {
			if started && b.Len() > 0 {
				if ad, perr := classad.ParseOld(b.String()); perr == nil {
					fn(ad)
				}
			}
			started = true
			b.Reset()
			continue
		}
		if started {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if err := sc.Err(); err != nil {
		tb.Fatal(err)
	}
}

// remnantDictCorpus is a named set of ads to measure or benchmark over.
type remnantDictCorpus struct {
	name string
	ads  []*classad.ClassAd
}

// remnantDictCorpora returns the corpora the measurement and benchmark run over: the history files
// named by CLASSAD_REMDICT_CORPUS (comma-separated paths; real data stays outside the repository),
// the OSPool slot fixture when present, and the synthetic history set.
func remnantDictCorpora(tb testing.TB, synthetic int) []remnantDictCorpus {
	var out []remnantDictCorpus
	for _, p := range strings.Split(os.Getenv("CLASSAD_REMDICT_CORPUS"), ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		var ads []*classad.ClassAd
		readHistoryTail(tb, p, func(ad *classad.ClassAd) { ads = append(ads, ad) })
		out = append(out, remnantDictCorpus{strings.TrimSuffix(filepath.Base(p), ".gz"), ads})
	}
	if data, err := os.ReadFile("testdata/ospool_slots.ldif"); err == nil && os.Getenv("CLASSAD_REMDICT_OSPOOL") != "" {
		var ads []*classad.ClassAd
		for _, blk := range strings.Split(string(data), "\n\n") {
			if strings.TrimSpace(blk) == "" {
				continue
			}
			if ad, perr := classad.ParseOld(blk); perr == nil {
				ads = append(ads, ad)
			}
		}
		out = append(out, remnantDictCorpus{"ospool-slots", ads})
	}
	if synthetic > 0 {
		var hist []*classad.ClassAd
		historyLikeStream(1, synthetic, func(text string) { hist = append(hist, mustAd(tb, text)) })
		out = append(out, remnantDictCorpus{"history-like", hist})
	}
	return out
}

// remnantDictStore loads ads into a fresh persistent store of the given shape and columnarizes it:
// an archive (as history is ingested) or a mutable table keyed by position, each either as first
// written (dictionary-less zstd, names inline) or after a dictionary retrain (trained zstd for what
// is written next; interned segments).
func remnantDictStore(tb testing.TB, ads []*classad.ClassAd, mutable, retrain bool) *Collection {
	tb.Helper()
	codec, err := NewZSTDCodec(nil)
	if err != nil {
		tb.Fatal(err)
	}
	dir := tb.TempDir()
	var c *Collection
	var a *Archive
	if mutable {
		if c, err = Open(Options{Dir: dir, Shards: 1, SegmentSize: 4 << 20, Codec: codec}); err != nil {
			tb.Fatal(err)
		}
	} else {
		if a, err = CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 4 << 20, Codec: codec}); err != nil {
			tb.Fatal(err)
		}
		c = a.c
	}
	tb.Cleanup(func() { _ = c.Close() })
	warm := len(ads) / 5
	for i, ad := range ads {
		if retrain && i == warm {
			if _, rerr := c.RetrainDict(0); rerr != nil {
				tb.Fatal(rerr)
			}
		}
		if mutable {
			err = c.Put([]byte(fmt.Sprintf("%d", i)), ad)
		} else {
			err = a.Append(ad)
		}
		if err != nil {
			tb.Fatal(err)
		}
	}
	if retrain {
		// Interns (archive) or recompacts (mutable) what sealed since the first retrain, as the
		// periodic maintenance does.
		if _, rerr := c.RetrainDict(0); rerr != nil {
			tb.Fatal(rerr)
		}
	}
	c.colBudget = 1 << 20
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		tb.Fatal("schema scan did not enable")
	}
	return c
}

// TestRemnantExprDictMeasure measures what a per-block dictionary of repeated expression nodes in
// columnarized remnants saves, as a share of columnarized segment bytes, on every corpus
// remnantDictCorpora yields and every store shape remnantDictStore builds. Only aggregates and
// attribute names are logged.
//
// Measurement, not a check: run with CLASSAD_BENCH_REMDICT=1.
func TestRemnantExprDictMeasure(t *testing.T) {
	if os.Getenv("CLASSAD_BENCH_REMDICT") == "" {
		t.Skip("set CLASSAD_BENCH_REMDICT=1 to measure")
	}
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	for _, cp := range remnantDictCorpora(t, 30000) {
		for _, shape := range []struct{ mutable, retrain bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
			c := remnantDictStore(t, cp.ads, shape.mutable, shape.retrain)
			tl := measureRemnantDict(t, c, false)
			pct := func(x, of int) float64 { return 100 * float64(x) / float64(max(of, 1)) }
			kind := "archive"
			if shape.mutable {
				kind = "mutable"
			}
			t.Logf("%s %s retrain=%v: %d ads, %d columnarized recs, %d blocks (%.0f rec/blk); seg %d B (%.0f B/rec), remnants %.1f%% of seg, payload %.1f%%",
				cp.name, kind, shape.retrain, len(cp.ads), tl.records, tl.blocks, float64(tl.records)/float64(max(tl.blocks, 1)),
				tl.segBytes, float64(tl.segBytes)/float64(max(tl.records, 1)), pct(tl.remBytes, tl.segBytes), pct(tl.payloadBytes, tl.segBytes))
			t.Logf("   raw remnant bytes/rec: expr %.0f (repeating in block %.0f), nested %.0f, sealed %.0f; dictionary entries %.1f/block",
				float64(tl.rawExpr)/float64(max(tl.records, 1)), float64(tl.rawExprRep)/float64(max(tl.records, 1)),
				float64(tl.rawNested)/float64(max(tl.records, 1)), float64(tl.rawSealed)/float64(max(tl.records, 1)),
				float64(tl.dictEntries)/float64(max(tl.blocks, 1)))
			t.Logf("   SAVING %.2f%% of segment bytes: remnants %d -> %d B + dictionaries %d B (%.1f B/rec)",
				pct(tl.saving(), tl.segBytes), tl.remBytes, tl.newRemBytes, tl.dictBytes, float64(tl.saving())/float64(max(tl.records, 1)))
			if !shape.mutable && !shape.retrain {
				type nb struct {
					name string
					b    int
				}
				var top []nb
				for n, b := range tl.repByName {
					top = append(top, nb{n, b})
				}
				sort.Slice(top, func(i, j int) bool { return top[i].b > top[j].b })
				var parts []string
				for i := 0; i < len(top) && i < 15; i++ {
					parts = append(parts, fmt.Sprintf("%s %.1f%%", top[i].name, pct(top[i].b, tl.rawExprRep)))
				}
				t.Logf("   repeating expression bytes by attribute (%d names): %s", len(top), strings.Join(parts, ", "))
			}
		}
	}
}

// TestRemnantDictBench compares stores columnarized with and without remnant dictionaries on every
// corpus remnantDictCorpora yields: columnarized segment bytes, columnarize time, a full-ad scan, a
// projected query, a projected wire scan and a decoding scan. The arms are interleaved, their order
// alternates each round, and medians are reported with the machine's load average. Only aggregates
// are logged.
//
// Measurement, not a check: run with CLASSAD_BENCH_REMDICT=1 (CLASSAD_REMDICT_ROUNDS, default 5).
func TestRemnantDictBench(t *testing.T) {
	if os.Getenv("CLASSAD_BENCH_REMDICT") == "" {
		t.Skip("set CLASSAD_BENCH_REMDICT=1 to measure")
	}
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	rounds := 5
	if v, err := strconv.Atoi(os.Getenv("CLASSAD_REMDICT_ROUNDS")); err == nil && v > 0 {
		rounds = v
	}
	load := func() string {
		out, _ := exec.Command("uptime").Output()
		s := strings.TrimSpace(string(out))
		if i := strings.Index(s, "load average"); i >= 0 {
			return s[i:]
		}
		return s
	}
	proj := []string{"Requirements", "MemoryUsage", "Owner", "JobStatus", "RemoteWallClockTime", "RequestMemory"}
	q, err := vm.Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	type sample struct{ colz, full, projq, wireScan, decode time.Duration }
	median := func(ds []time.Duration) time.Duration {
		s := append([]time.Duration(nil), ds...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		return s[len(s)/2]
	}
	for _, cp := range remnantDictCorpora(t, 30000) {
		for _, retrain := range []bool{false, true} {
			t.Logf("%s retrain=%v: %d ads; %s", cp.name, retrain, len(cp.ads), load())
			samples := map[int][]sample{}
			bytesOf := map[int]int{}
			recsOf := map[int]int{}
			for r := range rounds {
				arms := []int{-1, 1}
				if r%2 == 1 {
					arms = []int{1, -1}
				}
				for _, arm := range arms {
					codec, err := NewZSTDCodec(nil)
					if err != nil {
						t.Fatal(err)
					}
					dir := t.TempDir()
					a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 4 << 20, Codec: codec, RemnantDictionary: arm})
					if err != nil {
						t.Fatal(err)
					}
					warm := len(cp.ads) / 5
					for i, ad := range cp.ads {
						if retrain && i == warm {
							if _, err := a.RetrainDict(0); err != nil {
								t.Fatal(err)
							}
						}
						if err := a.Append(ad); err != nil {
							t.Fatal(err)
						}
					}
					if retrain {
						if _, err := a.RetrainDict(0); err != nil {
							t.Fatal(err)
						}
					}
					a.c.colBudget = 1 << 20
					var s sample
					st := time.Now()
					if !a.BuildAndEnableSchemaScan(4096, 8) {
						t.Fatal("schema scan did not enable")
					}
					for a.c.ColumnarizeSealed() > 0 {
					}
					s.colz = time.Since(st)
					segBytes, recs := 0, 0
					for _, seg := range a.c.shards[0].segs {
						if seg != nil && seg.colNative.Load() != nil {
							segBytes += seg.used
							recs += seg.colNative.Load().seg.offsLen()
						}
					}
					bytesOf[arm], recsOf[arm] = segBytes, recs
					timeIt := func(fn func()) time.Duration {
						fn() // warm the block cache and the page cache
						st := time.Now()
						fn()
						return time.Since(st)
					}
					s.full = timeIt(func() {
						for range a.c.ScanRawWire(nil, false) {
						}
					})
					s.projq = timeIt(func() {
						for range a.c.QueryRawWire(q, proj, false) {
						}
					})
					s.wireScan = timeIt(func() {
						for range a.c.ScanRawWire(proj, false) {
						}
					})
					s.decode = timeIt(func() {
						for range a.c.Scan() {
						}
					})
					samples[arm] = append(samples[arm], s)
					_ = a.Close()
					_ = os.RemoveAll(dir)
				}
			}
			for _, arm := range []int{-1, 1} {
				var colz, full, projq, wireScan, decode []time.Duration
				for _, s := range samples[arm] {
					colz, full, projq, wireScan, decode = append(colz, s.colz), append(full, s.full), append(projq, s.projq), append(wireScan, s.wireScan), append(decode, s.decode)
				}
				name := "without"
				if arm > 0 {
					name = "with   "
				}
				t.Logf("  %s: columnarized %d B over %d recs (%.0f B/rec); columnarize %v; full scan %v; projected query %v; projected wire scan %v; decoding scan %v",
					name, bytesOf[arm], recsOf[arm], float64(bytesOf[arm])/float64(max(recsOf[arm], 1)),
					median(colz).Round(time.Millisecond), median(full).Round(time.Millisecond), median(projq).Round(time.Millisecond),
					median(wireScan).Round(time.Millisecond), median(decode).Round(time.Millisecond))
			}
			t.Logf("  segment bytes saved: %.2f%%; %s", 100*float64(bytesOf[-1]-bytesOf[1])/float64(max(bytesOf[-1], 1)), load())
		}
	}
}
