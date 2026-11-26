package androidtv

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A small ADB client over TCP (the box's "network debugging", port 5555),
// in Go so Couchlog needs no adb binary. It does what tvpresence did with
// adb-shell: connect, authenticate with an RSA key (the box asks once to
// allow it) and run shell commands.

const (
	cmdCNXN = 0x4e584e43
	cmdAUTH = 0x48545541
	cmdOPEN = 0x4e45504f
	cmdOKAY = 0x59414b4f
	cmdCLSE = 0x45534c43
	cmdWRTE = 0x45545257
	cmdSTLS = 0x534c5453

	authToken     = 1
	authSignature = 2
	authPublicKey = 3

	adbVersion = 0x01000001
	maxData    = 256 * 1024
)

// ErrUnauthorized means the box did not accept the key: allow it on the TV
// ("Allow USB debugging?", tick "Always allow").
var ErrUnauthorized = errors.New("adb: key not authorized on the device")

type packet struct {
	cmd, arg0, arg1 uint32
	data            []byte
}

func writePacket(w io.Writer, p packet) error {
	var sum uint32
	for _, b := range p.data {
		sum += uint32(b)
	}
	h := make([]byte, 24, 24+len(p.data))
	binary.LittleEndian.PutUint32(h[0:], p.cmd)
	binary.LittleEndian.PutUint32(h[4:], p.arg0)
	binary.LittleEndian.PutUint32(h[8:], p.arg1)
	binary.LittleEndian.PutUint32(h[12:], uint32(len(p.data)))
	binary.LittleEndian.PutUint32(h[16:], sum)
	binary.LittleEndian.PutUint32(h[20:], p.cmd^0xffffffff)
	_, err := w.Write(append(h, p.data...))
	return err
}

func readPacket(r io.Reader) (packet, error) {
	h := make([]byte, 24)
	if _, err := io.ReadFull(r, h); err != nil {
		return packet{}, err
	}
	p := packet{
		cmd:  binary.LittleEndian.Uint32(h[0:]),
		arg0: binary.LittleEndian.Uint32(h[4:]),
		arg1: binary.LittleEndian.Uint32(h[8:]),
	}
	n := binary.LittleEndian.Uint32(h[12:])
	if binary.LittleEndian.Uint32(h[20:]) != p.cmd^0xffffffff {
		return packet{}, fmt.Errorf("adb: bad packet magic")
	}
	if n > 16*1024*1024 {
		return packet{}, fmt.Errorf("adb: packet of %d bytes", n)
	}
	p.data = make([]byte, n)
	if _, err := io.ReadFull(r, p.data); err != nil {
		return packet{}, err
	}
	return p, nil
}

// Key is the RSA key the box authorizes, with its public half in Android's
// own format (what adbkey.pub holds).
type Key struct {
	priv *rsa.PrivateKey
	pub  string
}

// LoadOrCreateKey reads an adb private key (PEM, PKCS#8 or PKCS#1) and its
// .pub file, creating both if missing. tvpresence's ~/.config/tvpresence/adbkey
// works as is, so the box does not ask again.
func LoadOrCreateKey(path string) (*Key, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return createKey(path)
	}
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("adb: %s is not a PEM key", path)
	}
	var priv *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
		var ok bool
		if priv, ok = k.(*rsa.PrivateKey); !ok {
			return nil, fmt.Errorf("adb: %s is not an RSA key", path)
		}
	} else if priv, err = x509.ParsePKCS1PrivateKey(blk.Bytes); err != nil {
		return nil, fmt.Errorf("adb: %s: %w", path, err)
	}
	k := &Key{priv: priv, pub: androidPublicKey(&priv.PublicKey) + " couchlog"}
	if pb, err := os.ReadFile(path + ".pub"); err == nil && len(bytes.TrimSpace(pb)) > 0 {
		k.pub = strings.TrimSpace(string(pb))
	}
	return k, nil
}

func createKey(path string) (*Key, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	k := &Key{priv: priv, pub: androidPublicKey(&priv.PublicKey) + " couchlog"}
	if err := os.WriteFile(path+".pub", []byte(k.pub+"\n"), 0o644); err != nil {
		return nil, err
	}
	return k, nil
}

// androidPublicKey encodes a 2048-bit key the way adbd expects: modulus size
// in words, n0inv, modulus and R² mod n little-endian, exponent; base64.
func androidPublicKey(pub *rsa.PublicKey) string {
	const words = 2048 / 32
	n := pub.N
	r32 := new(big.Int).Lsh(big.NewInt(1), 32)
	n0inv := new(big.Int).ModInverse(new(big.Int).Mod(n, r32), r32)
	n0inv.Sub(r32, n0inv)
	rr := new(big.Int).Exp(big.NewInt(2), big.NewInt(2*2048), n)

	buf := make([]byte, 4+4+256+256+4)
	binary.LittleEndian.PutUint32(buf[0:], words)
	binary.LittleEndian.PutUint32(buf[4:], uint32(n0inv.Uint64()))
	putLE(buf[8:264], n)
	putLE(buf[264:520], rr)
	binary.LittleEndian.PutUint32(buf[520:], uint32(pub.E))
	return base64.StdEncoding.EncodeToString(buf)
}

func putLE(dst []byte, v *big.Int) {
	be := v.FillBytes(make([]byte, len(dst)))
	for i := range be {
		dst[i] = be[len(be)-1-i]
	}
}

// adbConn is one authenticated connection; commands run one at a time.
type adbConn struct {
	mu      sync.Mutex
	c       net.Conn
	localID uint32
}

// dialADB connects and authenticates. A key the box has not seen yet shows
// the "Allow USB debugging?" prompt; authWait is how long to wait for it.
func dialADB(ctx context.Context, addr string, key *Key, authWait time.Duration) (*adbConn, error) {
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	c, err := d.DialContext(dctx, "tcp", addr)
	cancel()
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*adbConn, error) { c.Close(); return nil, err }
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := writePacket(c, packet{cmdCNXN, adbVersion, maxData, []byte("host::\x00")}); err != nil {
		return fail(err)
	}
	sentSig, sentPub := false, false
	for {
		p, err := readPacket(c)
		if err != nil {
			if sentPub {
				return fail(fmt.Errorf("%w (%v)", ErrUnauthorized, err))
			}
			return fail(err)
		}
		switch p.cmd {
		case cmdCNXN:
			c.SetDeadline(time.Time{})
			return &adbConn{c: c}, nil
		case cmdSTLS:
			return fail(errors.New("adb: device wants TLS (wireless debugging pairing); use classic network debugging on port 5555"))
		case cmdAUTH:
			if p.arg0 != authToken {
				return fail(fmt.Errorf("adb: unexpected auth type %d", p.arg0))
			}
			switch {
			case !sentSig:
				sig, err := rsa.SignPKCS1v15(nil, key.priv, crypto.SHA1, p.data)
				if err != nil {
					return fail(err)
				}
				sentSig = true
				err = writePacket(c, packet{cmdAUTH, authSignature, 0, sig})
				if err != nil {
					return fail(err)
				}
			case !sentPub:
				// Not known yet: send the public key and wait for the user.
				sentPub = true
				c.SetDeadline(time.Now().Add(authWait))
				if err := writePacket(c, packet{cmdAUTH, authPublicKey, 0, append([]byte(key.pub), 0)}); err != nil {
					return fail(err)
				}
			default:
				return fail(ErrUnauthorized)
			}
		}
	}
}

// Shell runs a command and returns its output.
func (a *adbConn) Shell(cmd string, timeout time.Duration) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.c.SetDeadline(time.Now().Add(timeout))
	defer a.c.SetDeadline(time.Time{})
	a.localID++
	local := a.localID
	if err := writePacket(a.c, packet{cmdOPEN, local, 0, []byte("shell:" + cmd + "\x00")}); err != nil {
		return "", err
	}
	var out bytes.Buffer
	var remote uint32
	for {
		p, err := readPacket(a.c)
		if err != nil {
			return out.String(), err
		}
		if p.arg1 != local {
			continue // a late packet for an earlier stream
		}
		switch p.cmd {
		case cmdOKAY:
			remote = p.arg0
		case cmdWRTE:
			remote = p.arg0
			out.Write(p.data)
			if err := writePacket(a.c, packet{cmdOKAY, local, remote, nil}); err != nil {
				return out.String(), err
			}
		case cmdCLSE:
			writePacket(a.c, packet{cmdCLSE, local, p.arg0, nil})
			return out.String(), nil
		}
	}
}

func (a *adbConn) Close() error { return a.c.Close() }
