package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPassportURL(t *testing.T) {
	if got := passportURL("/cgi-bin/genimage?x=1"); got != passportBase()+"/cgi-bin/genimage?x=1" {
		t.Fatalf("a path must get the base prepended, got %s", got)
	}
	// Regression: upstream hands back captcha_url as an absolute URL. Prepending
	// the base again built "https://passport.baidu.comhttps://…" and every
	// captcha fetch failed, which the page showed as a broken image.
	abs := passportBase() + "/cgi-bin/genimage?x=1"
	if got := passportURL(abs); got != abs {
		t.Fatalf("an absolute URL must pass through unchanged, got %s", got)
	}
}

func TestCaptchaImage(t *testing.T) {
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html>vcodestr expired</html>"))
	}))
	defer html.Close()
	// An HTML body must be refused: inlining it as image/jpeg is exactly what
	// renders as a broken image in the page.
	if _, _, err := captchaImage(context.Background(), newPassportJar(), html.URL+"/cgi-bin/genimage?x=1"); err == nil {
		t.Fatal("a non-image body must be rejected")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
	}))
	defer empty.Close()
	if _, _, err := captchaImage(context.Background(), newPassportJar(), empty.URL+"/x"); err == nil {
		t.Fatal("an empty body must be rejected")
	}

	png := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png; charset=binary")
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
	}))
	defer png.Close()
	buf, ctype, err := captchaImage(context.Background(), newPassportJar(), png.URL+"/x")
	if err != nil {
		t.Fatalf("a real image must pass: %v", err)
	}
	// The response's own type wins, and the charset parameter is stripped.
	if ctype != "image/png" || len(buf) != 4 {
		t.Fatalf("want image/png and 4 bytes, got %q and %d", ctype, len(buf))
	}
	if _, _, err := captchaImage(context.Background(), newPassportJar(), ""); err == nil {
		t.Fatal("a missing captcha URL must be reported, not fetched")
	}
}
