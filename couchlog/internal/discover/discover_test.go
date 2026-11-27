package discover

import (
	"net/textproto"
	"testing"
)

func TestParseWebOSAnswer(t *testing.T) {
	b := []byte("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=1800\r\nLOCATION: http://192.168.1.220:1528/\r\n" +
		"ST: urn:lge-com:service:webos-second-screen:1\r\nDLNADeviceName.lge.com: %5BLG%5D%20webOS%20TV%20OLED65C3\r\n\r\n")
	r, ok := parseResponse(b)
	if !ok || r.st != stWebOS || hostOf(r.location, nil) != "192.168.1.220" {
		t.Fatalf("%+v %v", r, ok)
	}
	if n := webOSName(r.headers); n != "webOS TV OLED65C3" {
		t.Errorf("name %q", n)
	}
	if n := webOSName(textproto.MIMEHeader{}); n != "LG TV" {
		t.Errorf("default %q", n)
	}
	if _, ok := parseResponse([]byte("NOTIFY * HTTP/1.1\r\n\r\n")); ok {
		t.Error("a NOTIFY is not an answer")
	}
}
