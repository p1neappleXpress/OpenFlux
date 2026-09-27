package main

import (
	"fmt"
	"os"
	"strings"
)

// readURLFile returns the document URL kept in path (--url-file): the first
// non-empty line that is not a # comment.
func readURLFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := strings.TrimPrefix(string(data), string(rune(0xFEFF))) // a BOM from Notepad
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line, nil
		}
	}
	return "", fmt.Errorf("%s: no URL in the file", path)
}
