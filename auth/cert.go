package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// NodeCert binds a node identity to a reachable address, signed by the
// network issuer (Ed25519 locally, KMS-compatible payload on AWS).
type NodeCert struct {
	NetworkID string `json:"network_id"`
	NodeID    string `json:"node_id"`
	PubKey    string `json:"pub_key"` // base64 ed25519 public key of the node
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	IssuedAt  int64  `json:"issued_at"`
	ExpiresAt int64  `json:"expires_at"`
	Signature string `json:"signature"` // base64 signature over canonical payload
}

type nodeCertPayload struct {
	NetworkID string `json:"network_id"`
	NodeID    string `json:"node_id"`
	PubKey    string `json:"pub_key"`
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	IssuedAt  int64  `json:"issued_at"`
	ExpiresAt int64  `json:"expires_at"`
}

func (c *NodeCert) payload() ([]byte, error) {
	return json.Marshal(nodeCertPayload{
		NetworkID: c.NetworkID,
		NodeID:    c.NodeID,
		PubKey:    c.PubKey,
		IP:        c.IP,
		Port:      c.Port,
		IssuedAt:  c.IssuedAt,
		ExpiresAt: c.ExpiresAt,
	})
}

// ClientToken authorizes a client to consume (and optionally write) data.
type ClientToken struct {
	NetworkID string   `json:"network_id"`
	ClientID  string   `json:"client_id"`
	Scopes    []string `json:"scopes"`
	IssuedAt  int64    `json:"issued_at"`
	ExpiresAt int64    `json:"expires_at"`
	Signature string   `json:"signature"`
}

type clientTokenPayload struct {
	NetworkID string   `json:"network_id"`
	ClientID  string   `json:"client_id"`
	Scopes    []string `json:"scopes"`
	IssuedAt  int64    `json:"issued_at"`
	ExpiresAt int64    `json:"expires_at"`
}

func (t *ClientToken) payload() ([]byte, error) {
	return json.Marshal(clientTokenPayload{
		NetworkID: t.NetworkID,
		ClientID:  t.ClientID,
		Scopes:    t.Scopes,
		IssuedAt:  t.IssuedAt,
		ExpiresAt: t.ExpiresAt,
	})
}

func (t *ClientToken) HasScope(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope || s == "*" {
			return true
		}
	}
	return false
}

// EncodeToken serializes a client token as a URL-safe bearer string.
func EncodeToken(t *ClientToken) (string, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func DecodeToken(s string) (*ClientToken, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var t ClientToken
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// KeyPair is an Ed25519 issuer or node keypair.
type KeyPair struct {
	Public  ed25519.PublicKey
	Private ed25519.PrivateKey
}

func GenerateKeyPair() (*KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &KeyPair{Public: pub, Private: priv}, nil
}

func LoadOrCreateKeyPair(path string) (*KeyPair, error) {
	if path == "" {
		return GenerateKeyPair()
	}
	if b, err := os.ReadFile(path); err == nil {
		if len(b) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("invalid key size in %s", path)
		}
		priv := ed25519.PrivateKey(b)
		return &KeyPair{Public: priv.Public().(ed25519.PublicKey), Private: priv}, nil
	}
	kp, err := GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(parentDir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, kp.Private, 0600); err != nil {
		return nil, err
	}
	return kp, nil
}

func parentDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			return path[:i]
		}
	}
	return "."
}

func PublicKeyBase64(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

func ParsePublicKeyBase64(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, errors.New("invalid ed25519 public key size")
	}
	return ed25519.PublicKey(b), nil
}

type Issuer struct {
	NetworkID string
	Key       *KeyPair
}

func NewIssuer(networkID string, key *KeyPair) *Issuer {
	return &Issuer{NetworkID: networkID, Key: key}
}

func (i *Issuer) SignNodeCert(nodeID, nodePubKey, ip string, port int, ttl time.Duration) (*NodeCert, error) {
	now := time.Now().UTC()
	cert := &NodeCert{
		NetworkID: i.NetworkID,
		NodeID:    nodeID,
		PubKey:    nodePubKey,
		IP:        ip,
		Port:      port,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(ttl).Unix(),
	}
	payload, err := cert.payload()
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(i.Key.Private, payload)
	cert.Signature = base64.StdEncoding.EncodeToString(sig)
	return cert, nil
}

func (i *Issuer) SignClientToken(clientID string, scopes []string, ttl time.Duration) (*ClientToken, error) {
	now := time.Now().UTC()
	tok := &ClientToken{
		NetworkID: i.NetworkID,
		ClientID:  clientID,
		Scopes:    scopes,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(ttl).Unix(),
	}
	payload, err := tok.payload()
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(i.Key.Private, payload)
	tok.Signature = base64.StdEncoding.EncodeToString(sig)
	return tok, nil
}

type Verifier struct {
	NetworkID string
	Public    ed25519.PublicKey
}

func NewVerifier(networkID string, pub ed25519.PublicKey) *Verifier {
	return &Verifier{NetworkID: networkID, Public: pub}
}

func (v *Verifier) VerifyNodeCert(cert *NodeCert) error {
	if cert == nil {
		return errors.New("missing node cert")
	}
	if cert.NetworkID != v.NetworkID {
		return fmt.Errorf("network mismatch: got %s want %s", cert.NetworkID, v.NetworkID)
	}
	if time.Now().Unix() > cert.ExpiresAt {
		return errors.New("node cert expired")
	}
	payload, err := cert.payload()
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(cert.Signature)
	if err != nil {
		return err
	}
	if !ed25519.Verify(v.Public, payload, sig) {
		return errors.New("invalid node cert signature")
	}
	return nil
}

// VerifyNodeCertForAddr checks the signature and that the cert carries an IP.
// When an observed source IP matches cert.IP (or both are loopback), that is
// preferred. On AWS VPC hairpin/NAT the TCP source may differ from the
// public advertise address, so a cryptographically valid cert with a
// non-empty IP is still accepted — reachability is confirmed on dial-back
// to cert.IP.
func (v *Verifier) VerifyNodeCertForAddr(cert *NodeCert, observedIPs []string) error {
	if err := v.VerifyNodeCert(cert); err != nil {
		return err
	}
	if cert.IP == "" {
		return errors.New("cert missing ip")
	}
	for _, ip := range observedIPs {
		if ip == cert.IP {
			return nil
		}
	}
	if isLoopback(cert.IP) {
		for _, ip := range observedIPs {
			if isLoopback(ip) {
				return nil
			}
		}
	}
	return nil
}

func (v *Verifier) VerifyClientToken(tok *ClientToken) error {
	if tok == nil {
		return errors.New("missing client token")
	}
	if tok.NetworkID != v.NetworkID {
		return fmt.Errorf("network mismatch: got %s want %s", tok.NetworkID, v.NetworkID)
	}
	if time.Now().Unix() > tok.ExpiresAt {
		return errors.New("client token expired")
	}
	payload, err := tok.payload()
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(tok.Signature)
	if err != nil {
		return err
	}
	if !ed25519.Verify(v.Public, payload, sig) {
		return errors.New("invalid client token signature")
	}
	return nil
}

func isLoopback(ip string) bool {
	return ip == "127.0.0.1" || ip == "::1" || ip == "localhost"
}
