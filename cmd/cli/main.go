// Command cli builds the `personal-ai` command-line client (Phase 8). It will
// talk to the platform through the REST API. Until the adapter exists it exits
// with a clear error rather than pretending to work.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "error: the personal-ai CLI is not implemented yet (planned for Phase 8)")
	os.Exit(2)
}
