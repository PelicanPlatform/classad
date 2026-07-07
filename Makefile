# Makefile — developer convenience targets.
#
# Benchmark corpus generation for collections/vm.
#
# The benchmark corpus is a small, committed, gzipped sample of real ClassAds
# (collections/vm/testdata/pool_sample.ads.gz), sampled proportionally by ad
# type from a `condor_status -any -l` dump. The multi-GB source dumps are NOT
# committed (see .gitignore: *.ads). Regenerate the corpus with:
#
#     make corpus                    # download from the default pool, then sample
#     make corpus POOL=<collector>   # use a different collector
#     make corpus DUMP=/path/to.ads  # sample an existing dump (skip the download)
#     make corpus TARGET_GZ=250000   # aim for a different compressed size (bytes)
#
# Downloading the full pool is large (~1.4GB) and needs ~4GB RAM. The generated
# corpus is what gets committed; the dump is discarded.

POOL      ?= cm-1.ospool.osg-htc.org
DUMP      ?=
TARGET_GZ ?= 500000
CORPUS    := collections/vm/testdata/pool_sample.ads.gz
GEN       := collections/vm/testdata/gen_corpus.py

.PHONY: corpus
corpus:
ifeq ($(DUMP),)
	@tmp=$$(mktemp $${TMPDIR:-/tmp}/condor-any.XXXXXX.ads); \
	echo "Downloading -any dump from $(POOL) (~1.4GB, needs ~4GB RAM)..."; \
	condor_status -any -pool $(POOL) -l > "$$tmp"; \
	python3 $(GEN) "$$tmp" $(CORPUS) $(TARGET_GZ); \
	rm -f "$$tmp"
else
	python3 $(GEN) "$(DUMP)" $(CORPUS) $(TARGET_GZ)
endif
	@echo "Wrote $(CORPUS) ($$(wc -c < $(CORPUS)) bytes)"
