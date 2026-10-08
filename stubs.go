package main

// ---------- Встроенные функции (stubs) ----------

func (g *Gen) stubPrintHight() {
	// print(rdi = ptr: [len:8][bytes...]) -> write(1, ptr+8, len)
	g.mark("_print")
	g.emit(0x48, 0x89, 0xFE)       // mov rsi, rdi
	g.emit(0x48, 0x8B, 0x17)       // mov rdx, [rdi]
	g.emit(0x48, 0x83, 0xC6, 0x08) // add rsi, 8
	g.emit(0xB8, 0x01, 0, 0, 0)    // mov eax, 1 (sys_write)
	g.emit(0xBF, 0x01, 0, 0, 0)    // mov edi, 1 (stdout)
	g.emit(0x0F, 0x05)             // syscall
	g.emit(0xC3)                   // ret
}

func (g *Gen) stubPutiHight() {
	// puti(rdi = value) — печать десятичного числа
	g.mark("_puti")
	g.emit(0x55)                   // push rbp
	g.emit(0x48, 0x89, 0xE5)       // mov rbp, rsp
	g.emit(0x48, 0x83, 0xEC, 0x40) // sub rsp, 64
	g.emit(0x48, 0x89, 0xF8)       // mov rax, rdi
	g.emit(0x48, 0x8D, 0x75, 0xFF) // lea rsi, [rbp-1]
	g.emit(0x31, 0xDB)             // xor ebx, ebx
	g.emit(0x48, 0x85, 0xC0)       // test rax, rax
	g.jz("puti_pos")               // если 0 — тоже ок (цикл сделает 1 цифру)
	g.jump([]byte{0x0F, 0x89}, "puti_pos") // jns
	g.emit(0x48, 0xF7, 0xD8)       // neg rax
	g.emit(0xBB, 0x01, 0, 0, 0)    // mov ebx, 1
	g.mark("puti_pos")
	g.emit(0xB9, 0x0A, 0, 0, 0)    // mov ecx, 10
	g.mark("puti_loop")
	g.emit(0x31, 0xD2)             // xor edx, edx
	g.emit(0x48, 0xF7, 0xF1)       // div rcx
	g.emit(0x80, 0xC2, 0x30)       // add dl, '0'
	g.emit(0x48, 0xFF, 0xCE)       // dec rsi
	g.emit(0x88, 0x16)             // mov [rsi], dl
	g.emit(0x48, 0x85, 0xC0)       // test rax, rax
	g.jnz("puti_loop")
	g.emit(0x85, 0xDB)             // test ebx, ebx
	g.jz("puti_out")
	g.emit(0x48, 0xFF, 0xCE)       // dec rsi
	g.emit(0xC6, 0x06, 0x2D)       // mov byte [rsi], '-'
	g.mark("puti_out")
	g.emit(0x48, 0x89, 0xEA)       // mov rdx, rbp
	g.emit(0x48, 0xFF, 0xCA)       // dec rdx
	g.emit(0x48, 0x29, 0xF2)       // sub rdx, rsi  (длина)
	g.emit(0xB8, 0x01, 0, 0, 0)    // mov eax, 1
	g.emit(0xBF, 0x01, 0, 0, 0)    // mov edi, 1
	g.emit(0x0F, 0x05)             // syscall
	g.emit(0xC9)                   // leave
	g.emit(0xC3)                   // ret
}

func (g *Gen) stubExitHight() {
	// exit(rdi = code)
	g.mark("_exit")
	g.emit(0xB8, 0x3C, 0, 0, 0) // mov eax, 60
	g.emit(0x0F, 0x05)          // syscall
	g.emit(0xC3)
}

// Версии для $low: вывод в VGA text buffer 0xB8000, без syscall'ов.
func (g *Gen) stubPrintLow() {
	g.addDataQword("vga_cursor", 0)
	g.mark("_print")
	g.emit(0x48, 0x89, 0xFE)             // mov rsi, rdi
	g.emit(0x48, 0x8B, 0x0F)             // mov rcx, [rdi]   (длина)
	g.emit(0x48, 0x83, 0xC6, 0x08)       // add rsi, 8
	g.emit(0x48, 0xC7, 0xC2, 0x00, 0x80, 0x0B, 0x00) // mov rdx, 0xB8000
	// rdx += cursor*2
	g.emit(0x48, 0x8B, 0x05) // mov rax, [rel cursor]
	g.fix = append(g.fix, fixup{len(g.code), 'p', "vga_cursor"})
	g.u32(0)
	g.emit(0x48, 0x8D, 0x14, 0x50) // lea rdx, [rdx + rax*2]
	g.mark("plow_loop")
	g.emit(0x48, 0x85, 0xC9) // test rcx, rcx
	g.jz("plow_done")
	g.emit(0x8A, 0x1E)       // mov bl, [rsi]
	g.emit(0x88, 0x1A)       // mov [rdx], bl
	g.emit(0x48, 0xFF, 0xC2) // inc rdx
	g.emit(0xC6, 0x02, 0x07) // mov byte [rdx], 0x07
	g.emit(0x48, 0xFF, 0xC2) // inc rdx
	g.emit(0x48, 0xFF, 0xC6) // inc rsi
	g.emit(0x48, 0xFF, 0xC9) // dec rcx
	g.jmp("plow_loop")
	g.mark("plow_done")
	// cursor = (rdx - 0xB8000)/2
	g.emit(0x48, 0x81, 0xEA, 0x00, 0x80, 0x0B, 0x00) // sub rdx, 0xB8000
	g.emit(0x48, 0xD1, 0xEA) // shr rdx, 1
	g.emit(0x48, 0x89, 0x15) // mov [rel cursor], rdx
	g.fix = append(g.fix, fixup{len(g.code), 'p', "vga_cursor"})
	g.u32(0)
	g.emit(0xC3)
}

func (g *Gen) stubPutiLow() {
	// то же, но цифры сначала в буфер на стеке, затем в VGA
	g.mark("_puti")
	g.emit(0x55)                   // push rbp
	g.emit(0x48, 0x89, 0xE5)       // mov rbp, rsp
	g.emit(0x48, 0x83, 0xEC, 0x40) // sub rsp, 64
	g.emit(0x48, 0x89, 0xF8)       // mov rax, rdi
	g.emit(0x48, 0x8D, 0x75, 0xFF) // lea rsi, [rbp-1]
	g.emit(0x31, 0xDB)             // xor ebx, ebx
	g.emit(0x48, 0x85, 0xC0)       // test rax,rax
	g.jump([]byte{0x0F, 0x89}, "putil_pos")
	g.emit(0x48, 0xF7, 0xD8) // neg rax
	g.emit(0xBB, 0x01, 0, 0, 0)
	g.mark("putil_pos")
	g.emit(0xB9, 0x0A, 0, 0, 0)
	g.mark("putil_loop")
	g.emit(0x31, 0xD2)
	g.emit(0x48, 0xF7, 0xF1)
	g.emit(0x80, 0xC2, 0x30)
	g.emit(0x48, 0xFF, 0xCE)
	g.emit(0x88, 0x16)
	g.emit(0x48, 0x85, 0xC0)
	g.jnz("putil_loop")
	g.emit(0x85, 0xDB)
	g.jz("putil_out")
	g.emit(0x48, 0xFF, 0xCE)
	g.emit(0xC6, 0x06, 0x2D)
	g.mark("putil_out")
	// копируем [rsi .. rbp-1) в VGA
	g.emit(0x48, 0xC7, 0xC2, 0x00, 0x80, 0x0B, 0x00) // mov rdx, 0xB8000
	g.emit(0x48, 0x8B, 0x05)
	g.fix = append(g.fix, fixup{len(g.code), 'p', "vga_cursor"})
	g.u32(0)
	g.emit(0x48, 0x8D, 0x14, 0x50) // lea rdx, [rdx+rax*2]
	g.emit(0x48, 0x8D, 0x4D, 0xFF) // lea rcx, [rbp-1]
	g.mark("putil_copy")
	g.emit(0x48, 0x39, 0xF1) // cmp rcx, rsi
	g.jump([]byte{0x0F, 0x86}, "putil_done") // jbe
	g.emit(0x8A, 0x1E)       // mov bl, [rsi]
	g.emit(0x88, 0x1A)       // mov [rdx], bl
	g.emit(0x48, 0xFF, 0xC2)
	g.emit(0xC6, 0x02, 0x07)
	g.emit(0x48, 0xFF, 0xC2)
	g.emit(0x48, 0xFF, 0xC6)
	g.jmp("putil_copy")
	g.mark("putil_done")
	g.emit(0x48, 0x81, 0xEA, 0x00, 0x80, 0x0B, 0x00)
	g.emit(0x48, 0xD1, 0xEA)
	g.emit(0x48, 0x89, 0x15)
	g.fix = append(g.fix, fixup{len(g.code), 'p', "vga_cursor"})
	g.u32(0)
	g.emit(0xC9)
	g.emit(0xC3)
}

func (g *Gen) stubExitLow() {
	// exit: cli; hlt; jmp hlt
	g.mark("_exit")
	g.emit(0xFA)       // cli
	g.emit(0xF4)       // hlt
	g.emit(0xEB, 0xFD) // jmp назад на hlt
}

// ---------- Компиляция программы ----------

