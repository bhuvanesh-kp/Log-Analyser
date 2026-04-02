//go:build !windows

package tailer

import "os"

// openFile opens path for reading. On non-Windows platforms os.Open is
// sufficient — file renaming does not require special sharing flags.
func openFile(path string) (*os.File, error) {
	return os.Open(path)
}
