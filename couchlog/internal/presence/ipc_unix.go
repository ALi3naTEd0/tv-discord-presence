//go:build !windows

package presence

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ipcPaths are where Discord (native, Flatpak or Snap) listens.
func ipcPaths() []string {
	var dirs []string
	for _, env := range []string{"XDG_RUNTIME_DIR", "TMPDIR", "TMP", "TEMP"} {
		if d := os.Getenv(env); d != "" {
			dirs = append(dirs, d)
		}
	}
	dirs = append(dirs, "/tmp")
	var out []string
	for _, d := range dirs {
		for _, sub := range []string{"", "app/com.discordapp.Discord", "snap.discord", "app/dev.vencord.Vesktop"} {
			for i := 0; i < 10; i++ {
				out = append(out, filepath.Join(d, sub, "discord-ipc-"+string(rune('0'+i))))
			}
		}
	}
	return out
}

func openIPC(path string) (io.ReadWriteCloser, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return net.DialTimeout("unix", path, 3*time.Second)
}
