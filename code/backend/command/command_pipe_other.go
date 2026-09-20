//go:build !linux

package command

import "os"

type commandOutputReader struct{ *os.File }

func newCommandOutputReader(pipe *os.File) *commandOutputReader { return &commandOutputReader{pipe} }
func (reader *commandOutputReader) interrupt()                  { _ = reader.Close() }
