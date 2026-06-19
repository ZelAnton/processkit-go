# processkit-go

Kernel-backed, no-orphan child-process management for Go — a native implementation
of the [processkit](https://github.com/ZelAnton/ProcessKit-rs) model.

Every process you start — and everything *it* spawns — runs inside a kill-on-drop
OS container (a Windows **Job Object**, or a POSIX **process group**), so no
descendant ever outlives your run. Capture output, read exit codes, set timeouts,
and cancel through `context.Context` — with typed, `errors.Is`/`errors.As`-friendly
errors.

> **Status:** early (v0.x). The API is still taking shape and is **not yet frozen**.

## Requirements

- Go 1.25 or later
- Windows or Unix (Linux, macOS, the BSDs)

## Installation

```sh
go get github.com/ZelAnton/processkit-go
```

## Usage

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ZelAnton/processkit-go"
)

func main() {
	ctx := context.Background()

	// Run-and-capture: a non-zero exit is data, not an error.
	res, err := processkit.Command("git", "rev-parse", "HEAD").Output(ctx)
	if err != nil {
		log.Fatal(err) // spawn failure, cancelled context, …
	}
	fmt.Println(res.Stdout(), res.Outcome())

	// Require success and get trimmed stdout directly.
	version, err := processkit.Command("go", "version").Run(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(version)
}
```

Pick the verb that fits: `Output` (the full `Result`), `Run` (trimmed stdout, must
succeed), `ExitCode` (the code), or `Probe` (a yes/no predicate). Bound a run with
`.WithTimeout(d)` and tear the whole tree down by cancelling the `context`.

## Changelog

See [CHANGELOG.md](CHANGELOG.md) for the version history.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for build/test instructions and
conventions. To report a security issue, follow [SECURITY.md](SECURITY.md) —
please do not open a public issue.

## License

This project is licensed under the [MIT License](LICENSE).
