module github.com/PelicanPlatform/classad/collections

go 1.25.0

require github.com/PelicanPlatform/classad v0.0.0

require (
	github.com/RoaringBitmap/roaring/v2 v2.19.0
	github.com/klauspost/compress v1.19.0
	github.com/tidwall/btree v1.8.1
	golang.org/x/sys v0.46.0
)

require (
	github.com/bits-and-blooms/bitset v1.24.4 // indirect
	github.com/mschoch/smat v0.2.0 // indirect
)

replace github.com/PelicanPlatform/classad => ../
