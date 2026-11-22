package cloud

import "strings"

// commands splits a shell command line into simple commands, each a list of
// words with quotes removed. It is a reader for what agents typed, not a
// shell: it ignores expansions it cannot resolve and treats subshells and
// command substitutions as separate commands.
func commands(s string) [][]string {
	var (
		out      [][]string
		cmd      []string
		word     strings.Builder
		inWord   bool
		dropNext bool     // the next word is a redirect target
		heredocs []string // delimiters waiting for the end of this line
	)
	endWord := func() {
		if inWord {
			if dropNext {
				dropNext = false
			} else {
				cmd = append(cmd, word.String())
			}
		}
		word.Reset()
		inWord = false
	}
	endCmd := func() {
		endWord()
		dropNext = false
		if len(cmd) > 0 {
			out = append(out, cmd)
		}
		cmd = nil
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\n':
			endCmd()
			for _, d := range heredocs {
				i = skipHeredoc(s, i+1, d) - 1
			}
			heredocs = nil
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
		case c == ';' || c == '&' || c == '|' || c == '(' || c == ')' || c == '`':
			if c == '`' && inWord {
				word.WriteByte('`') // a name built from a substitution is unknown
			}
			endCmd()
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			if inWord {
				word.WriteString("$(")
			}
			endCmd()
			i++
		case c == '#' && !inWord:
			for i < len(s) && s[i] != '\n' {
				i++
			}
			i--
		case c == '<' && strings.HasPrefix(s[i:], "<<<"):
			endWord()
			i += 2
		case c == '<' && strings.HasPrefix(s[i:], "<<"):
			endWord()
			j := i + 2
			if j < len(s) && s[j] == '-' {
				j++
			}
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			k := j
			for k < len(s) && !strings.ContainsRune(" \t\n;&|<>()", rune(s[k])) {
				k++
			}
			if d := strings.Trim(s[j:k], `'"\`); d != "" {
				heredocs = append(heredocs, d)
			}
			i = k - 1
		case c == '>' || c == '<':
			if inWord && isDigits(word.String()) {
				word.Reset()
				inWord = false
			} else {
				endWord()
			}
			for i+1 < len(s) && (s[i+1] == '>' || s[i+1] == '&' || s[i+1] == '|') {
				i++
			}
			for i+1 < len(s) && (s[i+1] == ' ' || s[i+1] == '\t') {
				i++
			}
			dropNext = true
			inWord = false
		case c == '\\':
			if i+1 < len(s) {
				i++
				if s[i] != '\n' {
					word.WriteByte(s[i])
					inWord = true
				}
			}
		case c == '\'':
			inWord = true
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				word.WriteString(s[i+1:])
				i = len(s)
				break
			}
			word.WriteString(s[i+1 : i+1+end])
			i += end + 1
		case c == '"':
			inWord = true
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					i++
					if s[i] == '\n' {
						i++
						continue
					}
				}
				word.WriteByte(s[i])
				i++
			}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	endCmd()
	return out
}

// skipHeredoc returns the index just past the line that ends a heredoc.
func skipHeredoc(s string, from int, delim string) int {
	i := from
	for i < len(s) {
		end := strings.IndexByte(s[i:], '\n')
		line := s[i:]
		next := len(s)
		if end >= 0 {
			line, next = s[i:i+end], i+end+1
		}
		if strings.TrimSpace(line) == delim {
			return next
		}
		i = next
	}
	return len(s)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
