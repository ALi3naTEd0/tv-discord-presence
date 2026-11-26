// Package presence shows what you are watching as your Discord status
// ("Watching Netflix — Ozark, S1 · E2"). Discord only accepts this through
// its local connection (IPC: a socket, or a named pipe on Windows), so it
// runs on the computer where Discord is.
package presence

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

const (
	opHandshake = 0
	opFrame     = 1
	opClose     = 2
)

// ErrNoDiscord means Discord is not running on this computer.
var ErrNoDiscord = errors.New("presence: Discord is not running here")

// Conn is an open connection to the Discord client.
type Conn struct {
	mu sync.Mutex
	rw io.ReadWriteCloser
}

// Dial connects to the first Discord found and identifies as clientID (the
// Discord application whose name shows: "Watching …").
func Dial(clientID string) (*Conn, error) {
	var last error = ErrNoDiscord
	for _, p := range ipcPaths() {
		rw, err := openIPC(p)
		if err != nil {
			last = err
			continue
		}
		c := &Conn{rw: rw}
		if err := c.write(opHandshake, map[string]any{"v": 1, "client_id": clientID}); err != nil {
			rw.Close()
			last = err
			continue
		}
		var ready struct {
			Evt     string `json:"evt"`
			Message string `json:"message"` // on a close frame
			Data    struct {
				Message string `json:"message"`
			} `json:"data"`
		}
		if op, err := c.read(&ready); err != nil || op == opClose || ready.Evt != "READY" {
			rw.Close()
			if err == nil {
				msg := ready.Message
				if msg == "" {
					msg = ready.Data.Message
				}
				err = fmt.Errorf("presence: Discord refused the application id (%s)", msg)
			}
			return nil, err
		}
		return c, nil
	}
	return nil, last
}

func (c *Conn) write(op uint32, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	h := make([]byte, 8, 8+len(b))
	binary.LittleEndian.PutUint32(h[0:], op)
	binary.LittleEndian.PutUint32(h[4:], uint32(len(b)))
	_, err = c.rw.Write(append(h, b...))
	return err
}

func (c *Conn) read(v any) (uint32, error) {
	h := make([]byte, 8)
	if _, err := io.ReadFull(c.rw, h); err != nil {
		return 0, err
	}
	op, n := binary.LittleEndian.Uint32(h[0:]), binary.LittleEndian.Uint32(h[4:])
	if n > 1<<20 {
		return 0, fmt.Errorf("presence: frame of %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c.rw, b); err != nil {
		return 0, err
	}
	if v != nil {
		json.Unmarshal(b, v)
	}
	return op, nil
}

func nonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// SetActivity shows a, or clears the status when a is nil.
func (c *Conn) SetActivity(a *Activity) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	args := map[string]any{"pid": os.Getpid()}
	if a != nil {
		args["activity"] = a
	}
	if err := c.write(opFrame, map[string]any{"cmd": "SET_ACTIVITY", "args": args, "nonce": nonce()}); err != nil {
		return err
	}
	var res struct {
		Evt  string `json:"evt"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	op, err := c.read(&res)
	if err != nil {
		return err
	}
	if op == opClose || res.Evt == "ERROR" {
		return fmt.Errorf("presence: Discord said %q", res.Data.Message)
	}
	return nil
}

// Close ends the connection; Discord clears the status.
func (c *Conn) Close() error { return c.rw.Close() }
