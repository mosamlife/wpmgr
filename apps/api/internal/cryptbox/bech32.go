package cryptbox

import (
	"fmt"
	"strings"
)

// This file implements just enough of the Bech32 encoding (BIP-173) to build
// age's on-disk X25519 secret-key string — "AGE-SECRET-KEY-1" followed by a
// Bech32-encoded 32-byte scalar and a 6-character checksum, all upper-cased.
// That is the exact format age.ParseX25519Identity accepts (and the format
// age-keygen emits): filippo.io/age itself vendors this same reference
// algorithm as an unexported internal package, so it cannot be imported from
// here — this is an independent, from-spec reimplementation of the encode
// half only. Its correctness is proven by round-tripping every value it
// produces through age.ParseX25519Identity in cryptbox_test.go; a checksum
// bug would fail every one of those tests, not just this package's own.

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var bech32Generator = [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}

func bech32Polymod(values []byte) uint32 {
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= bech32Generator[i]
			}
		}
	}
	return chk
}

func bech32HRPExpand(hrp string) []byte {
	lower := strings.ToLower(hrp)
	ret := make([]byte, 0, len(lower)*2+1)
	for i := 0; i < len(lower); i++ {
		ret = append(ret, lower[i]>>5)
	}
	ret = append(ret, 0)
	for i := 0; i < len(lower); i++ {
		ret = append(ret, lower[i]&31)
	}
	return ret
}

func bech32CreateChecksum(hrp string, data []byte) []byte {
	values := append(bech32HRPExpand(hrp), data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	mod := bech32Polymod(values) ^ 1
	ret := make([]byte, 6)
	for p := 0; p < 6; p++ {
		shift := uint(5 * (5 - p))
		ret[p] = byte(mod>>shift) & 31
	}
	return ret
}

// bech32ConvertBits regroups a byte slice from an 8-bit-per-element base into
// a 5-bit-per-element base (or the reverse), as required to map arbitrary
// 8-bit secret-key bytes onto Bech32's 5-bit charset.
func bech32ConvertBits(data []byte, fromBits, toBits uint, pad bool) ([]byte, error) {
	var ret []byte
	acc := uint32(0)
	bits := uint(0)
	maxv := uint32(1<<toBits - 1)
	for idx, value := range data {
		if uint32(value)>>fromBits != 0 {
			return nil, fmt.Errorf("bech32: invalid data range: data[%d]=%d", idx, value)
		}
		acc = acc<<fromBits | uint32(value)
		bits += fromBits
		for bits >= toBits {
			bits -= toBits
			ret = append(ret, byte(acc>>bits)&byte(maxv))
		}
	}
	if pad {
		if bits > 0 {
			ret = append(ret, byte(acc<<(toBits-bits))&byte(maxv))
		}
	} else if bits >= fromBits {
		return nil, fmt.Errorf("bech32: illegal zero padding")
	} else if byte(acc<<(toBits-bits))&byte(maxv) != 0 {
		return nil, fmt.Errorf("bech32: non-zero padding")
	}
	return ret, nil
}

// bech32Encode encodes hrp+data as a Bech32 string. If hrp is not entirely
// lowercase, the output is upper-cased in full (matching age's own encoder,
// which is what makes the "AGE-SECRET-KEY-" human-readable part come out
// upper-case to match age-keygen's convention).
func bech32Encode(hrp string, data []byte) (string, error) {
	if len(hrp) < 1 {
		return "", fmt.Errorf("bech32: invalid hrp: %q", hrp)
	}
	for p, c := range hrp {
		if c < 33 || c > 126 {
			return "", fmt.Errorf("bech32: invalid hrp character: hrp[%d]=%d", p, c)
		}
	}
	if strings.ToUpper(hrp) != hrp && strings.ToLower(hrp) != hrp {
		return "", fmt.Errorf("bech32: mixed case hrp: %q", hrp)
	}
	values, err := bech32ConvertBits(data, 8, 5, true)
	if err != nil {
		return "", err
	}
	lower := strings.ToLower(hrp) == hrp
	lowerHRP := strings.ToLower(hrp)
	var sb strings.Builder
	sb.WriteString(lowerHRP)
	sb.WriteString("1")
	for _, p := range values {
		sb.WriteByte(bech32Charset[p])
	}
	for _, p := range bech32CreateChecksum(lowerHRP, values) {
		sb.WriteByte(bech32Charset[p])
	}
	if lower {
		return sb.String(), nil
	}
	return strings.ToUpper(sb.String()), nil
}

// ageSecretKeyHRP is age's human-readable part for an X25519 identity's
// on-disk secret-key encoding.
const ageSecretKeyHRP = "AGE-SECRET-KEY-"

// encodeAgeSecretKey encodes a 32-byte X25519 scalar as age's on-disk secret
// key string (bech32, HRP "AGE-SECRET-KEY-", upper-cased) — the exact format
// age.ParseX25519Identity requires and age-keygen emits.
func encodeAgeSecretKey(scalar []byte) (string, error) {
	if len(scalar) != ageX25519ScalarSize {
		return "", fmt.Errorf("encode age secret key: want %d bytes, got %d", ageX25519ScalarSize, len(scalar))
	}
	return bech32Encode(ageSecretKeyHRP, scalar)
}
