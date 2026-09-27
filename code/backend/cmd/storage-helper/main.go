// Command storage-helper performs one bounded, read-only filesystem probe over
// standard input and output: open one folder and report the access result and
// its mount point and filesystem type. It runs as a separate process so a
// hanging OS call on a dead network share can be killed without freezing
// Replicaro.
package main

import (
	"io"
	"os"

	"github.com/local/replicaro/storageidentity"
)

func serve(reader io.Reader, writer io.Writer) error {
	return storageidentity.ServeHelper(
		reader,
		writer,
		storageidentity.DefaultMaxInput,
		storageidentity.DefaultMaxOutput,
	)
}

func main() {
	if err := serve(os.Stdin, os.Stdout); err != nil {
		os.Exit(2)
	}
}
