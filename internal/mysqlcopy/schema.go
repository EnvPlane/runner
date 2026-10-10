package mysqlcopy

import "strings"

// normalizeSchema removes ONLY the mutable AUTO_INCREMENT numeric table option
// outside all quotes and outside the CREATE column/constraint parentheses. A
// column AUTO_INCREMENT attribute, literal/default/comment and index definition
// remain byte-exact. No other schema normalization is permitted.
func normalizeSchema(s string) string {
	var quote byte
	escape := false
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if quote != 0 {
			switch c {
			case '\\':
				escape = true
			case quote:
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			continue
		}
		if c == '(' {
			depth++
			continue
		}
		if c == ')' {
			depth--
			continue
		}
		if depth != 0 || i == 0 || s[i-1] != ' ' || !strings.HasPrefix(s[i:], "AUTO_INCREMENT=") {
			continue
		}
		end := i + len("AUTO_INCREMENT=")
		start := end
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			end++
		}
		if end == start || (end < len(s) && s[end] != ' ' && s[end] != ';') {
			continue
		}
		return s[:i-1] + s[end:]
	}
	return s
}
