//go:build !js || !wasm

// This file exists so that `go build ./...` and `go vet ./...` succeed on a
// normal host. The real program is main.go, which is constrained to js/wasm;
// without a stub the package would have no buildable files off that platform
// and every repo-wide command would fail on it.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"polyql-wasm is the browser build; compile it with GOOS=js GOARCH=wasm (see `make playground`).")
	os.Exit(2)
}
