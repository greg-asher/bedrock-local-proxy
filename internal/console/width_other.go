//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly)

package console

import "os"

// queryTerminalWidth has no portable implementation here, so the console uses
// its default width unless COLUMNS says otherwise.
func queryTerminalWidth(*os.File) int { return 0 }
