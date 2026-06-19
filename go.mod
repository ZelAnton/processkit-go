// Module path and Go version for processkit-go.
//
// The `go` directive is the single source of truth for the language version: CI,
// CodeQL, and the release workflow all read it via `go-version-file: go.mod`, and
// it is the minimum version the module compiles on (Go's MSRV equivalent). Raise
// it when you adopt a newer language/std feature. Floor is 1.22 for
// SysProcAttr.UseCgroupFD / CgroupFD (atomic cgroup-v2 placement at spawn).
module github.com/ZelAnton/processkit-go

go 1.22
