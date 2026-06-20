// Module path and Go version for processkit-go.
//
// The `go` directive is the language floor — the minimum version the module
// compiles on (Go's MSRV equivalent) — and CI/CodeQL/release read it via
// `go-version-file: go.mod`. Raise it when you adopt a newer language/std feature.
//
// Floor is 1.25: the lowest Go that current golang.org/x/sys + govulncheck support.
// CgroupFD (atomic cgroup-v2 placement at spawn — the original reason we need
// >= 1.22) is satisfied. Raised from an initial 1.22 to take x/sys security fixes
// (GO-2026-5024) and avoid recurring tooling friction with the 1.22 floor.
//
// The `toolchain` directive pins the *build* toolchain to a patched release so CI
// builds against a fixed standard library — separate from the language floor.
// Bumped to 1.25.10 for the net stdlib fix (GO-2026-4971), reached via WaitForPort.
module github.com/ZelAnton/processkit-go

go 1.25.0

toolchain go1.25.10

require golang.org/x/sys v0.46.0
