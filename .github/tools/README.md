# CI tool module

An isolated Go module whose only purpose is to pin the version of the
parser generator CI runs.

Every job in `test.yml` (plus `soak.yml` and `enospc.yml`) has to generate
`parser/y.go` from `parser/classad.y` before it can build this repo. That
used to be `go install golang.org/x/tools/cmd/goyacc@latest`, repeated in
seven places: each run fetched whatever had been published most recently
and verified it against nothing. A parser generator runs with full access
to the build, so "whatever was published this morning" is not a version
policy.

Keeping the requirement here gives it a home Dependabot understands — the
`gomod` ecosystem scans every `go.mod` listed in `.github/dependabot.yml`
— so a new x/tools release arrives as an ordinary bump PR, after the
configured cooldown, with the version recorded in this module's `go.sum`
and verified on download.

It is a module of its own so that goyacc's dependencies never enter the
module graph of anything that imports `classad`.

Nothing imports this module. CI builds the binary with:

    (cd .github/tools && GOWORK=off go build -o "$BINDIR/goyacc" golang.org/x/tools/cmd/goyacc)
