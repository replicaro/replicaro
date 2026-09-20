// targetverify admits the exact selected native inputs for one public build.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/local/replicaro/componentmanifest"
)

func main() {
	target := flag.String("target", "", "exact supported product target")
	flag.Parse()
	if *target == "" || flag.NArg() != 0 {
		fatal(fmt.Errorf("usage: targetverify --target <exact-target>"))
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		fatal(fmt.Errorf("cannot locate managed backend source tree"))
	}
	backendRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	if err := componentmanifest.VerifySourceTree(backendRoot, *target); err != nil {
		fatal(err)
	}
	fmt.Printf("Selected native component inputs verified for %s.\n", *target)
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "targetverify:", err); os.Exit(1) }
