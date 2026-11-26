package httpapi

import (
	"net/http"
	"strings"
)

func wantsAsync(r *http.Request) bool {
	for _, value := range r.Header.Values("Prefer") {
		parts, ok := splitPreferences(value)
		if !ok {
			continue
		}
		for _, part := range parts {
			if !strings.Contains(part, ";") && strings.EqualFold(strings.TrimSpace(part), "respond-async") {
				return true
			}
		}
	}
	return false
}

// splitPreferences separates RFC 7230 list members while preserving quoted
// commas. Any malformed quote makes that field unusable as an async signal.
func splitPreferences(value string) ([]string, bool) {
	var out []string
	start, quoted, escaped := 0, false, false
	for i := 0; i < len(value); i++ {
		switch c := value[i]; {
		case escaped:
			escaped = false
		case quoted && c == '\\':
			escaped = true
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			out = append(out, value[start:i])
			start = i + 1
		}
	}
	if quoted || escaped {
		return nil, false
	}
	return append(out, value[start:]), true
}
