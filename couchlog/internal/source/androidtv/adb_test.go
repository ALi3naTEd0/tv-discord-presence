package androidtv

import (
	"context"
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeADBD is the device side: it challenges, checks the signature against
// the keys it trusts (or learns a key sent with AUTH type 3 when accept is
// set, like tapping "Allow"), then runs "shell:" commands from a table.
type fakeADBD struct {
	trusted  []*rsa.PublicKey
	accept   bool
	shell    map[string]string
	addr     string
	gotPubKy string
}

func (f *fakeADBD) start(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f.addr = ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
}

func (f *fakeADBD) serve(c net.Conn) {
	defer c.Close()
	token := []byte("0123456789abcdefghij") // 20 bytes, like a SHA-1
	authed := false
	for {
		p, err := readPacket(c)
		if err != nil {
			return
		}
		switch p.cmd {
		case cmdCNXN:
			writePacket(c, packet{cmdAUTH, authToken, 0, token})
		case cmdAUTH:
			switch p.arg0 {
			case authSignature:
				for _, k := range f.trusted {
					if rsa.VerifyPKCS1v15(k, crypto.SHA1, token, p.data) == nil {
						authed = true
					}
				}
				if authed {
					writePacket(c, packet{cmdCNXN, adbVersion, maxData, []byte("device::ro.product.name=fake;\x00")})
				} else {
					writePacket(c, packet{cmdAUTH, authToken, 0, token})
				}
			case authPublicKey:
				f.gotPubKy = strings.TrimRight(string(p.data), "\x00")
				if f.accept {
					writePacket(c, packet{cmdCNXN, adbVersion, maxData, []byte("device::\x00")})
					authed = true
				} else {
					return // the user never answered: the device drops it
				}
			}
		case cmdOPEN:
			if !authed {
				return
			}
			cmd := strings.TrimSuffix(strings.TrimPrefix(string(p.data), "shell:"), "\x00")
			const remote = 99
			writePacket(c, packet{cmdOKAY, remote, p.arg0, nil})
			out := f.shell[cmd]
			// send in small chunks, each acknowledged
			for len(out) > 0 {
				n := min(7, len(out))
				writePacket(c, packet{cmdWRTE, remote, p.arg0, []byte(out[:n])})
				out = out[n:]
				if ack, err := readPacket(c); err != nil || ack.cmd != cmdOKAY {
					return
				}
			}
			writePacket(c, packet{cmdCLSE, remote, p.arg0, nil})
			readPacket(c) // our CLSE
		}
	}
}

func TestADBKnownKeyRunsShell(t *testing.T) {
	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "adbkey"))
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeADBD{trusted: []*rsa.PublicKey{&key.priv.PublicKey}, shell: map[string]string{"echo hi": "hi there, a longer output\n"}}
	d.start(t)
	c, err := dialADB(context.Background(), d.addr, key, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 2; i++ { // two commands on one connection
		out, err := c.Shell("echo hi", 2*time.Second)
		if err != nil || out != "hi there, a longer output\n" {
			t.Fatalf("shell: %q %v", out, err)
		}
	}
	if d.gotPubKy != "" {
		t.Error("sent the public key although the signature was accepted")
	}
}

func TestADBNewKeyAskedAndAllowed(t *testing.T) {
	key, _ := LoadOrCreateKey(filepath.Join(t.TempDir(), "adbkey"))
	d := &fakeADBD{accept: true, shell: map[string]string{}}
	d.start(t)
	c, err := dialADB(context.Background(), d.addr, key, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if !strings.HasSuffix(d.gotPubKy, " couchlog") {
		t.Errorf("public key sent %q", d.gotPubKy)
	}
}

func TestADBNewKeyNotAllowed(t *testing.T) {
	key, _ := LoadOrCreateKey(filepath.Join(t.TempDir(), "adbkey"))
	d := &fakeADBD{shell: map[string]string{}}
	d.start(t)
	_, err := dialADB(context.Background(), d.addr, key, 200*time.Millisecond)
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err %v, want ErrUnauthorized", err)
	}
}

// The Android encoding must carry the modulus and a correct n0inv, or adbd
// rejects the key without saying why.
func TestAndroidPublicKeyEncoding(t *testing.T) {
	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "adbkey"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := base64.StdEncoding.DecodeString(strings.Fields(key.pub)[0])
	if err != nil || len(b) != 524 {
		t.Fatalf("decoded %d bytes, err %v", len(b), err)
	}
	if binary.LittleEndian.Uint32(b[0:]) != 64 {
		t.Error("modulus size is not 64 words")
	}
	le := b[8:264]
	be := make([]byte, 256)
	for i := range le {
		be[i] = le[255-i]
	}
	n := new(big.Int).SetBytes(be)
	if n.Cmp(key.priv.N) != 0 {
		t.Error("modulus does not round-trip")
	}
	n0inv := binary.LittleEndian.Uint32(b[4:])
	if uint32(key.priv.N.Uint64())*n0inv != 0xffffffff { // n0inv * n ≡ -1 (mod 2^32)
		t.Errorf("n0inv %#x is wrong", n0inv)
	}
	if binary.LittleEndian.Uint32(b[520:]) != uint32(key.priv.E) {
		t.Error("exponent")
	}
}

func TestLoadKeyKeepsExistingPub(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "adbkey")
	k1, _ := LoadOrCreateKey(p)
	os.WriteFile(p+".pub", []byte("QUFBQQ== someone@titan\n"), 0o644) // what tvpresence wrote
	k2, err := LoadOrCreateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if k2.pub != "QUFBQQ== someone@titan" || k2.priv.N.Cmp(k1.priv.N) != 0 {
		t.Errorf("reloaded key pub %q", k2.pub)
	}
}
