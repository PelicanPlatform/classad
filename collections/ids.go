package collections

// ATTRIBUTE ID SPACES.
//
// An attribute id here is always a small integer, and there are several unrelated numberings of the
// same names. Mixing two of them does not fail: it reads ANOTHER attribute, whose value then comes
// back under the asked-for name. So each numbering is its own type, and a value moves between them
// only in the translation functions: idRemap's methods, segIDRemap, segIDMapper, globalIDOf, the
// segDictHandle accessors, and the places an id is decoded from or encoded to bytes. A conversion
// written anywhere else, such as storedID(g) or globalID(s), is the bug these types exist to prevent.
//
// All of these are defined types over uint32, so they cost nothing: a conversion between them is a
// change of type only and emits no instruction. That is also why the translation points convert in
// place rather than through one-line helper functions: a helper is free only where it is inlined, and
// the inliner does not inline calls inside a closure body it has itself copied into a caller.
//
// A record wire (wire.Ad) is keyed by whichever space its segment uses -- a sealed segment with a
// dictionary keys its records by segID, anything else by globalID -- and wire knows nothing of that,
// so its keys are bare uint32s; a key is typed where it leaves wire and untyped where it enters.

// globalID is this process's intern id for an attribute (Collection.intern). It is handed out in
// first-seen order at every Open, so it means nothing to another process and must never be written
// to disk or compared with an id read from it.
type globalID uint32

// segID is an attribute's id in a sealed segment's own dictionary (segDictHandle). It is durable --
// it travels with the segment -- but means nothing in any other segment.
type segID uint32

// storedID is the id a columnar payload's cold tail was WRITTEN with: a segID for a payload keyed by
// its segment's dictionary, or the writing process's globalID for one that is not. A reader turns a
// globalID into one only through idRemap.
type storedID uint32
