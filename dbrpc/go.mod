module github.com/PelicanPlatform/classad/dbrpc

go 1.25.0

require (
	github.com/PelicanPlatform/classad v0.7.0
	github.com/PelicanPlatform/classad/db v0.7.1
	github.com/bbockelm/cedar v0.5.3
)

require (
	github.com/PelicanPlatform/classad/collections v0.7.0 // indirect
	github.com/RoaringBitmap/roaring/v2 v2.19.0 // indirect
	github.com/bits-and-blooms/bitset v1.24.4 // indirect
	github.com/klauspost/compress v1.19.0 // indirect
	github.com/mschoch/smat v0.2.0 // indirect
	github.com/tidwall/btree v1.8.1 // indirect
	golang.org/x/sys v0.43.0 // indirect
)

replace github.com/PelicanPlatform/classad => ../

replace github.com/PelicanPlatform/classad/collections => ../collections

replace github.com/PelicanPlatform/classad/db => ../db

replace github.com/bbockelm/cedar => ../../golang-cedar
