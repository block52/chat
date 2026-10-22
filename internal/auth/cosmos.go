// Package auth verifies Cosmos (ADR-36) signatures for chat messages.
//
// A client proves ownership of a bech32 Cosmos address by signing the exact
// message bytes with the secp256k1 private key behind that address. The server
// re-derives the address from the supplied public key and checks that:
//
//  1. the pubkey hashes to the claimed address, and
//  2. the signature is valid over the signed payload.
//
// Signing follows ADR-36 "sign arbitrary data": the raw message is wrapped in a
// StdSignDoc with empty chain-id, account number 0 and sequence 0, the single
// message being an amino "sign/MsgSignData" carrying the base64 data. The signer
// signs the SHA-256 of that canonical JSON. This matches what Keplr's
// signArbitrary and cosmjs verifyADR36Amino produce.
package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/cosmos/btcutil/bech32"
	"golang.org/x/crypto/ripemd160" //nolint:staticcheck // Cosmos addresses are defined as ripemd160(sha256(pubkey)).
)

// ErrAddressMismatch means the public key does not derive to the claimed address.
var ErrAddressMismatch = errors.New("auth: public key does not match address")

// ErrBadSignature means the signature failed verification over the signed payload.
var ErrBadSignature = errors.New("auth: signature verification failed")

// SignedMessage is what a client sends to prove authorship of a chat message.
type SignedMessage struct {
	// Address is the signer's bech32 Cosmos address (e.g. "cosmos1...").
	Address string `json:"address"`
	// PubKey is the standard-base64 compressed (33-byte) secp256k1 public key.
	PubKey string `json:"pubKey"`
	// Signature is the standard-base64 64-byte compact secp256k1 signature (r||s).
	Signature string `json:"signature"`
	// Message is the human-readable payload that was signed (the chat text,
	// JSON-encoded envelope, nonce, etc. — opaque to the verifier).
	Message string `json:"message"`
}

// Verify checks that m.Signature is a valid ADR-36 signature of m.Message by the
// key behind m.Address. It returns the bech32 HRP (prefix) on success so callers
// can enforce an expected chain prefix.
func Verify(m SignedMessage) (hrp string, err error) {
	pubBytes, err := base64.StdEncoding.DecodeString(m.PubKey)
	if err != nil {
		return "", fmt.Errorf("auth: decode pubkey: %w", err)
	}
	pub, err := btcec.ParsePubKey(pubBytes)
	if err != nil {
		return "", fmt.Errorf("auth: parse pubkey: %w", err)
	}

	// 1. Address derivation: addr = ripemd160(sha256(compressed pubkey)).
	hrp, derived, err := deriveAddress(pub.SerializeCompressed(), m.Address)
	if err != nil {
		return "", err
	}
	if derived != m.Address {
		return "", ErrAddressMismatch
	}

	// 2. Signature check over the ADR-36 sign doc.
	digest := adr36Digest(m.Address, m.Message)
	sigBytes, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		return "", fmt.Errorf("auth: decode signature: %w", err)
	}
	if len(sigBytes) != 64 {
		return "", fmt.Errorf("auth: signature must be 64 bytes, got %d", len(sigBytes))
	}
	var r, s btcec.ModNScalar
	if overflow := r.SetByteSlice(sigBytes[:32]); overflow {
		return "", ErrBadSignature
	}
	if overflow := s.SetByteSlice(sigBytes[32:]); overflow {
		return "", ErrBadSignature
	}
	sig := ecdsa.NewSignature(&r, &s)
	if !sig.Verify(digest, pub) {
		return "", ErrBadSignature
	}
	return hrp, nil
}

// deriveAddress returns the bech32 encoding of ripemd160(sha256(pubkey)) using
// the same HRP (prefix) as the claimed address, so verification is independent
// of chain prefix (cosmos, osmo, etc.).
func deriveAddress(compressedPub []byte, claimed string) (hrp, addr string, err error) {
	hrp, _, err = bech32.DecodeToBase256(claimed)
	if err != nil {
		return "", "", fmt.Errorf("auth: decode bech32 address: %w", err)
	}
	sha := sha256.Sum256(compressedPub)
	rip := ripemd160.New()
	if _, err := rip.Write(sha[:]); err != nil {
		return "", "", fmt.Errorf("auth: ripemd160: %w", err)
	}
	addrBytes := rip.Sum(nil)
	encoded, err := bech32.EncodeFromBase256(hrp, addrBytes)
	if err != nil {
		return "", "", fmt.Errorf("auth: encode bech32: %w", err)
	}
	return hrp, encoded, nil
}

// adr36Digest builds the ADR-36 amino StdSignDoc for arbitrary data and returns
// its SHA-256, which is what the signer actually signs.
func adr36Digest(signer, data string) []byte {
	doc := stdSignDoc(signer, data)
	return sha256Bytes(doc)
}

func sha256Bytes(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// stdSignDoc produces the canonical (sorted-key, no-whitespace) JSON of the
// ADR-36 sign doc. We build it by hand so the field order exactly matches the
// amino JSON that wallets sign; encoding/json sorts map keys alphabetically,
// which is the canonicalization ADR-36 requires.
func stdSignDoc(signer, data string) []byte {
	b64 := base64.StdEncoding.EncodeToString([]byte(data))
	doc := map[string]any{
		"chain_id":       "",
		"account_number": "0",
		"sequence":       "0",
		"fee": map[string]any{
			"gas":    "0",
			"amount": []any{},
		},
		"msgs": []any{
			map[string]any{
				"type": "sign/MsgSignData",
				"value": map[string]any{
					"signer": signer,
					"data":   b64,
				},
			},
		},
		"memo": "",
	}
	// json.Marshal sorts object keys, giving deterministic canonical output.
	out, _ := json.Marshal(doc)
	return out
}
