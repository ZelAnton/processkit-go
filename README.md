# processkit-go

Kernel-backed, no-orphan child-process trees for Go — a native processkit implementation.

## Requirements

- Go 1.22 or later

## Installation

Available on [pkg.go.dev](https://pkg.go.dev/github.com/ZelAnton/processkit-go).

```sh
go get github.com/ZelAnton/processkit-go
```

## Usage

```go
package main

import (
	"fmt"

	"github.com/ZelAnton/processkit-go"
)

func main() {
	fmt.Println(processkit.Greet("World")) // Hello, World!
}
```

## Changelog

See [CHANGELOG.md](CHANGELOG.md) for the version history.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for build/test instructions and
conventions. To report a security issue, follow [SECURITY.md](SECURITY.md) —
please do not open a public issue.

## License

This project is licensed under the [MIT License](LICENSE).
