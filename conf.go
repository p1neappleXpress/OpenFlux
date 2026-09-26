package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// confFile is a parsed OpenFlux .conf, INI-like.
type confFile struct {
	Interface  map[string]string
	Transports []confTransport
}

type confTransport struct {
	Name   string
	Values map[string]string
}

// parseConf reads a .conf file.
func parseConf(path string) (*confFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := &confFile{Interface: make(map[string]string)}
	var current *confTransport
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			body := strings.TrimSpace(line[1 : len(line)-1])
			if strings.EqualFold(body, "Interface") {
				current = nil
				continue
			}
			if strings.HasPrefix(strings.ToLower(body), "transport") {
				name := strings.TrimSpace(body[len("Transport"):])
				name = strings.Trim(name, `"`)
				out.Transports = append(out.Transports, confTransport{
					Name:   name,
					Values: make(map[string]string),
				})
				current = &out.Transports[len(out.Transports)-1]
				continue
			}
			return nil, fmt.Errorf("%s:%d: unknown section [%s]", path, lineNo, body)
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected key = value", path, lineNo)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if i := strings.IndexAny(v, "#;"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		if current == nil {
			out.Interface[k] = v
		} else {
			current.Values[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// confValue returns the value for key in the [Interface] section.
func confValue(iface map[string]string, key string) (string, bool) {
	v, ok := iface[key]
	return v, ok
}

// confBool parses a bool with a fallback.
func confBool(v string, def bool) bool {
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// confInt parses an int with a fallback.
func confInt(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// applyConfString sets *target to the conf value if the flag was not
// explicitly set on the command line.
func applyConfString(iface map[string]string, confKey, flagName string, target *string, set map[string]bool) {
	if set[flagName] {
		return
	}
	if v, ok := confValue(iface, confKey); ok {
		*target = v
	}
}
