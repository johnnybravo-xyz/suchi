// SPDX-License-Identifier: AGPL-3.0-or-later

package localauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- RFC 6238 TOTP requires HMAC-SHA1 interoperability.
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"time"
)

const (
	totpDigits          = 6
	totpPeriod          = 30 * time.Second
	totpSecretBytes     = 20
	recoveryCodeCount   = 10
	recoveryCodeBytes   = 15
	recoveryCodeEncoded = 24
)

var (
	base32NoPadding        = base32.StdEncoding.WithPadding(base32.NoPadding)
	errMalformedFactorCode = errors.New("local-auth: malformed two-factor code")
)

func generateTOTPSecret() ([]byte, string, error) {
	secret := make([]byte, totpSecretBytes)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return nil, "", err
	}
	return secret, base32NoPadding.EncodeToString(secret), nil
}

func decodeTOTPSecret(encoded string) ([]byte, error) {
	secret, err := base32NoPadding.DecodeString(strings.ToUpper(strings.TrimSpace(encoded)))
	if err != nil || len(secret) != totpSecretBytes {
		return nil, errMalformedFactorCode
	}
	return secret, nil
}

func matchTOTP(secret []byte, submitted string, now time.Time, lastUsedStep int64) (int64, bool) {
	if len(secret) != totpSecretBytes || len(submitted) != totpDigits {
		return 0, false
	}
	var submittedValue uint32
	for i := 0; i < len(submitted); i++ {
		if submitted[i] < '0' || submitted[i] > '9' {
			return 0, false
		}
		submittedValue = submittedValue*10 + uint32(submitted[i]-'0')
	}

	current := now.Unix() / int64(totpPeriod/time.Second)
	matched := int64(-1)
	for offset := int64(-1); offset <= 1; offset++ {
		step := current + offset
		if step >= 0 && step > lastUsedStep && hotpValue(secret, uint64(step), totpDigits) == submittedValue && step > matched {
			matched = step
		}
	}
	return matched, matched >= 0
}

func hotpCode(secret []byte, counter uint64, digits int) string {
	value := hotpValue(secret, counter, digits)
	buf := make([]byte, digits)
	for i := digits - 1; i >= 0; i-- {
		buf[i] = byte(value%10) + '0'
		value /= 10
	}
	return string(buf)
}

func hotpValue(secret []byte, counter uint64, digits int) uint32 {
	var counterBytes [8]byte
	binary.BigEndian.PutUint64(counterBytes[:], counter)
	mac := hmac.New(sha1.New, secret) // #nosec G401 -- RFC 6238 mandates HMAC-SHA1 here.
	_, _ = mac.Write(counterBytes[:])
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 0x0f
	binaryCode := (uint32(digest[offset])&0x7f)<<24 |
		uint32(digest[offset+1])<<16 |
		uint32(digest[offset+2])<<8 |
		uint32(digest[offset+3])
	modulus := uint32(1)
	for range digits {
		modulus *= 10
	}
	return binaryCode % modulus
}

func generateRecoveryCodes() ([]string, [][]byte, error) {
	codes := make([]string, recoveryCodeCount)
	digests := make([][]byte, recoveryCodeCount)
	var raw [recoveryCodeBytes]byte
	for i := range recoveryCodeCount {
		if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
			return nil, nil, err
		}
		canonical := base32NoPadding.EncodeToString(raw[:])
		codes[i] = formatRecoveryCode(canonical)
		sum := sha256.Sum256([]byte(canonical))
		digests[i] = append([]byte(nil), sum[:]...)
	}
	return codes, digests, nil
}

func formatRecoveryCode(canonical string) string {
	var formatted [recoveryCodeEncoded + 5]byte
	out := 0
	for i := 0; i < len(canonical); i++ {
		if i > 0 && i%4 == 0 {
			formatted[out] = '-'
			out++
		}
		formatted[out] = canonical[i]
		out++
	}
	return string(formatted[:out])
}

func recoveryCodeDigest(submitted string) ([sha256.Size]byte, error) {
	var canonical [recoveryCodeEncoded]byte
	n := 0
	for i := 0; i < len(submitted); i++ {
		char := submitted[i]
		if char == '-' || char == ' ' || char == '\t' || char == '\r' || char == '\n' {
			continue
		}
		if char >= 'a' && char <= 'z' {
			char -= 'a' - 'A'
		}
		if !((char >= 'A' && char <= 'Z') || (char >= '2' && char <= '7')) || n >= len(canonical) {
			return [sha256.Size]byte{}, errMalformedFactorCode
		}
		canonical[n] = char
		n++
	}
	if n != len(canonical) {
		return [sha256.Size]byte{}, errMalformedFactorCode
	}
	return sha256.Sum256(canonical[:]), nil
}

func clearBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
