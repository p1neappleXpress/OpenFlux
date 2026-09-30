package utils

import "unicode/utf16"

// MinSecretChars is the shortest shared secret any peer accepts.
const MinSecretChars = 16

// SecretChars is the length of a secret in characters as every OpenFlux
// client counts them: UTF-16 code units, the way Kotlin and Java measure
// String.length. Counting bytes instead (as the core once did) let the core
// accept a 10-letter Cyrillic secret (20 bytes) that Desktop and Android
// reject, so the same link worked on one device and not on another.
func SecretChars(s string) int {
	return len(utf16.Encode([]rune(s)))
}
