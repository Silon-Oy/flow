// Command flow-runner is the Flow runner daemon. It pulls work from the central
// service (POST /v1/leases/acquire) and runs the S1–S12 orchestration per-run in
// a hardened container.
//
// Vaihe 0: placeholder so `go build ./...` covers the binary. The pull loop and
// orchestration land in Vaihe 1.
package main

import "fmt"

func main() {
	fmt.Println("flow-runner: not yet implemented (Vaihe 0 skeleton)")
}
