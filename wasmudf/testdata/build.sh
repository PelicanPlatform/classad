#!/bin/sh
# Rebuild the .wasm fixtures from the .wat sources next to them, and record what
# was built from what.
#
# Both the source and the binary are committed: the .wat is what a reviewer reads,
# and the .wasm is what the tests load. The tests load the committed binary rather
# than assembling one, so the suite needs no wasm toolchain -- a test that skipped
# itself when wat2wasm was missing would be green on every machine that cannot run
# it, which is the state this arrangement exists to avoid.
#
# fixtures.sha256 is what keeps the pair honest. Editing a .wat without running
# this script leaves the manifest disagreeing with the source, and
# TestFixtureBinariesAreCurrent fails by name -- which is the real hazard, because
# the tests would otherwise stay green while testing the old binary and the source
# a reviewer reads said something else.
#
# The manifest, rather than rebuilding and comparing bytes in CI: wat2wasm's output
# is only comparable against the SAME wabt, and installing whatever a runner's
# distribution packages would make the check fail on an encoder change with nothing
# actually stale. (It also would not have worked: older wabt needs
# --enable-annotations for the custom section in trustclaim.wat and newer wabt
# rejects that flag.)
#
# Requires wabt (https://github.com/WebAssembly/wabt): `brew install wabt`,
# `apt install wabt`.
set -eu
cd "$(dirname "$0")"

for f in *.wat; do
	# Annotations carry the custom section in trustclaim.wat. They are on by
	# default in wabt 1.0.37 and later and need the flag before that, and the
	# flag is rejected by the versions that default to on -- so try both.
	if ! wat2wasm --enable-annotations "$f" -o "${f%.wat}.wasm" 2>/dev/null; then
		wat2wasm "$f" -o "${f%.wat}.wasm"
	fi
	echo "built ${f%.wat}.wasm"
done

# Hash the SOURCES. The question the manifest answers is "was every .wat compiled
# by this script since it was last edited", so hashing the inputs is the whole
# point; hashing the outputs would only restate what is already on disk.
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum *.wat > fixtures.sha256
else
	shasum -a 256 *.wat > fixtures.sha256
fi
echo "recorded $(wc -l < fixtures.sha256 | tr -d ' ') sources in fixtures.sha256"
