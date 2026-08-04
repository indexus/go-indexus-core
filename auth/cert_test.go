package auth

import (
	"testing"
	"time"
)

func TestNodeCertSignAndVerify(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	issuer := NewIssuer("indexus-test", kp)
	cert, err := issuer.SignNodeCert("nodeA", PublicKeyBase64(kp.Public), "127.0.0.1", 21000, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	v := NewVerifier("indexus-test", kp.Public)
	if err := v.VerifyNodeCert(cert); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := v.VerifyNodeCertForAddr(cert, []string{"127.0.0.1"}); err != nil {
		t.Fatalf("verify addr: %v", err)
	}
	cert.IP = "10.0.0.1"
	// signature no longer matches payload — re-sign path would be needed; mutate after sign breaks sig
	bad := *cert
	bad.NodeID = "evil"
	if err := v.VerifyNodeCert(&bad); err == nil {
		t.Fatal("expected failure on tampered cert")
	}
}

func TestClientTokenScopeAndEncode(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	issuer := NewIssuer("indexus-test", kp)
	tok, err := issuer.SignClientToken("user-1", []string{"read", "write"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	v := NewVerifier("indexus-test", kp.Public)
	if err := v.VerifyClientToken(tok); err != nil {
		t.Fatal(err)
	}
	if !tok.HasScope("read") || !tok.HasScope("write") {
		t.Fatal("scopes")
	}
	enc, err := EncodeToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := DecodeToken(enc)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.VerifyClientToken(dec); err != nil {
		t.Fatal(err)
	}
}
