#!/bin/sh
# Rebuild the .wasm fixtures from the .wat sources next to them.
#
# Both the source and the binary are committed: the .wat is what a reviewer reads,
# and the .wasm is what the tests load. The tests load the committed binary rather
# than assembling one, so the suite needs no wasm toolchain -- a test that skipped
# itself when wat2wasm was missing would be green on every machine that cannot
# run it, which is the state this arrangement exists to avoid.
#
# Requires wabt (https://github.com/WebAssembly/wabt): `brew install wabt`,
# `apt install wabt`.
set -eu
cd "$(dirname "$0")"
for f in *.wat; do
	wat2wasm "$f" -o "${f%.wat}.wasm"
	echo "built ${f%.wat}.wasm"
done
