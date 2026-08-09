package domain

import "testing"

func TestSetsEnvelopeRoundTrip(t *testing.T) {
	body := []byte{0x01, 0x02, 0x03}
	redirects := []SetsRedirect{
		{Location: "qu", Name: "peerAAAA", IP: "127.0.0.1", Port: 21010},
		{Location: "7v", Name: "peerBBBB", IP: "10.0.0.2", Port: 21011},
	}
	framed := EncodeSetsEnvelope(body, redirects)
	if !IsSetsEnvelope(framed) {
		t.Fatal("framed payload missing magic")
	}
	got, inner, ok, err := DecodeSetsEnvelope(framed)
	if err != nil || !ok {
		t.Fatalf("decode: ok=%v err=%v", ok, err)
	}
	if string(inner) != string(body) {
		t.Fatalf("body=%v want=%v", inner, body)
	}
	if len(got) != 2 || got[0].Location != "qu" || got[1].Port != 21011 {
		t.Fatalf("redirects=%+v", got)
	}
}

func TestSetsEnvelopeLegacyRaw(t *testing.T) {
	raw := []byte{0, 0, 0, 1, 9} // not IXS1
	_, body, ok, err := DecodeSetsEnvelope(raw)
	if err != nil || ok {
		t.Fatalf("legacy should not parse as envelope: ok=%v err=%v", ok, err)
	}
	if string(body) != string(raw) {
		t.Fatal("legacy body should pass through unchanged")
	}
}
