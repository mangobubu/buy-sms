package application

import (
	"regexp"
	"strings"

	"buysms/internal/domain"
)

// Keep formatted numeric tokens together so a code such as "2895-3" is not
// mistaken for a four-digit OTP followed by an unrelated digit.
var smsCodeNumbers = regexp.MustCompile(`[0-9]+(?:[\s\p{Zs}\p{Pd}.]+[0-9]+)*`)

// displaySMSCode reconciles Hero's extracted code with the original SMS only
// when the body has one distinct, standalone 4-8 digit candidate contained in
// the supplied code. Some responses include an expiry digit in smsCode.
// Keep this at the presentation boundary: historical messages benefit too,
// while stored provider codes, message fingerprints and poll state stay intact.
func displaySMSCode(providerID, code, text string) string {
	if domain.NormalizeProvider(strings.ToLower(strings.TrimSpace(providerID))) != domain.ProviderHeroSMS || len(code) <= 4 || text == "" {
		return code
	}
	for _, char := range code {
		if char < '0' || char > '9' {
			return code
		}
	}

	spans := smsCodeNumbers.FindAllStringIndex(text, -1)
	// An exact upstream code is authoritative even when the provider places it
	// directly after an ASCII label (for example, "OTP28953").
	for _, span := range spans {
		if strings.Map(func(char rune) rune {
			if char >= '0' && char <= '9' {
				return char
			}
			return -1
		}, text[span[0]:span[1]]) == code {
			return code
		}
	}

	candidate := ""
	for _, span := range spans {
		start, end := span[0], span[1]
		if (start > 0 && smsCodeWordByte(text[start-1])) || (end < len(text) && smsCodeWordByte(text[end])) {
			continue
		}
		number := text[start:end]
		digits := strings.Map(func(char rune) rune {
			if char >= '0' && char <= '9' {
				return char
			}
			return -1
		}, number)

		if digits != number || len(number) < 4 || len(number) > 8 || !strings.Contains(code, number) {
			continue
		}
		if candidate != "" && candidate != number {
			return code
		}
		candidate = number
	}
	if candidate != "" && len(candidate) < len(code) && strings.Contains(code, candidate) {
		return candidate
	}
	return code
}

// ASCII identifiers are not numeric OTPs. Non-ASCII neighbours remain valid
// so messages such as "验证码2895，有效期3分钟" need no surrounding spaces.
func smsCodeWordByte(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_'
}
