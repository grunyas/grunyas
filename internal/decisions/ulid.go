package decisions

import (
	"crypto/rand"
	"fmt"
	"time"
)

const encoding = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID generates a ULID-compliant 26-character identifier per
// https://github.com/ulid/spec.
func NewULID() string {
	now := uint64(time.Now().UTC().UnixMilli())

	var src [16]byte
	src[0] = byte(now >> 40)
	src[1] = byte(now >> 32)
	src[2] = byte(now >> 24)
	src[3] = byte(now >> 16)
	src[4] = byte(now >> 8)
	src[5] = byte(now)
	if _, err := rand.Read(src[6:]); err != nil {
		panic(fmt.Sprintf("ulid: crypto/rand.Read failed: %v", err))
	}

	var dst [26]byte
	// 16 bytes → 128 bits → 26 base32 chars (130 bits, last 2 are padding)
	for i := 0; i < 26; i++ {
		bitOff := i * 5
		byteIdx := bitOff / 8
		bitShift := bitOff % 8

		val := uint16(src[byteIdx]) << 8
		if byteIdx+1 < 16 {
			val |= uint16(src[byteIdx+1])
		}
		val >>= (16 - 5 - bitShift)
		dst[i] = encoding[val&0x1F]
	}
	return string(dst[:])
}
