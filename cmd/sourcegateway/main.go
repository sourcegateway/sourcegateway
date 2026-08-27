// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sourcegateway/sourcegateway/internal/cmd"
)

// main runs the Source Gateway process.
func main() {
	if err := cmd.Run(context.Background(), os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "sourcegateway:", err)
		os.Exit(1)
	}
}
