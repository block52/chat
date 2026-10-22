package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/cosmos/btcutil/bech32"
	"golang.org/x/crypto/ripemd160" //nolint:staticcheck
)

// signTestMessage mirrors what a wallet (Keplr signArbitrary / cosmjs) does, so
// the test exercises the exact round-trip the server verifies.
func signTestMessage(t *testing.T, hrp, text string) SignedMessage {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	pub := priv.PubKey().SerializeCompressed()

	sha := sha256.Sum256(pub)
	rip := ripemd160.New()
	_, _ = rip.Write(sha[:])
	addr, err := bech32.EncodeFromBase256(hrp, rip.Sum(nil))
	if err != nil {
		t.Fatalf("bech32: %v", err)
	}

	digest := adr36Digest(addr, text)
	sig := ecdsa.Sign(priv, digest)
	// Serialize to 64-byte compact r||s the way wallets transmit it.
	r, s := sigScalars(t, sig)

	return SignedMessage{
		Address:   addr,
		PubKey:    base64.StdEncoding.EncodeToString(pub),
		Signature: base64.StdEncoding.EncodeToString(append(r, s...)),
		Message:   text,
	}
}

func sigScalars(t *testing.T, sig *ecdsa.Signature) (r, s []byte) {
	t.Helper()
	// btcec's Signature.Serialize is DER; reconstruct compact via the modular
	// scalars by re-parsing. Easier: sign already gives low-S; extract via DER.
	der := sig.Serialize()
	// Minimal DER parse: 0x30 len 0x02 rlen r 0x02 slen s
	i := 2
	if der[i] != 0x02 {
		t.Fatalf("bad DER")
	}
	rl := int(der[i+1])
	rb := der[i+2 : i+2+rl]
	i = i + 2 + rl
	if der[i] != 0x02 {
		t.Fatalf("bad DER")
	}
	sl := int(der[i+1])
	sb := der[i+2 : i+2+sl]
	return leftPad(trimLeadingZero(rb)), leftPad(trimLeadingZero(sb))
}

func trimLeadingZero(b []byte) []byte {
	for len(b) > 1 && b[0] == 0 {
		b = b[1:]
	}
	return b
}

func leftPad(b []byte) []byte {
	if len(b) >= 32 {
		return b[len(b)-32:]
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func TestVerifyValidSignature(t *testing.T) {
	m := signTestMessage(t, "cosmos", "gg wp, nice table")
	hrp, err := Verify(m)
	if err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
	if hrp != "cosmos" {
		t.Fatalf("hrp = %q, want cosmos", hrp)
	}
}

func TestVerifyTamperedMessage(t *testing.T) {
	m := signTestMessage(t, "cosmos", "original")
	m.Message = "tampered"
	if _, err := Verify(m); err == nil {
		t.Fatal("expected failure for tampered message")
	}
}

func TestVerifyWrongAddress(t *testing.T) {
	m := signTestMessage(t, "cosmos", "hi")
	// Swap in a different valid address; pubkey no longer derives to it.
	other := signTestMessage(t, "cosmos", "hi")
	m.Address = other.Address
	if _, err := Verify(m); err == nil {
		t.Fatal("expected address mismatch")
	}
}

func TestVerifyNonDefaultPrefix(t *testing.T) {
	m := signTestMessage(t, "osmo", "hello osmosis")
	hrp, err := Verify(m)
	if err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
	if hrp != "osmo" {
		t.Fatalf("hrp = %q, want osmo", hrp)
	}
}
