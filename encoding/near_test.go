package encoding

import (
	"testing"
)

func TestRandomNameNearKeepsPrefixBits(t *testing.T) {
	target, err := BASE64.Decode("AAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		// generate a valid name then decode
		n, err := BASE64.RandomName()
		if err != nil {
			t.Fatal(err)
		}
		target, err = BASE64.Decode(n)
		if err != nil {
			t.Fatal(err)
		}
	}

	keep := 16
	name, err := BASE64.RandomNameNear(target, keep)
	if err != nil {
		t.Fatal(err)
	}
	got, err := BASE64.Decode(name)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < keep/8; i++ {
		if got[i] != target[i] {
			t.Fatalf("byte %d: got %02x want %02x", i, got[i], target[i])
		}
	}
}
