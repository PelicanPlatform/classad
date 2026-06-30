# golang-classads dev container

Rocky Linux 9 image with Go, HTCondor, and the HTCondor **Python ClassAd
bindings** preinstalled. It gives you both implementations of ClassAd
evaluation side by side:

| Implementation | How to reach it |
| --- | --- |
| **Go** (this repo) | `go run ./cmd/classad-parser '[ x = 1 + 2 ]'`, or import `github.com/PelicanPlatform/classad/...` |
| **Reference C++** (via Python) | `python3 -c "import classad2 as c; ..."` |

Both were smoke-tested at build time and agree on `[ x = 10; y = x + 5 ]` → `y = 15`.

## Goal: differential fuzzer

The reason this container exists is to run a fuzzer that:

1. Generates random ClassAds / ClassAd expressions.
2. Evaluates each in **both** the Go library and the reference C++
   implementation (`classad2` Python module).
3. Diffs the results and reports any disagreement (different value, one side
   errors and the other doesn't, parse mismatch, etc.).

The `classad2` module is the ground truth — it wraps the same C++ ClassAd
library HTCondor ships. Older docs reference `import classad`; this image has
`classad2` (the supported modern binding). A tiny compatibility shim:

```python
try:
    import classad2 as classad
except ImportError:
    import classad
```

Useful `classad2` entry points: `classad.parseOne(str)` /
`classad.parseAds(str)` to parse, `ad.eval("attr")` to evaluate an attribute,
and `classad.ExprTree(str)` + `.eval()` to evaluate a bare expression.

## Building / running

VS Code "Reopen in Container" uses `.devcontainer/devcontainer.json`. The repo
is bind-mounted at `/workspace`. To build/run manually:

```bash
docker build -f .devcontainer/Dockerfile -t golang-classads-dev .
docker run --rm -it -v "$PWD:/workspace" golang-classads-dev bash
```
