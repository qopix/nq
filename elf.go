package main

// ---------- Свой ELF64 writer (без линкера, без C) ----------

const (
	elfBase    = 0x400000          // обычная программа Linux
	kernelBase = 0xFFFFFFFF80100000 // $kernel: higher half, нужен загрузчик
	elfHdrSize = 64 + 56           // ehdr + 1 phdr
)

// WriteELF собирает минимальный ELF64 (ET_EXEC, один RWE-сегмент) с базой base.
// code должен быть уже запатчен под адрес base+elfHdrSize.
func WriteELF(code []byte, base uint64) []byte {
	entry := base + elfHdrSize
	total := uint64(elfHdrSize + len(code))

	out := make([]byte, 0, total)
	out = append(out, 0x7F, 'E', 'L', 'F', 2, 1, 1, 0)
	out = append(out, make([]byte, 8)...)
	w16 := func(v uint16) { out = append(out, byte(v), byte(v>>8)) }
	w32 := func(v uint32) { out = append(out, byte(v), byte(v>>8), byte(v>>16), byte(v>>24)) }
	w64 := func(v uint64) {
		for i := 0; i < 8; i++ {
			out = append(out, byte(v>>(8*i)))
		}
	}
	w16(2)     // ET_EXEC
	w16(0x3E)  // EM_X86_64
	w32(1)     // version
	w64(entry) // e_entry
	w64(64)    // e_phoff
	w64(0)     // e_shoff
	w32(0)     // flags
	w16(64)    // e_ehsize
	w16(56)    // e_phentsize
	w16(1)     // e_phnum
	w16(0)     // e_shentsize
	w16(0)     // e_shnum
	w16(0)     // e_shstrndx
	w32(1)     // PT_LOAD
	w32(7)     // R|W|X
	w64(0)     // p_offset
	w64(base)  // p_vaddr
	w64(base)  // p_paddr
	w64(total) // p_filesz
	w64(total) // p_memsz
	w64(0x1000)
	out = append(out, code...)
	return out
}
