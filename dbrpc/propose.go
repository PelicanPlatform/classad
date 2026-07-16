package dbrpc

// Consistent-HA write routing. In a raft-replicated deployment the local store must not be
// written directly by clients -- every write goes through the consensus log so all replicas
// apply it identically. A Server configured with a propose hook accumulates each committing
// transaction's mutations and hands them to the hook on commit (instead of committing the
// local transaction); the hook proposes them to raft, whose FSM is the store's sole writer.

// WriteKind identifies a buffered mutation. The values match the semantics of the qmgmt /
// classad-log operations, so the hook can translate a batch 1:1 into a raft mutation batch.
type WriteKind uint8

const (
	WriteNewClassAd      WriteKind = iota // store Value (old-ClassAd text) under Key
	WriteDestroyClassAd                   // remove Key
	WriteSetAttribute                     // set Key's attribute Name to expression Value
	WriteDeleteAttribute                  // remove Key's attribute Name
)

// WriteOp is one buffered mutation from a transaction, ready to be proposed to consensus.
type WriteOp struct {
	Kind        WriteKind
	Key         string
	Name, Value string
}

// ProposeFunc applies a committing transaction's ops to the replicated store via consensus.
// table is the transaction's table; ops preserve issue order. It returns nil on a
// quorum-committed apply, or an error (e.g. not-the-leader) that is surfaced to the client.
type ProposeFunc func(table string, ops []WriteOp) error

// SetProposeHook routes committing writes through fn (raft) instead of the local store. Call
// once before serving. Passing nil restores direct local commits (the default). When set,
// the server is otherwise read-only against its local store: the hook is the sole writer.
func (s *Server) SetProposeHook(fn ProposeFunc) { s.propose = fn }
