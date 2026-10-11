package main

import (
	"fmt"
	"strconv"
	"strings"
)

// ---------- Tokens ----------

type Tok struct {
	Kind string // "ident" "num" "str" "punct" "eof"
	Val  string
	Line int
	Col  int // столбец в символах, с 1
}

// Ровно 25 ключевых слов языка Nq.
var keywords = map[string]bool{
	"fn": true, "let": true, "var": true, "const": true,
	"if": true, "else": true, "while": true, "for": true,
	"return": true, "break": true, "continue": true,
	"import": true, "as": true, "type": true, "struct": true,
	"ptr": true, "deref": true, "addr": true, "sizeof": true,
	"cast": true, "extern": true, "asm": true,
	"true": true, "false": true, "nil": true,
}

const puncts = "+-*/%=!<>&|^~@(){}[],;:$."

type Lexer struct {
	src       string
	pos       int
	line      int
	lineStart int    // позиция начала текущей строки в src
	file      string // для сообщений об ошибках
	lines     []string
}

func NewLexer(src string) *Lexer { return &Lexer{src: src, line: 1} }

func isAlpha(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func (l *Lexer) err(msg string) {
	panic(formatDiag(l.file, l.line, colOf(l.src, l.lineStart, l.pos), msg, l.lines))
}

func (l *Lexer) Tokenize() []Tok {
	var out []Tok
	l.lines = strings.Split(l.src, "\n")
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		col := colOf(l.src, l.lineStart, l.pos)
		switch {
		case c == '\n':
			l.line++
			l.pos++
			l.lineStart = l.pos
		case c == ' ' || c == '\t' || c == '\r':
			l.pos++
		case c == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
		case c == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '*':
			l.pos += 2
			for l.pos+1 < len(l.src) && !(l.src[l.pos] == '*' && l.src[l.pos+1] == '/') {
				if l.src[l.pos] == '\n' {
					l.line++
					l.lineStart = l.pos + 1
				}
				l.pos++
			}
			l.pos += 2
		case isAlpha(c):
			start := l.pos
			for l.pos < len(l.src) && (isAlpha(l.src[l.pos]) || isDigit(l.src[l.pos])) {
				l.pos++
			}
			out = append(out, Tok{"ident", l.src[start:l.pos], l.line, col})
		case isDigit(c):
			start := l.pos
			if c == '0' && l.pos+1 < len(l.src) && (l.src[l.pos+1] == 'x' || l.src[l.pos+1] == 'X') {
				l.pos += 2
				for l.pos < len(l.src) && (isDigit(l.src[l.pos]) ||
					(l.src[l.pos] >= 'a' && l.src[l.pos] <= 'f') ||
					(l.src[l.pos] >= 'A' && l.src[l.pos] <= 'F')) {
					l.pos++
				}
			} else {
				for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
					l.pos++
				}
			}
			out = append(out, Tok{"num", l.src[start:l.pos], l.line, col})
		case c == '"':
			l.pos++
			var s []byte
			for l.pos < len(l.src) && l.src[l.pos] != '"' {
				ch := l.src[l.pos]
				if ch == '\\' && l.pos+1 < len(l.src) {
					l.pos++
					switch l.src[l.pos] {
					case 'n':
						s = append(s, '\n')
					case 't':
						s = append(s, '\t')
					case 'r':
						s = append(s, '\r')
					case '0':
						s = append(s, 0)
					case '\\':
						s = append(s, '\\')
					case '"':
						s = append(s, '"')
					default:
						s = append(s, l.src[l.pos])
					}
					l.pos++
				} else {
					if ch == '\n' {
						l.line++
						l.lineStart = l.pos + 1
					}
					s = append(s, ch)
					l.pos++
				}
			}
			if l.pos >= len(l.src) {
				l.err("незакрытая строка")
			}
			l.pos++
			out = append(out, Tok{"str", string(s), l.line, col})
		case c == '\'':
			l.pos++
			if l.pos >= len(l.src) {
				l.err("незакрытый символ")
			}
			ch := l.src[l.pos]
			if ch == '\\' && l.pos+1 < len(l.src) {
				l.pos++
				switch l.src[l.pos] {
				case 'n':
					ch = '\n'
				case 't':
					ch = '\t'
				case 'r':
					ch = '\r'
				case '0':
					ch = 0
				default:
					ch = l.src[l.pos]
				}
			}
			l.pos++
			if l.pos >= len(l.src) || l.src[l.pos] != '\'' {
				l.err("символ должен закрываться '")
			}
			l.pos++
			out = append(out, Tok{"num", strconv.Itoa(int(ch)), l.line, col})
		default:
			// двухсимвольные операторы
			if l.pos+1 < len(l.src) {
				two := l.src[l.pos : l.pos+2]
				switch two {
				case "==", "!=", "<=", ">=", "&&", "||", "->", "+=", "-=", "*=", "/=", "%=", "<<", ">>", "&=", "|=", "^=":
					out = append(out, Tok{"punct", two, l.line, col})
					l.pos += 2
					continue
				}
			}
			ok := false
			for i := 0; i < len(puncts); i++ {
				if puncts[i] == c {
					ok = true
					break
				}
			}
			if !ok {
				l.err(fmt.Sprintf("неожиданный символ %q", string(c)))
			}
			out = append(out, Tok{"punct", string(c), l.line, col})
			l.pos++
		}
	}
	out = append(out, Tok{"eof", "", l.line, colOf(l.src, l.lineStart, l.pos)})
	return out
}
