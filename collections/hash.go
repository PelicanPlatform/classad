package collections

// Hasher maps a stable key to a 64-bit hash. The hash routes a key to a shard
// and indexes the per-shard directory; it is never used as the ad's identity
// (the full key is stored inline in each record and compared on lookup), so hash
// collisions are resolved exactly and never lose data.
type Hasher interface {
	Hash(key []byte) uint64
}

// fnvHasher is the zero-dependency default: 64-bit FNV-1a.
type fnvHasher struct{}

const (
	fnvOffset64 = 1469598103934665603
	fnvPrime64  = 1099511628211
)

func (fnvHasher) Hash(key []byte) uint64 {
	h := uint64(fnvOffset64)
	for _, c := range key {
		h ^= uint64(c)
		h *= fnvPrime64
	}
	return h
}
