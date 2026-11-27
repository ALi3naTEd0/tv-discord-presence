// Package discover finds TVs and boxes on the local network for the setup
// screen: LG webOS TVs answer SSDP for their second-screen service, Android
// TV / Google TV boxes answer DIAL and have network debugging (ADB) on port
// 5555. Nothing here pairs or changes anything; it only looks.
package discover

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Found is one device on the network.
type Found struct {
	Kind  string `json:"kind"` // webos | androidtv
	Name  string `json:"name"`
	Host  string `json:"host"`
	Model string `json:"model,omitempty"`
	// ADB is false for an Android TV that answered DIAL but has network
	// debugging off: setup explains how to turn it on.
	ADB bool `json:"adb"`
}

const (
	stWebOS = "urn:lge-com:service:webos-second-screen:1"
	stDIAL  = "urn:dial-multiscreen-org:service:dial:1"
)

// Scan looks for devices for about wait (3 s if 0).
func Scan(ctx context.Context, wait time.Duration) ([]Found, error) {
	if wait <= 0 {
		wait = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, wait+4*time.Second)
	defer cancel()
	byHost := map[string]*Found{}
	var mu sync.Mutex
	add := func(f Found) {
		mu.Lock()
		defer mu.Unlock()
		if old, ok := byHost[f.Host]; ok {
			// webOS wins the kind; a name and a model fill in when missing
			if f.Kind == "webos" {
				old.Kind = "webos"
			}
			if old.Name == "" || strings.HasPrefix(old.Name, "Android TV (") {
				old.Name = f.Name
			}
			if old.Model == "" {
				old.Model = f.Model
			}
			old.ADB = old.ADB || f.ADB
			return
		}
		byHost[f.Host] = &f
	}
	responses, err := ssdp(ctx, []string{stWebOS, stDIAL}, wait)
	if err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	for _, r := range responses {
		host := hostOf(r.location, r.from)
		if host == "" {
			continue
		}
		switch r.st {
		case stWebOS:
			add(Found{Kind: "webos", Host: host, Name: webOSName(r.headers)})
		case stDIAL:
			wg.Add(1)
			go func(loc, host string) {
				defer wg.Done()
				name, mfr, mdl := describe(ctx, loc)
				if isLG(mfr, mdl) {
					return // the LG's own DIAL answer: already found as webOS
				}
				add(Found{Kind: "androidtv", Host: host, Name: name, Model: strings.TrimSpace(mfr + " " + mdl), ADB: adbOpen(ctx, host)})
			}(r.location, host)
		}
	}
	// boxes that do not answer DIAL: anything with ADB open on the local networks
	for _, host := range scanPort(ctx, 5555) {
		add(Found{Kind: "androidtv", Host: host, Name: "Android TV (" + host + ")", ADB: true})
	}
	wg.Wait()
	out := make([]Found, 0, len(byHost))
	for _, f := range byHost {
		if f.Name == "" {
			f.Name = map[string]string{"webos": "LG TV", "androidtv": "Android TV"}[f.Kind]
		}
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind+out[i].Host > out[j].Kind+out[j].Host })
	return out, nil
}

type response struct {
	st, location string
	from         net.IP
	headers      textproto.MIMEHeader
}

func ssdp(ctx context.Context, sts []string, wait time.Duration) ([]response, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, fmt.Errorf("discover: %w", err)
	}
	defer conn.Close()
	dst := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	for range 2 { // UDP drops: ask twice
		for _, st := range sts {
			msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: " + st + "\r\n\r\n"
			conn.WriteToUDP([]byte(msg), dst)
		}
	}
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetReadDeadline(deadline)
	var out []response
	seen := map[string]bool{}
	buf := make([]byte, 4096)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		r, ok := parseResponse(buf[:n])
		if !ok {
			continue
		}
		r.from = from.IP
		key := r.st + "|" + r.location
		if !seen[key] {
			seen[key] = true
			out = append(out, r)
		}
	}
	return out, nil
}

func parseResponse(b []byte) (response, bool) {
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(b)))
	line, err := tp.ReadLine()
	if err != nil || !strings.Contains(line, "200") {
		return response{}, false
	}
	h, err := tp.ReadMIMEHeader()
	if err != nil && len(h) == 0 {
		return response{}, false
	}
	return response{st: h.Get("ST"), location: h.Get("Location"), headers: h}, true
}

func hostOf(location string, from net.IP) string {
	if u, err := url.Parse(location); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	if from != nil {
		return from.String()
	}
	return ""
}

// webOSName is the TV's own name ("[LG] webOS TV OLED65C3"), from the header LG adds.
func webOSName(h textproto.MIMEHeader) string {
	n := h.Get("DLNADeviceName.lge.com")
	if u, err := url.QueryUnescape(n); err == nil {
		n = u
	}
	n = strings.TrimSpace(strings.TrimPrefix(n, "[LG]"))
	if n == "" {
		return "LG TV"
	}
	return n
}

type description struct {
	Device struct {
		FriendlyName string `xml:"friendlyName"`
		Manufacturer string `xml:"manufacturer"`
		ModelName    string `xml:"modelName"`
	} `xml:"device"`
}

// describe reads a UPnP device description: friendly name, maker, model.
func describe(ctx context.Context, location string) (name, mfr, model string) {
	if location == "" {
		return "", "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return "", "", ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", ""
	}
	defer resp.Body.Close()
	var d description
	if xml.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&d) != nil {
		return "", "", ""
	}
	return strings.TrimSpace(d.Device.FriendlyName), strings.TrimSpace(d.Device.Manufacturer), strings.TrimSpace(d.Device.ModelName)
}

func isLG(mfr, model string) bool {
	s := strings.ToLower(mfr + " " + model)
	return strings.Contains(s, "lg electronics") || strings.Contains(s, "webos")
}

func adbOpen(ctx context.Context, host string) bool {
	d := net.Dialer{Timeout: 700 * time.Millisecond}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, "5555"))
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// scanPort tries port on every address of this computer's private IPv4
// networks (up to a /24 each), in parallel, briefly.
func scanPort(ctx context.Context, port int) []string {
	var hosts []string
	for _, n := range localNets() {
		base := n.IP.To4().Mask(n.Mask)
		for i := 1; i < 255; i++ {
			ip := net.IPv4(base[0], base[1], base[2], byte(i))
			if !ip.Equal(n.IP) {
				hosts = append(hosts, ip.String())
			}
		}
	}
	var mu sync.Mutex
	var open []string
	sem := make(chan struct{}, 96)
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func(h string) {
			defer func() { <-sem; wg.Done() }()
			d := net.Dialer{Timeout: 400 * time.Millisecond}
			c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(h, fmt.Sprint(port)))
			if err == nil {
				c.Close()
				mu.Lock()
				open = append(open, h)
				mu.Unlock()
			}
		}(h)
	}
	wg.Wait()
	return open
}

// localNets are this computer's private IPv4 networks (home LAN), not VPNs
// like Tailscale's 100.64/10 or container bridges.
func localNets() []*net.IPNet {
	var out []*net.IPNet
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		n := strings.ToLower(ifc.Name)
		if strings.HasPrefix(n, "docker") || strings.HasPrefix(n, "br-") || strings.HasPrefix(n, "veth") ||
			strings.HasPrefix(n, "tailscale") || strings.HasPrefix(n, "podman") || strings.HasPrefix(n, "virbr") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || !ipn.IP.IsPrivate() {
				continue
			}
			ones, _ := ipn.Mask.Size()
			if ones < 24 {
				ipn = &net.IPNet{IP: ipn.IP, Mask: net.CIDRMask(24, 32)}
			}
			out = append(out, ipn)
		}
	}
	return out
}
