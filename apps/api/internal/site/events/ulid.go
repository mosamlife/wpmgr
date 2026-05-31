package events

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

// ----------------------------------------------------------------------------
// Minimal ULID (ADR-038). A ULID is a 26-char Crockford-base32 string that is
// lexicographically sortable by creation time: the first 48 bits are the Unix
// millisecond timestamp, the remaining 80 bits are randomness. We mint our own
// rather than add a dependency (none present in go.mod) — the only property the
// SSE bus needs is "monotonic, lexicographically-sortable, collision-free per
// process", which a time-prefix + per-millisecond monotonic random tail gives.
//
// Monotonicity within the same millisecond is guaranteed by incrementing the
// random tail (like the canonical oklog/ulid MonotonicEntropy), so two events
// minted in the same ms still sort in emit order — important because the SSE
// replay cursor (?since / Last-Event-ID) does a string `> $cursor` compare.
// ----------------------------------------------------------------------------

const encoding = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base32

var ulidMu struct {
	sync.Mutex
	lastMS   uint64
	lastRand [10]byte
}

// NewULID returns a fresh, monotonic ULID for the given time.
func NewULID(t time.Time) string {
	ms := uint64(t.UnixMilli())

	ulidMu.Lock()
	if ms == ulidMu.lastMS {
		// Same millisecond: increment the previous random tail so the new id
		// sorts strictly after the previous one.
		incr(&ulidMu.lastRand)
	} else {
		ulidMu.lastMS = ms
		_, _ = rand.Read(ulidMu.lastRand[:])
	}
	var entropy [10]byte = ulidMu.lastRand
	ulidMu.Unlock()

	var raw [16]byte
	// 48-bit timestamp (big-endian) in the top 6 bytes.
	binary.BigEndian.PutUint64(raw[0:8], ms<<16)
	copy(raw[6:16], entropy[:])

	return encode(raw)
}

// incr adds 1 to the 80-bit big-endian random tail (with carry).
func incr(b *[10]byte) {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return
		}
	}
}

// encode renders the 128-bit value as 26 Crockford-base32 chars.
func encode(raw [16]byte) string {
	var out [26]byte
	out[0] = encoding[(raw[0]&224)>>5]
	out[1] = encoding[raw[0]&31]
	out[2] = encoding[(raw[1]&248)>>3]
	out[3] = encoding[((raw[1]&7)<<2)|((raw[2]&192)>>6)]
	out[4] = encoding[(raw[2]&62)>>1]
	out[5] = encoding[((raw[2]&1)<<4)|((raw[3]&240)>>4)]
	out[6] = encoding[((raw[3]&15)<<1)|((raw[4]&128)>>7)]
	out[7] = encoding[(raw[4]&124)>>2]
	out[8] = encoding[((raw[4]&3)<<3)|((raw[5]&224)>>5)]
	out[9] = encoding[raw[5]&31]
	out[10] = encoding[(raw[6]&248)>>3]
	out[11] = encoding[((raw[6]&7)<<2)|((raw[7]&192)>>6)]
	out[12] = encoding[(raw[7]&62)>>1]
	out[13] = encoding[((raw[7]&1)<<4)|((raw[8]&240)>>4)]
	out[14] = encoding[((raw[8]&15)<<1)|((raw[9]&128)>>7)]
	out[15] = encoding[(raw[9]&124)>>2]
	out[16] = encoding[((raw[9]&3)<<3)|((raw[10]&224)>>5)]
	out[17] = encoding[raw[10]&31]
	out[18] = encoding[(raw[11]&248)>>3]
	out[19] = encoding[((raw[11]&7)<<2)|((raw[12]&192)>>6)]
	out[20] = encoding[(raw[12]&62)>>1]
	out[21] = encoding[((raw[12]&1)<<4)|((raw[13]&240)>>4)]
	out[22] = encoding[((raw[13]&15)<<1)|((raw[14]&128)>>7)]
	out[23] = encoding[(raw[14]&124)>>2]
	out[24] = encoding[((raw[14]&3)<<3)|((raw[15]&224)>>5)]
	out[25] = encoding[raw[15]&31]
	return string(out[:])
}
