package wasmudf

import (
	"encoding/binary"
	"unsafe"
)

// Bulk moves between a Go []int64 and the little-endian 8-byte lanes of guest
// memory.
//
// wasm linear memory is little-endian by definition, so on a little-endian host
// an int64 column and the bytes a guest reads are the SAME bytes in the same
// order, and copying them one element at a time through binary.LittleEndian is a
// loop doing what a memmove does. The profile put that loop (writeInts) at 21% of
// a trusted 4096-record batch, which is the whole reason this file exists.
//
// On a big-endian host the two are not the same bytes, so the element loop is not
// an optimization to be removed -- it is the conversion. nativeLittle selects
// between them once, at init, rather than per call.
var nativeLittle = func() bool {
	var x uint16 = 1
	return *(*byte)(unsafe.Pointer(&x)) == 1
}()

// i64Bytes views an int64 slice as the bytes it already is. Only called where
// nativeLittle, because only there do those bytes mean what the guest will read.
func i64Bytes(xs []int64) []byte {
	if len(xs) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&xs[0])), len(xs)*8)
}

func u64Bytes(xs []uint64) []byte {
	if len(xs) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&xs[0])), len(xs)*8)
}

// putInts writes xs into dst, which must be len(xs)*8 bytes of guest memory.
func putInts(dst []byte, xs []int64) {
	if nativeLittle {
		copy(dst, i64Bytes(xs))
		return
	}
	for i, x := range xs {
		binary.LittleEndian.PutUint64(dst[i*8:], uint64(x))
	}
}

func putWords(dst []byte, xs []uint64) {
	if nativeLittle {
		copy(dst, u64Bytes(xs))
		return
	}
	for i, x := range xs {
		binary.LittleEndian.PutUint64(dst[i*8:], x)
	}
}

// getInts reads n 8-byte lanes from src into dst.
func getInts(dst []int64, src []byte, n int) {
	if nativeLittle {
		copy(i64Bytes(dst[:n]), src[:n*8])
		return
	}
	for i := range n {
		dst[i] = int64(binary.LittleEndian.Uint64(src[i*8:]))
	}
}

// fillStates writes one state byte across n elements.
//
// The doubling copy rather than a byte loop: Go turns `for i := range s { s[i] =
// 0 }` into a memclr but has no such idiom for a nonzero byte, so a uniform state
// plane would be written a byte at a time -- which is what the columnar form
// exists to stop doing. Each copy is a memmove and there are log2(n) of them.
func fillStates(st []uint8, n int, s uint8) {
	if n <= 0 {
		return
	}
	st = st[:n]
	st[0] = s
	for i := 1; i < n; i *= 2 {
		copy(st[i:], st[:i])
	}
}
