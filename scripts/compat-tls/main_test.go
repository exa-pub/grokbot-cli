package main

import (
	"crypto/tls"
	"encoding/json"
	"testing"
)

func TestEmptyHelloFieldsEncodeAsLists(t *testing.T) {
	rec := &recorder{}
	if _, err := rec.record(&tls.ClientHelloInfo{}); err != nil {
		t.Fatal(err)
	}
	h, err := rec.take()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"cipherSuites":[],"curves":[],"versions":[],"alpn":[],"signatures":[]}`
	if string(b) != want {
		t.Fatalf("got %s, want %s", b, want)
	}
}
