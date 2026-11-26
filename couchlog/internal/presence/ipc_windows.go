//go:build windows

package presence

import (
	"io"
	"os"
)

func ipcPaths() []string {
	var out []string
	for i := 0; i < 10; i++ {
		out = append(out, `\\.\pipe\discord-ipc-`+string(rune('0'+i)))
	}
	return out
}

// openIPC opens Discord's named pipe, which Windows lets you use as a file.
func openIPC(path string) (io.ReadWriteCloser, error) {
	return os.OpenFile(path, os.O_RDWR, 0)
}
