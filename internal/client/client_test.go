package client

import (
	"testing"
	"time"
)

func TestChecksumJSVector(t *testing.T) {
	got := Checksum("00000000-0000-4000-8000-000000000000", time.UnixMilli(1761350400000))
	want := "RQQGHwNK00000000-0000-4000-8000-000000000000"
	if got != want {
		t.Fatalf("%s != %s", got, want)
	}
}
