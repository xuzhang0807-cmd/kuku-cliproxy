package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseQrChannel(t *testing.T) {
	// The confirmed payload: channel_v is a *stringified* object, not an object.
	raw := `{"errno":0,"channel_v":"{\"status\":\"0\",\"v\":\"ticket-32\",\"u\":\"https://kuku.baidu.com\"}"}`
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatal(err)
	}
	got := parseQrChannel(parsed)
	if got.State != "confirmed" || got.BDUSS != "ticket-32" || got.Target != "https://kuku.baidu.com" {
		t.Fatalf("bad confirmed payload: %+v", got)
	}

	cases := []struct {
		body string
		want string
	}{
		{`{"errno":0,"channel_v":{"status":"1"}}`, "scanned"},
		{`{"errno":0,"channel_v":{"status":"2"}}`, "expired"},
		{`{"errno":0,"channel_v":{"status":"0"}}`, "pending"}, // confirmed but no ticket
		{`{"errno":1}`, "pending"},
		{`{"errno":0,"channel_v":"not json"}`, "pending"},
	}
	for _, c := range cases {
		var p map[string]any
		if err := json.Unmarshal([]byte(c.body), &p); err != nil {
			t.Fatal(err)
		}
		if got := parseQrChannel(p); got.State != c.want {
			t.Fatalf("%s: want %s, got %+v", c.body, c.want, got)
		}
	}
}

func TestUnwrapJSONP(t *testing.T) {
	obj := unwrapJSONP(`bd__cbs__abc({"errno":0,"data":{"token":"deadbeef"}})`)
	if str(asObject(obj["data"])["token"]) != "deadbeef" {
		t.Fatalf("jsonp not unwrapped: %+v", obj)
	}
	// passport sometimes answers with JS, not JSON: single-quoted keys.
	js := unwrapJSONP(`loginrecord:{ 'email':[], 'phone':[] }`)
	if js == nil {
		t.Fatal("single-quoted JS payload must still parse")
	}
	if _, ok := js["email"]; !ok {
		t.Fatalf("single-quoted keys not normalised: %+v", js)
	}
	if unwrapJSONP("not json at all") != nil {
		t.Fatal("garbage must not parse")
	}
}

func TestParseLoginReply(t *testing.T) {
	// Form submissions answer with an HTML redirect script, not JSONP.
	html := `<script>location.href += "?err_no=0&codeString=abc&isslide=0";</script>`
	parsed := parseLoginReply(nil, html)
	if parsed == nil {
		t.Fatal("redirect script must be parsed")
	}
	no := str(asObject(parsed["errInfo"])["no"])
	if no != "0" {
		t.Fatalf("err_no not read: %+v", parsed)
	}
	if str(asObject(parsed["data"])["codeString"]) != "abc" {
		t.Fatalf("codeString not read: %+v", parsed)
	}
	if parseLoginReply(nil, "<html>404</html>") != nil {
		t.Fatal("a page with no err_no must not be read as a reply")
	}
}

func TestMoonshad(t *testing.T) {
	phone := "13800138000"
	got := moonshad(phone)
	if got != moonshad(phone) {
		t.Fatal("must be deterministic")
	}
	// Each doubling is a single (non-global) replace, and only the hex letters
	// a/d can ever match — so the length is exactly predictable: +1 per rule that
	// fires, at most once each. A global replace, or a stray o/h/s/n/m rule
	// firing, changes this number.
	sum := md5.Sum([]byte(phone + "Moonshadow"))
	base := hex.EncodeToString(sum[:])
	want := 32
	if strings.Contains(base, "a") {
		want++
	}
	if strings.Contains(base, "d") {
		want++
	}
	if len(got) != want {
		t.Fatalf("want length %d, got %d (%q)", want, len(got), got)
	}
	if moonshad("13800138001") == got {
		t.Fatal("different phones must produce different values")
	}
}

func TestPhoneRe(t *testing.T) {
	for _, ok := range []string{"13800138000", "19912345678", "15000000000"} {
		if !phoneRe.MatchString(ok) {
			t.Fatalf("%s must be accepted", ok)
		}
	}
	for _, bad := range []string{"", "1380013800", "138001380000", "12800138000", "23800138000", "1380013800a"} {
		if phoneRe.MatchString(bad) {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

func TestDataURL(t *testing.T) {
	if dataURL("image/png", nil) != "" {
		t.Fatal("empty bytes must produce no data URL")
	}
	if got := dataURL("image/png", []byte{1, 2, 3}); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("bad data URL: %s", got)
	}
}

func TestLoginSessionLifecycle(t *testing.T) {
	s := &loginSession{ID: "ls-1", Kind: "sms", State: "sent", Sends: 1, Expires: time.Now().Add(time.Minute)}
	loginMu.Lock()
	loginSessions["ls-1"] = s
	loginMu.Unlock()
	defer func() { loginMu.Lock(); delete(loginSessions, "ls-1"); loginMu.Unlock() }()

	if got := getLoginSession("ls-1"); got != s {
		t.Fatal("session must be found while live")
	}
	if getLoginSession("nope") != nil {
		t.Fatal("unknown id must return nil")
	}
	// An expired session is dropped, not handed out.
	loginMu.Lock()
	s.Expires = time.Now().Add(-time.Minute)
	loginMu.Unlock()
	if getLoginSession("ls-1") != nil {
		t.Fatal("expired session must not be returned")
	}
}
