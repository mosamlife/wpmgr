package razorpay

// signature.go — the two HMAC-SHA256 signature checks this adapter performs,
// both hex-encoded and constant-time compared, per
// https://razorpay.com/docs/webhooks/validate-test/ and
// https://razorpay.com/docs/payments/subscriptions/verify-signature/:
//
//  1. verifyHMACSHA256Hex: the webhook body signature (X-Razorpay-Signature),
//     keyed by the WEBHOOK secret.
//  2. the browser checkout-callback signature (razorpay_signature), keyed by
//     the API KEY SECRET — see VerifyCheckoutCallback in provider.go.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// verifyHMACSHA256Hex reports whether hexSignature is the correct
// HMAC-SHA256(body, secret), hex-encoded, compared in constant time. body
// MUST be the exact raw bytes being authenticated — for a webhook this means
// the raw request body BEFORE any JSON unmarshal (re-marshaling a decoded
// struct can reorder keys/whitespace and silently break verification).
func verifyHMACSHA256Hex(body []byte, hexSignature, secret string) bool {
	if hexSignature == "" || secret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)

	sig, err := hex.DecodeString(hexSignature)
	if err != nil {
		return false
	}
	return hmac.Equal(expected, sig)
}
