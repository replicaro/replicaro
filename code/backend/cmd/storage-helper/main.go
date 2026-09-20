// Command storage-helper performs one bounded, read-only filesystem storage
// identity observation over standard input and output.
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
