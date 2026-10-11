package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// formatDiag собирает сообщение об ошибке в виде
//
//	файл:строка:столбец: ошибка: текст
//	  12 | let x: u8 = 300
//	     |             ^
func formatDiag(file string, line, col int, msg string, lines []string) string {
	var b strings.Builder
	if col > 0 {
		fmt.Fprintf(&b, "%s:%d:%d: ошибка: %s", file, line, col, msg)
	} else {
		fmt.Fprintf(&b, "%s:%d: ошибка: %s", file, line, msg)
	}
	if line >= 1 && line <= len(lines) {
		text := strings.TrimRight(lines[line-1], "\r")
		fmt.Fprintf(&b, "\n  %4d | %s", line, text)
		if col > 0 {
			pad := make([]rune, 0, col)
			n := 0
			for _, r := range text {
				if n >= col-1 {
					break
				}
				if r == '\t' {
					pad = append(pad, '\t')
				} else {
					pad = append(pad, ' ')
				}
				n++
			}
			for ; n < col-1; n++ {
				pad = append(pad, ' ')
			}
			fmt.Fprintf(&b, "\n       | %s^", string(pad))
		}
	}
	return b.String()
}

// colOf: номер столбца (в символах, с 1) для позиции pos в src, начало строки — lineStart.
func colOf(src string, lineStart, pos int) int {
	if pos < lineStart {
		return 1
	}
	return utf8.RuneCountInString(src[lineStart:pos]) + 1
}
