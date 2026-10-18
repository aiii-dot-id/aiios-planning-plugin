//go:build !wasm_unknown

package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newPlanningPlugin().Serve("planning-ready"); err != nil {
		fmt.Fprintf(os.Stderr, "id.aeon.planning: %v\n", err)
		os.Exit(1)
	}
}
