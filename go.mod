// Module path and Go version for processkit-go.
//
// The `go` directive is the single source of truth for the language version: CI,
// CodeQL, and the release workflow all read it via `go-version-file: go.mod`, and
// it is the minimum version the module compiles on (Go's MSRV equivalent). Raise
// it when you adopt a newer language/std feature.
//
// Floor is 1.25: the lowest Go that current golang.org/x/sys + govulncheck support.
// CgroupFD (atomic cgroup-v2 placement at spawn — the original reason we need
// >= 1.22) is satisfied. Raised from an initial 1.22 to take x/sys security fixes
// (GO-2026-5024) and avoid recurring tooling friction with the 1.22 floor.
module github.com/ZelAnton/processkit-go

go 1.25.0

require golang.org/x/sys v0.46.0
