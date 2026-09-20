package oneme

import (
	"crypto/rand"
	"fmt"
	"log"
	"openflux/utils"
)

var vvv bool

func logDebug(format string, args ...interface{}) {
	if vvv {
		utils.Debugf("[MAX] "+format, args...)
	}
}

func logInfo(format string, args ...interface{}) {
	log.Print(utils.RedactURLs(fmt.Sprintf("[MAX] "+format, args...)))
}

func logError(format string, args ...interface{}) {
	log.Print(utils.RedactURLs(fmt.Sprintf("[MAX] "+format, args...)))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func genUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
