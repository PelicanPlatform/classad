package wire

// IsDelta reports whether a decoded (decompressed) record holds only changed attributes
// rather than a whole ad. A record too short to carry a header is not a delta -- callers
// treat anything unrecognized as a full record, which is the safe direction: a full record
// read as a delta would send the reader hunting for a base that does not exist.
func IsDelta(rec []byte) bool {
	return len(rec) >= 3 && rec[0] == magicByte && rec[1] == formatVer && rec[2]&flagDelta != 0
}
