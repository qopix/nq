package main

import (
	"fmt"
	"sort"
	"strconv"
)

// ---------- Генератор машинного кода x86-64 ----------
// Всё собирается вручную: байты инструкций + фиксапы. Никаких C-прослоек.

type fixup struct {
	pos    int    // позиция 4 или 8 байт в code
	kind   byte   // 'r' = rel32 к метке кода, 'a' = abs64 к данным, 'p' = rip-rel32 к данным
	target string
}

type local struct {
	off int // смещение от rbp (отрицательное)
	typ *Type
	mut bool
}

type Gen struct {
	code    []byte
	data    []byte
	fix     []fixup
	labels  map[string]int
	dlabels map[string]int
	strLbl  map[string]string
	lblN    int
	strCnt  int

	mode   string // "low" | "mid" | "hight"
	target string // "hosted" | "kernel" | "boot"

	cur    *Func
	line   int
	col    int // столбец начала текущего оператора
	scopes []map[string]*local
	frame  int
	brkLbl []string
	cntLbl []string

	bssSize   int
	bssLabels map[string]int
	Extra     uint64 // сколько памяти сверх образа нужно под bss и стек (считает Patch)
}

func NewGen(mode string) *Gen {
	return &Gen{
		labels:    map[string]int{},
		bssLabels: map[string]int{},
		dlabels:   map[string]int{},
		strLbl:  map[string]string{},
		mode:    mode,
		target:  "hosted",
	}
}

func (g *Gen) fail(format string, a ...any) {
	file := ""
	var lines []string
	if g.cur != nil {
		file = g.cur.File
		if g.cur.Prog != nil {
			lines = g.cur.Prog.Lines
		}
	}
	panic(formatDiag(file, g.line, g.col, fmt.Sprintf(format, a...), lines))
}

func (g *Gen) emit(bs ...byte) { g.code = append(g.code, bs...) }

func (g *Gen) u32(v uint32) {
	g.code = append(g.code, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}
func (g *Gen) u64(v uint64) {
	for i := 0; i < 8; i++ {
		g.code = append(g.code, byte(v>>(8*i)))
	}
}

func (g *Gen) mark(name string) { g.labels[name] = len(g.code) }

func (g *Gen) newLbl(prefix string) string {
	g.lblN++
	return prefix + strconv.Itoa(g.lblN)
}

// jmp/jcc/call с rel32 фиксапом
func (g *Gen) jump(op []byte, target string) {
	g.emit(op...)
	g.fix = append(g.fix, fixup{len(g.code), 'r', target})
	g.u32(0)
}
func (g *Gen) jmp(target string)  { g.jump([]byte{0xE9}, target) }
func (g *Gen) jz(target string)   { g.jump([]byte{0x0F, 0x84}, target) }
func (g *Gen) jnz(target string)  { g.jump([]byte{0x0F, 0x85}, target) }
func (g *Gen) call(target string) { g.jump([]byte{0xE8}, target) }

// mov rax, <адрес данных> (abs64)
func (g *Gen) movRaxDataAbs(target string) {
	g.emit(0x48, 0xB8)
	g.fix = append(g.fix, fixup{len(g.code), 'a', target})
	g.u64(0)
}

// Строка в данных: 8 байт длины + байты (одинаковые строки хранятся один раз).
func (g *Gen) addString(s string) string {
	if lbl, ok := g.strLbl[s]; ok {
		return lbl
	}
	lbl := "str" + strconv.Itoa(g.strCnt)
	g.strCnt++
	g.strLbl[s] = lbl
	g.dlabels[lbl] = len(g.data)
	n := uint64(len(s))
	for i := 0; i < 8; i++ {
		g.data = append(g.data, byte(n>>(8*i)))
	}
	g.data = append(g.data, []byte(s)...)
	g.data = append(g.data, 0) // NUL после текста: p+8 годится как C-строка
	return lbl
}

func (g *Gen) addDataQword(name string, v uint64) {
	g.dlabels[name] = len(g.data)
	g.u64data(v)
}
func (g *Gen) u64data(v uint64) {
	for i := 0; i < 8; i++ {
		g.data = append(g.data, byte(v>>(8*i)))
	}
}

func (g *Gen) movImm(v int64) {
	if v >= 0 && v <= 0x7fffffff {
		g.emit(0xB8) // mov eax, imm32 (обнуляет верх rax)
		g.u32(uint32(v))
		return
	}
	g.emit(0x48, 0xB8) // mov rax, imm64
	g.u64(uint64(v))
}

// alloc(n) -> ptr<u8>: mmap(0, n, RW, PRIVATE|ANON, -1, 0), только для обычных программ
func (g *Gen) stubAlloc() {
	g.mark("_alloc")
	g.emit(0x48, 0x89, 0xFE)                   // mov rsi, rdi
	g.emit(0x31, 0xFF)                         // xor edi, edi
	g.emit(0xBA, 0x03, 0, 0, 0)                // mov edx, 3
	g.emit(0x41, 0xBA, 0x22, 0, 0, 0)          // mov r10d, 0x22
	g.emit(0x49, 0xC7, 0xC0, 0xFF, 0xFF, 0xFF, 0xFF) // mov r8, -1
	g.emit(0x45, 0x31, 0xC9)                   // xor r9d, r9d
	g.emit(0xB8, 0x09, 0, 0, 0)                // mov eax, 9 (mmap)
	g.emit(0x0F, 0x05)                         // syscall
	g.emit(0xC3)
}

// ---------- Компиляция программы ----------

var builtinNames = map[string]bool{
	"print": true, "puti": true, "exit": true, "alloc": true, "make": true,
	// инструкции железа: только $kernel / $boot
	"inb": true, "inw": true, "inl": true, "outb": true, "outw": true, "outl": true,
	"cli": true, "sti": true, "hlt": true, "pause": true, "rdtsc": true,
	"readcr0": true, "readcr2": true, "readcr3": true, "readcr4": true,
	"writecr0": true, "writecr3": true, "writecr4": true,
	"rdmsr": true, "wrmsr": true, "lidt": true, "lgdt": true, "invlpg": true,
	// есть везде
	"fnaddr": true, "callfn": true,
	// только обычные программы
	"syscall": true, "argc": true, "argv": true,
}

var hwBuiltins = map[string]bool{
	"inb": true, "inw": true, "inl": true, "outb": true, "outw": true, "outl": true,
	"cli": true, "sti": true, "hlt": true, "pause": true, "rdtsc": true,
	"readcr0": true, "readcr2": true, "readcr3": true, "readcr4": true,
	"writecr0": true, "writecr3": true, "writecr4": true,
	"rdmsr": true, "wrmsr": true, "lidt": true, "lgdt": true, "invlpg": true,
}

var hostBuiltins = map[string]bool{"syscall": true, "argc": true, "argv": true, "alloc": true, "make": true}

func (g *Gen) allocBss(name string, size int) {
	g.bssSize = (g.bssSize + 15) &^ 15
	g.bssLabels[name] = g.bssSize
	g.bssSize += size
}

// адрес глобальной переменной -> rax
func (g *Gen) globalAddr(gl *Global) {
	g.emit(0x48, 0xB8)
	kind := byte('a')
	if gl.InBss {
		kind = 'B'
	}
	g.fix = append(g.fix, fixup{len(g.code), kind, gl.Label})
	g.u64(0)
}

// фиксап особого вида (адрес bss, вершина стека, размер bss, метка кода)
func (g *Gen) special64(kind byte, target string) {
	g.fix = append(g.fix, fixup{len(g.code), kind, target})
	g.u64(0)
}

func (g *Gen) findGlobal(name string) *Global {
	pr := g.cur.Prog
	if gl, ok := pr.Globals[name]; ok {
		return gl
	}
	var found *Global
	for _, ip := range pr.Alias {
		if gl, ok := ip.Globals[name]; ok {
			if found != nil && found != gl {
				g.fail("глобальная %s есть в нескольких импортах", name)
			}
			found = gl
		}
	}
	return found
}

func (g *Gen) fnLabel(f *Func) string { return "f:" + f.Prog.ID + "." + f.Name }

// Compile: progs[0] — главный файл, остальные — его импорты.
func Compile(progs []*Program) *Gen {
	mainProg := progs[0]
	g := NewGen(mainProg.Mode)
	if mainProg.Target != "" {
		g.target = mainProg.Target
	}
	if g.target != "hosted" && mainProg.Mode == "hight" {
		panic(mainProg.Path + ": $" + g.target + " нельзя вместе с $hight: программа $hight всегда запускается на обычном ПК. Используй $mid или $low")
	}
	if g.target == "boot" && mainProg.Mode != "low" {
		panic(mainProg.Path + ": $boot нужен $low: загрузчиком может быть только низкоуровневый код")
	}
	for _, pr := range progs {
		for _, f := range pr.Funcs {
			if builtinNames[f.Name] {
				g.cur, g.line, g.col = f, f.Line, 0
				g.fail("имя функции %s занято встроенной функцией", f.Name)
			}
		}
	}
	for _, pr := range progs {
		g.cur = &Func{File: pr.Path, Prog: pr}
		names := make([]string, 0, len(pr.Structs))
		for n := range pr.Structs {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			d := pr.Structs[n]
			g.line, g.col = d.Line, 0
			for _, f := range d.Fields {
				g.checkValueType(f.Typ, "поле "+d.Name+"."+f.Name)
				if f.Typ.Name == "void" {
					g.fail("поле %s.%s не может быть void", d.Name, f.Name)
				}
			}
		}
	}
	// глобальные переменные: скаляры в data (с начальным значением), массивы в bss (нули)
	for _, pr := range progs {
		g.cur = &Func{File: pr.Path, Prog: pr}
		gnames := make([]string, 0, len(pr.Globals))
		for n := range pr.Globals {
			gnames = append(gnames, n)
		}
		sort.Strings(gnames)
		for _, n := range gnames {
			gl := pr.Globals[n]
			g.line, g.col = gl.Line, 0
			gl.Label = "g:" + pr.ID + "." + gl.Name
			if gl.Arr > 0 {
				g.checkType(gl.Typ)
				if gl.Typ.Name == "void" {
					g.fail("массив из void невозможен")
				}
				gl.InBss = true
				sz := gl.Arr * gl.Typ.size()
				if sz < 1 {
					sz = 1
				}
				g.allocBss(gl.Label, sz)
			} else {
				g.checkValueType(gl.Typ, "глобальная "+gl.Name)
				if gl.Typ.Name == "void" {
					g.fail("глобальная переменная не может быть void")
				}
				if gl.Typ.isInt() && !fitsInt(gl.Typ, gl.Init) {
					g.fail("значение %d не помещается в %s", gl.Init, gl.Typ)
				}
				g.addDataQword(gl.Label, uint64(gl.Init))
			}
		}
	}
	mf := mainProg.byName["main"]
	if mf == nil {
		panic(mainProg.Path + ": нет функции main")
	}
	g.cur, g.line, g.col = mf, mf.Line, 0
	if len(mf.Params) != 0 {
		g.fail("main не принимает параметров")
	}
	if mf.Ret.Name != "i64" && mf.Ret.Name != "void" {
		g.fail("main должна возвращать i64 или ничего")
	}

	// _start: подготовка окружения, вызов main, выход
	g.mark("_start")
	switch g.target {
	case "hosted":
		g.allocBss("__sp0", 8)
		g.emit(0x48, 0xB8) // mov rax, &__sp0
		g.special64('B', "__sp0")
		g.emit(0x48, 0x89, 0x20) // mov [rax], rsp  (argc/argv лежат на стеке запуска)
	case "kernel", "boot":
		g.emit(0xFC)       // cld
		g.emit(0x48, 0xBC) // mov rsp, <вершина стека за образом>
		g.special64('S', "")
		if g.target == "boot" { // загрузчик сам зануляет bss
			g.emit(0x48, 0xBF) // mov rdi, <начало bss>
			g.special64('G', "")
			g.emit(0xB9) // mov ecx, <размер bss>
			g.fix = append(g.fix, fixup{len(g.code), 'Z', ""})
			g.u32(0)
			g.emit(0x31, 0xC0) // xor eax, eax
			g.emit(0xF3, 0xAA) // rep stosb
		}
	}
	g.call(g.fnLabel(mf))
	if g.target == "hosted" {
		g.emit(0x48, 0x89, 0xC7)    // mov rdi, rax (код возврата main)
		g.emit(0xB8, 0x3C, 0, 0, 0) // mov eax, 60
		g.emit(0x0F, 0x05)          // syscall
		g.stubPrintHight()
		g.stubPutiHight()
		g.stubExitHight()
		g.stubAlloc()
	} else {
		g.emit(0xFA)       // cli
		g.emit(0xF4)       // hlt
		g.emit(0xEB, 0xFD) // jmp hlt
		g.stubPrintLow()   // вывод в VGA 0xB8000
		g.stubPutiLow()
		g.stubExitLow()
	}

	for _, pr := range progs {
		for _, f := range pr.Funcs {
			g.genFunc(f)
		}
	}
	return g
}

func (g *Gen) pushScope() { g.scopes = append(g.scopes, map[string]*local{}) }
func (g *Gen) popScope()  { g.scopes = g.scopes[:len(g.scopes)-1] }

func (g *Gen) declare(name string, t *Type, mut bool) *local {
	if keywords[name] {
		g.fail("%q — ключевое слово", name)
	}
	sc := g.scopes[len(g.scopes)-1]
	if _, dup := sc[name]; dup {
		g.fail("%q уже объявлена в этой области", name)
	}
	g.frame += 8
	l := &local{off: -g.frame, typ: t, mut: mut}
	sc[name] = l
	return l
}

func (g *Gen) lookup(name string) *local {
	for i := len(g.scopes) - 1; i >= 0; i-- {
		if l, ok := g.scopes[i][name]; ok {
			return l
		}
	}
	return nil
}

func (g *Gen) loadLocal(l *local) {
	g.emit(0x48, 0x8B, 0x85) // mov rax, [rbp+disp32]
	g.u32(uint32(int32(l.off)))
}

func (g *Gen) storeLocal(l *local) {
	g.emit(0x48, 0x89, 0x85) // mov [rbp+disp32], rax
	g.u32(uint32(int32(l.off)))
}

// lookupConst ищет константу в текущем файле, затем в его импортах.
func (g *Gen) lookupConst(name string) (int64, bool) {
	pr := g.cur.Prog
	if v, ok := pr.Consts[name]; ok {
		return v, true
	}
	var val int64
	found := false
	for _, ip := range pr.Alias {
		if v, ok := ip.Consts[name]; ok {
			if found && v != val {
				g.fail("константа %s есть в нескольких импортах", name)
			}
			val, found = v, true
		}
	}
	return val, found
}

// findFunc: pkg.name — в импорте с таким псевдонимом; name — свой файл, затем импорты.
func (g *Gen) findFunc(pkg, name string) *Func {
	pr := g.cur.Prog
	if pkg != "" {
		ip, ok := pr.Alias[pkg]
		if !ok {
			g.fail("пакет %q не импортирован", pkg)
		}
		f := ip.byName[name]
		if f == nil {
			g.fail("в пакете %s нет функции %s", pkg, name)
		}
		return f
	}
	if f, ok := pr.byName[name]; ok {
		return f
	}
	var found *Func
	for _, ip := range pr.Alias {
		if f, ok := ip.byName[name]; ok {
			if found != nil && found != f {
				g.fail("имя %s есть в нескольких импортах: пиши пакет.%s(...)", name, name)
			}
			found = f
		}
	}
	return found
}

// checkType: все имена структур в типе должны существовать
func (g *Gen) checkType(t *Type) {
	switch t.Name {
	case "ptr":
		g.checkType(t.Elem)
	case "struct":
		if t.def() == nil {
			g.fail("неизвестный тип %s", t)
		}
	}
}

// checkValueType: тип переменной/параметра/результата — структуры только через ptr<Имя>
func (g *Gen) checkValueType(t *Type, what string) {
	g.checkType(t)
	if t.Name == "struct" {
		g.fail("структуру %s нельзя использовать по значению (%s): пиши ptr<%s>", t, what, t)
	}
}

// genStructAddr: rax = адрес структуры, на которую указывает выражение x
func (g *Gen) genStructAddr(x Expr) *StructDef {
	if ix, ok := x.(*IndexE); ok {
		pt := g.genIndexAddr(ix) // rax = адрес элемента
		switch {
		case pt.Elem.Name == "struct":
			return pt.Elem.def()
		case pt.Elem.isPtr() && pt.Elem.Elem.Name == "struct":
			g.loadElem(pt.Elem)
			return pt.Elem.Elem.def()
		}
		g.fail("у элемента %s нет полей", pt.Elem)
	}
	t := g.genExpr(x)
	if t.isPtr() && t.Elem.Name == "struct" {
		return t.Elem.def()
	}
	g.fail("поля есть только у ptr<структура>, а тут %s", t)
	return nil
}

func (g *Gen) structNamed(name string) *StructDef {
	if _, isConst := g.lookupConst(name); isConst {
		return nil
	}
	return lookupStruct(g.cur.Prog, "", name)
}

func (g *Gen) addOff(off int) {
	if off != 0 {
		g.emit(0x48, 0x05) // add rax, imm32
		g.u32(uint32(off))
	}
}

// небезопасные операции (ptr <-> число, арифметика указателей) запрещены в $hight
func (g *Gen) needUnsafe(what string) {
	if g.mode == "hight" && !g.cur.Prog.Trusted {
		g.fail("%s запрещено в $hight (нужен $mid или $low)", what)
	}
}

// Вход обработчика прерывания: сохраняет все регистры, вызывает тело, iretq.
// Для @exception код ошибки (лежит на стеке) передаётся первым параметром и снимается перед iretq.
func (g *Gen) emitISRStub(entry string, exc bool) {
	g.mark(entry)
	g.emit(0x50, 0x51, 0x52, 0x53, 0x55, 0x56, 0x57) // push rax rcx rdx rbx rbp rsi rdi
	for b := byte(0x50); b <= 0x57; b++ {
		g.emit(0x41, b) // push r8..r15
	}
	if exc {
		g.emit(0x48, 0x8B, 0x7C, 0x24, 0x78) // mov rdi, [rsp+120] (код ошибки)
	}
	g.emit(0xFC) // cld
	g.call(entry + "$b")
	for b := byte(0x5F); b >= 0x58; b-- {
		g.emit(0x41, b) // pop r15..r8
	}
	g.emit(0x5F, 0x5E, 0x5D, 0x5B, 0x5A, 0x59, 0x58) // pop rdi rsi rbp rbx rdx rcx rax
	if exc {
		g.emit(0x48, 0x83, 0xC4, 0x08) // add rsp, 8
	}
	g.emit(0x48, 0xCF) // iretq
}

func (g *Gen) genFunc(f *Func) {
	g.cur = f
	g.line, g.col = f.Line, 0
	isISR := f.hasAttr("interrupt") || f.hasAttr("exception")
	isExc := f.hasAttr("exception")
	if isISR {
		if g.target == "hosted" {
			g.fail("@interrupt/@exception есть только в $kernel и $boot")
		}
		if f.hasAttr("interrupt") && f.hasAttr("exception") {
			g.fail("нужен один атрибут: @interrupt или @exception")
		}
		if (!isExc && len(f.Params) != 0) || (isExc && len(f.Params) != 1) {
			g.fail("@interrupt без параметров, @exception — один параметр (код ошибки)")
		}
		g.emitISRStub(g.fnLabel(f), isExc)
		g.mark(g.fnLabel(f) + "$b")
	} else {
		g.mark(g.fnLabel(f))
	}
	g.scopes = []map[string]*local{{}}
	g.frame = 0
	g.brkLbl, g.cntLbl = nil, nil

	g.emit(0x55)             // push rbp
	g.emit(0x48, 0x89, 0xE5) // mov rbp, rsp
	g.emit(0x48, 0x81, 0xEC) // sub rsp, imm32 (патчим потом)
	framePos := len(g.code)
	g.u32(0)

	// параметры приходят в rdi, rsi, rdx, rcx, r8, r9
	regStore := [][]byte{
		{0x48, 0x89, 0xBD}, {0x48, 0x89, 0xB5}, {0x48, 0x89, 0x95},
		{0x48, 0x89, 0x8D}, {0x4C, 0x89, 0x85}, {0x4C, 0x89, 0x8D},
	}
	g.checkValueType(f.Ret, "результат "+f.Name)
	for _, prm := range f.Params {
		g.checkValueType(prm.Typ, "параметр "+prm.Name)
	}
	for i, prm := range f.Params {
		if _, dup := g.scopes[0][prm.Name]; dup {
			g.fail("параметр %s указан дважды", prm.Name)
		}
		if i >= 6 { // 7-й и дальше параметры уже лежат на стеке вызывающего
			g.scopes[0][prm.Name] = &local{off: 16 + 8*(i-6), typ: prm.Typ}
			continue
		}
		g.frame += 8
		g.scopes[0][prm.Name] = &local{off: -g.frame, typ: prm.Typ}
		g.emit(regStore[i]...)
		g.u32(uint32(int32(-g.frame)))
	}

	retLbl := g.newLbl("ret_")
	g.genStmts(f.Body, retLbl)
	g.emit(0x31, 0xC0) // xor eax, eax: дошли до конца без return -> 0
	g.mark(retLbl)
	g.emit(0x48, 0x89, 0xEC) // mov rsp, rbp
	g.emit(0x5D)             // pop rbp
	g.emit(0xC3)             // ret

	n := uint32(g.frame)
	g.code[framePos+0] = byte(n)
	g.code[framePos+1] = byte(n >> 8)
	g.code[framePos+2] = byte(n >> 16)
	g.code[framePos+3] = byte(n >> 24)
}

// ---------- Стейтменты ----------

func (g *Gen) genStmts(ss []Stmt, retLbl string) {
	for _, s := range ss {
		g.genStmt(s, retLbl)
	}
}

func (g *Gen) genScoped(ss []Stmt, retLbl string) {
	g.pushScope()
	g.genStmts(ss, retLbl)
	g.popScope()
}

// cond: вычислить bool и выставить флаги для jz/jnz
func (g *Gen) cond(e Expr, what string) {
	t := g.genExpr(e)
	if t.Name != "bool" {
		g.fail("%s должно быть bool, а не %s (сравни явно, например x != 0)", what, t)
	}
	g.emit(0x48, 0x85, 0xC0) // test rax, rax
}

// exprTo: вычислить e и проверить, что оно подходит к типу want
func (g *Gen) constValue(e Expr) (int64, bool) {
	switch x := e.(type) {
	case *NumE:
		return x.Val, true
	case *UnE:
		if x.Op == "-" || x.Op == "~" {
			v, ok := g.constValue(x.X)
			if !ok {
				return 0, false
			}
			if x.Op == "-" {
				return -v, true
			}
			return ^v, true
		}
	case *VarE:
		if g.lookup(x.Name) != nil || g.findGlobal(x.Name) != nil {
			return 0, false
		}
		return g.lookupConst(x.Name)
	case *BinE:
		a, ok1 := g.constValue(x.L)
		b, ok2 := g.constValue(x.R)
		if ok1 && ok2 {
			return constEval(&BinE{x.Op, &NumE{a}, &NumE{b}}, nil)
		}
	}
	return 0, false
}

func (g *Gen) exprTo(e Expr, want *Type, what string) {
	if want.isInt() {
		if v, ok := g.constValue(e); ok {
			if !fitsInt(want, v) {
				g.fail("константа %d не помещается в %s (%s)", v, want, what)
			}
			g.movImm(v)
			return
		}
	}
	have := g.genExpr(e)
	if have.Name == "void" {
		g.fail("%s: у выражения нет значения", what)
	}
	if !assignable(want, have) {
		g.fail("нельзя использовать %s как %s (%s)", have, want, what)
	}
}

func (g *Gen) genStmt(s Stmt, retLbl string) {
	g.line = s.Ln()
	g.col = s.Cn()
	switch st := s.(type) {
	case *LetS:
		var t *Type
		if st.Typ != nil {
			if st.Typ.Name == "void" {
				g.fail("переменная не может быть void")
			}
			g.checkValueType(st.Typ, "переменная "+st.Name)
			g.exprTo(st.Init, st.Typ, "переменная "+st.Name)
			t = st.Typ
		} else {
			t = g.genExpr(st.Init)
			if t.Name == "void" {
				g.fail("у выражения нет значения")
			}
			if t.Name == "nil" {
				g.fail("тип не выводится из nil, напиши: var p: ptr<T> = nil")
			}
		}
		l := g.declare(st.Name, t, st.Mut)
		g.storeLocal(l)
	case *AssignS:
		l := g.lookup(st.Name)
		if l == nil {
			if gl := g.findGlobal(st.Name); gl != nil {
				if gl.Arr > 0 {
					g.fail("%s — статический массив: пиши %s[i] = ...", st.Name, st.Name)
				}
				if !gl.Mut {
					g.fail("глобальная %s объявлена через let; для изменения объяви через var", st.Name)
				}
				g.exprTo(st.X, gl.Typ, "присваивание "+st.Name)
				g.emit(0x48, 0x89, 0xC3) // mov rbx, rax
				g.globalAddr(gl)
				g.storeRbx(gl.Typ)
				break
			}
			if _, isConst := g.lookupConst(st.Name); isConst {
				g.fail("%s — константа, её нельзя менять", st.Name)
			}
			g.fail("присваивание неизвестной переменной: %s", st.Name)
		}
		if !l.mut {
			g.fail("%s объявлена через let (неизменяемая); для изменения объяви через var", st.Name)
		}
		g.exprTo(st.X, l.typ, "присваивание "+st.Name)
		g.storeLocal(l)
	case *DAssignS:
		pt := g.genExpr(st.Ptr)
		if !pt.isPtr() || pt.Elem.Name == "struct" {
			g.fail("записывать через deref можно только в ptr на число/указатель, а тут %s", pt)
		}
		g.emit(0x50) // push rax (адрес)
		g.exprTo(st.X, pt.Elem, "запись через указатель")
		g.storePop(pt.Elem)
	case *FAssignS:
		def := g.genStructAddr(st.X.X)
		f := def.field(st.X.Name)
		if f == nil {
			g.fail("у структуры %s нет поля %s", def.Name, st.X.Name)
		}
		g.addOff(f.Off)
		g.emit(0x50) // push rax (адрес поля)
		g.exprTo(st.V, f.Typ, "запись в поле "+def.Name+"."+f.Name)
		g.storePop(f.Typ)
	case *IAssignS:
		pt := g.genIndexAddr(st.X)
		if pt.Elem.Name == "struct" {
			g.fail("структуру целиком присвоить нельзя: пиши p[i].поле = значение")
		}
		g.emit(0x50) // push rax (адрес элемента)
		g.exprTo(st.V, pt.Elem, "запись по индексу")
		g.storePop(pt.Elem)
	case *ExprS:
		g.genExpr(st.X)
	case *RetS:
		ret := g.cur.Ret
		if st.X == nil {
			if ret.Name != "void" && ret.Name != "i64" {
				g.fail("нужно вернуть значение типа %s", ret)
			}
			g.emit(0x31, 0xC0)
		} else {
			if ret.Name == "void" {
				g.fail("функция %s ничего не возвращает", g.cur.Name)
			}
			g.exprTo(st.X, ret, "return")
		}
		g.jmp(retLbl)
	case *IfS:
		lElse := g.newLbl("else_")
		lEnd := g.newLbl("endif_")
		g.cond(st.Cond, "условие if")
		g.jz(lElse)
		g.genScoped(st.Then, retLbl)
		g.jmp(lEnd)
		g.mark(lElse)
		g.genScoped(st.Else, retLbl)
		g.mark(lEnd)
	case *WhileS:
		lTop := g.newLbl("while_")
		lEnd := g.newLbl("wend_")
		g.brkLbl = append(g.brkLbl, lEnd)
		g.cntLbl = append(g.cntLbl, lTop)
		g.mark(lTop)
		g.cond(st.Cond, "условие while")
		g.jz(lEnd)
		g.genScoped(st.Body, retLbl)
		g.jmp(lTop)
		g.mark(lEnd)
		g.brkLbl = g.brkLbl[:len(g.brkLbl)-1]
		g.cntLbl = g.cntLbl[:len(g.cntLbl)-1]
	case *ForS:
		lTop := g.newLbl("for_")
		lPost := g.newLbl("fpost_")
		lEnd := g.newLbl("fend_")
		g.pushScope()
		if ls, ok := st.Init.(*LetS); ok {
			ls.Mut = true // переменная цикла всегда изменяемая
		}
		g.genStmt(st.Init, retLbl)
		g.brkLbl = append(g.brkLbl, lEnd)
		g.cntLbl = append(g.cntLbl, lPost)
		g.mark(lTop)
		g.line = st.Ln()
		g.cond(st.Cond, "условие for")
		g.jz(lEnd)
		g.genScoped(st.Body, retLbl)
		g.mark(lPost)
		g.genStmt(st.Post, retLbl)
		g.jmp(lTop)
		g.mark(lEnd)
		g.brkLbl = g.brkLbl[:len(g.brkLbl)-1]
		g.cntLbl = g.cntLbl[:len(g.cntLbl)-1]
		g.popScope()
	case *BreakS:
		if len(g.brkLbl) == 0 {
			g.fail("break вне цикла")
		}
		g.jmp(g.brkLbl[len(g.brkLbl)-1])
	case *ContS:
		if len(g.cntLbl) == 0 {
			g.fail("continue вне цикла")
		}
		g.jmp(g.cntLbl[len(g.cntLbl)-1])
	case *AsmS:
		if g.mode != "low" {
			g.fail("asm доступен только в $low")
		}
		g.emit(st.Code...)
	default:
		g.fail("неизвестный оператор %T", s)
	}
}

// ---------- Выражения (результат в rax) ----------

func (g *Gen) scaleReg(rbx bool, elem *Type) {
	n := elem.size()
	if n <= 1 {
		return
	}
	if rbx {
		g.emit(0x48, 0x69, 0xDB) // imul rbx, rbx, imm32
	} else {
		g.emit(0x48, 0x69, 0xC0) // imul rax, rax, imm32
	}
	g.u32(uint32(n))
}

// genIndexAddr: rax = адрес p[i]; возвращает тип p (ptr<T>). Индексация безопасна в любом
// режиме (границы не проверяются), поэтому needUnsafe здесь не нужен.
func (g *Gen) genIndexAddr(ix *IndexE) *Type {
	pt := g.genExpr(ix.X)
	if !pt.isPtr() {
		g.fail("индексировать можно только ptr<T>, а тут %s", pt)
	}
	g.emit(0x50) // push rax (база)
	it := g.genExpr(ix.I)
	if !it.isInt() {
		g.fail("индекс должен быть числом, а не %s", it)
	}
	g.scaleReg(false, pt.Elem) // rax = i * sizeof(T)
	g.emit(0x5B)               // pop rbx
	g.emit(0x48, 0x01, 0xD8)   // add rax, rbx
	return pt
}

// loadElem: rax = *(T*)rax (с расширением до 64 бит)
func (g *Gen) loadElem(t *Type) {
	switch {
	case t.Name == "i32":
		g.emit(0x48, 0x63, 0x00) // movsxd rax, dword [rax]
	case t.size() == 1:
		g.emit(0x48, 0x0F, 0xB6, 0x00) // movzx rax, byte [rax]
	case t.size() == 2:
		g.emit(0x48, 0x0F, 0xB7, 0x00) // movzx rax, word [rax]
	case t.size() == 4:
		g.emit(0x8B, 0x00) // mov eax, [rax]
	default:
		g.emit(0x48, 0x8B, 0x00) // mov rax, [rax]
	}
}

// storeRbx: *(T*)rax = rbx
func (g *Gen) storeRbx(t *Type) {
	switch t.size() {
	case 1:
		g.emit(0x88, 0x18) // mov [rax], bl
	case 2:
		g.emit(0x66, 0x89, 0x18) // mov [rax], bx
	case 4:
		g.emit(0x89, 0x18) // mov [rax], ebx
	default:
		g.emit(0x48, 0x89, 0x18) // mov [rax], rbx
	}
}

// storePop: значение в rax, адрес лежит на стеке -> записать по адресу
func (g *Gen) storePop(t *Type) {
	g.emit(0x48, 0x89, 0xC3) // mov rbx, rax
	g.emit(0x58)             // pop rax
	g.storeRbx(t)
}

func ptrCompat(a, b *Type) bool {
	switch {
	case a.isPtr() && b.isPtr():
		return sameType(a, b)
	case a.isPtr() && b.Name == "nil", a.Name == "nil" && b.isPtr():
		return true
	}
	return false
}

func (g *Gen) genExpr(e Expr) *Type {
	switch ex := e.(type) {
	case *NumE:
		g.movImm(ex.Val)
		return tI64
	case *BoolE:
		if ex.Val {
			g.movImm(1)
		} else {
			g.movImm(0)
		}
		return tBool
	case *NilE:
		g.emit(0x31, 0xC0) // xor eax, eax
		return tNil
	case *StrE:
		g.movRaxDataAbs(g.addString(ex.Val))
		return tStr
	case *VarE:
		if l := g.lookup(ex.Name); l != nil {
			g.loadLocal(l)
			return l.typ
		}
		if gl := g.findGlobal(ex.Name); gl != nil {
			g.globalAddr(gl)
			if gl.Arr > 0 {
				return ptrTo(gl.Typ) // статический массив: значение — указатель на первый элемент
			}
			g.loadElem(gl.Typ)
			return gl.Typ
		}
		if v, ok := g.lookupConst(ex.Name); ok {
			g.movImm(v)
			return tI64
		}
		g.fail("неизвестная переменная: %s", ex.Name)
	case *CastE:
		return g.genCast(ex)
	case *FieldE:
		def := g.genStructAddr(ex.X)
		f := def.field(ex.Name)
		if f == nil {
			g.fail("у структуры %s нет поля %s", def.Name, ex.Name)
		}
		g.addOff(f.Off)
		g.loadElem(f.Typ)
		return f.Typ
	case *IndexE:
		pt := g.genIndexAddr(ex)
		if pt.Elem.Name == "struct" {
			g.fail("элемент-структуру целиком брать нельзя: используй p[i].поле")
		}
		g.loadElem(pt.Elem)
		return pt.Elem
	case *MakeE:
		if g.target != "hosted" {
			g.fail("make есть только в обычных программах (не в $kernel/$boot)")
		}
		if ex.T.Name == "void" {
			g.fail("make(void, n) не имеет смысла")
		}
		g.checkType(ex.T)
		nt := g.genExpr(ex.N)
		if !nt.isInt() {
			g.fail("размер make должен быть числом, а не %s", nt)
		}
		g.scaleReg(false, ex.T)       // rax = n * sizeof(T)
		g.emit(0x48, 0x89, 0xC7)      // mov rdi, rax
		g.call("_alloc")
		return ptrTo(ex.T)
	case *SizeofE:
		var t *Type
		if ex.T != nil {
			g.checkType(ex.T)
			t = ex.T
		} else if v, ok := ex.X.(*VarE); ok && g.lookup(v.Name) == nil && g.structNamed(v.Name) != nil {
			t = &Type{Name: "struct", Ref: v.Name, Prog: g.cur.Prog}
		} else {
			// тип выражения без генерации кода: сгенерировать и откатить
			save, nfix := len(g.code), len(g.fix)
			t = g.genExpr(ex.X)
			g.code = g.code[:save]
			g.fix = g.fix[:nfix]
		}
		g.movImm(int64(t.size()))
		return tI64
	case *UnE:
		return g.genUnary(ex)
	case *BinE:
		return g.genBin(ex)
	case *CallE:
		return g.genCallExpr(ex)
	}
	g.fail("неизвестное выражение %T", e)
	return nil
}

func (g *Gen) genCast(ex *CastE) *Type {
	g.checkValueType(ex.T, "приведение")
	from := g.genExpr(ex.X)
	to := ex.T
	switch {
	case to.isInt():
		if from.isPtr() {
			g.needUnsafe("приведение ptr к числу")
		} else if !from.isInt() && from.Name != "bool" {
			g.fail("нельзя привести %s к %s", from, to)
		}
		switch to.Name {
		case "u8":
			g.emit(0x0F, 0xB6, 0xC0) // movzx eax, al
		case "u16":
			g.emit(0x0F, 0xB7, 0xC0) // movzx eax, ax
		case "u32":
			g.emit(0x89, 0xC0) // mov eax, eax
		case "i32":
			g.emit(0x48, 0x63, 0xC0) // movsxd rax, eax
		}
	case to.isPtr():
		switch {
		case from.Name == "nil":
		case from.isPtr() && sameType(from, to):
		case from.isInt() || from.isPtr():
			g.needUnsafe("приведение к ptr")
		default:
			g.fail("нельзя привести %s к %s", from, to)
		}
	case to.Name == "bool":
		g.fail("в bool приводить нельзя, сравни явно: x != 0")
	default:
		g.fail("нельзя привести %s к %s", from, to)
	}
	return to
}

func (g *Gen) genUnary(ex *UnE) *Type {
	switch ex.Op {
	case "-":
		t := g.genExpr(ex.X)
		if !t.isInt() {
			g.fail("нельзя взять минус от %s", t)
		}
		g.emit(0x48, 0xF7, 0xD8) // neg rax
		return tI64
	case "~":
		t := g.genExpr(ex.X)
		if !t.isInt() {
			g.fail("'~' применяется к числу, а тут %s", t)
		}
		g.emit(0x48, 0xF7, 0xD0) // not rax
		if t.isU64() {
			return tU64
		}
		return tI64
	case "!":
		t := g.genExpr(ex.X)
		if t.Name != "bool" {
			g.fail("'!' применяется к bool, а тут %s", t)
		}
		g.emit(0x48, 0x85, 0xC0) // test rax, rax
		g.emit(0x0F, 0x94, 0xC0) // sete al
		g.emit(0x48, 0x0F, 0xB6, 0xC0)
		return tBool
	case "addr":
		v, ok := ex.X.(*VarE)
		if !ok {
			g.fail("addr берёт адрес только переменной")
		}
		l := g.lookup(v.Name)
		if l == nil {
			if gl := g.findGlobal(v.Name); gl != nil && gl.Arr == 0 {
				if !gl.Mut {
					g.fail("addr(%s): глобальная должна быть объявлена через var", v.Name)
				}
				g.globalAddr(gl)
				return ptrTo(gl.Typ)
			}
			g.fail("неизвестная переменная: %s", v.Name)
		}
		if !l.mut {
			g.fail("addr(%s): переменная должна быть объявлена через var", v.Name)
		}
		g.emit(0x48, 0x8D, 0x85) // lea rax, [rbp+disp32]
		g.u32(uint32(int32(l.off)))
		return ptrTo(l.typ)
	case "deref":
		t := g.genExpr(ex.X)
		if !t.isPtr() {
			g.fail("deref применяется к ptr, а тут %s", t)
		}
		if t.Elem.Name == "struct" {
			g.fail("нельзя разыменовать %s целиком: используй p.поле или p[i].поле", t)
		}
		if t.Elem.Name == "void" {
			g.fail("нельзя разыменовать %s", t)
		}
		g.loadElem(t.Elem)
		return t.Elem
	}
	g.fail("неизвестный унарный оператор %s", ex.Op)
	return nil
}

func intResult(lt, rt *Type) *Type {
	if lt.isU64() || rt.isU64() {
		return tU64
	}
	return tI64
}

func (g *Gen) genBin(ex *BinE) *Type {
	if ex.Op == "&&" || ex.Op == "||" {
		end := g.newLbl("sc_")
		lt := g.genExpr(ex.L)
		if lt.Name != "bool" {
			g.fail("'%s' работает с bool, а тут %s", ex.Op, lt)
		}
		g.emit(0x48, 0x85, 0xC0) // test rax, rax
		if ex.Op == "&&" {
			g.jz(end) // левая ложь: результат уже 0
		} else {
			g.jnz(end) // левая истина: результат уже 1
		}
		rt := g.genExpr(ex.R)
		if rt.Name != "bool" {
			g.fail("'%s' работает с bool, а тут %s", ex.Op, rt)
		}
		g.mark(end)
		return tBool
	}

	lt := g.genExpr(ex.L)
	g.emit(0x50) // push rax
	rt := g.genExpr(ex.R)
	g.emit(0x48, 0x89, 0xC3) // mov rbx, rax
	g.emit(0x58)             // pop rax  (rax=L, rbx=R)

	bad := func() {
		g.fail("нельзя применить %s к %s и %s", ex.Op, lt, rt)
	}
	switch ex.Op {
	case "+", "-":
		switch {
		case lt.isInt() && rt.isInt():
		case lt.isPtr() && rt.isInt():
			g.needUnsafe("арифметика с указателями")
			g.scaleReg(true, lt.Elem)
		case ex.Op == "+" && lt.isInt() && rt.isPtr():
			g.needUnsafe("арифметика с указателями")
			g.scaleReg(false, rt.Elem)
		default:
			bad()
		}
		if ex.Op == "+" {
			g.emit(0x48, 0x01, 0xD8) // add rax, rbx
		} else {
			g.emit(0x48, 0x29, 0xD8) // sub rax, rbx
		}
		switch {
		case lt.isPtr():
			return lt
		case rt.isPtr():
			return rt
		}
		return intResult(lt, rt)
	case "*", "/", "%", "&", "|", "^":
		if !lt.isInt() || !rt.isInt() {
			bad()
		}
		unsigned := lt.isU64() || rt.isU64()
		switch ex.Op {
		case "*":
			g.emit(0x48, 0x0F, 0xAF, 0xC3) // imul rax, rbx
		case "/", "%":
			if unsigned {
				g.emit(0x31, 0xD2)       // xor edx, edx
				g.emit(0x48, 0xF7, 0xF3) // div rbx
			} else {
				g.emit(0x48, 0x99)       // cqo
				g.emit(0x48, 0xF7, 0xFB) // idiv rbx
			}
			if ex.Op == "%" {
				g.emit(0x48, 0x89, 0xD0) // mov rax, rdx
			}
		case "&":
			g.emit(0x48, 0x21, 0xD8) // and rax, rbx
		case "|":
			g.emit(0x48, 0x09, 0xD8) // or rax, rbx
		case "^":
			g.emit(0x48, 0x31, 0xD8) // xor rax, rbx
		}
		return intResult(lt, rt)
	case "<<", ">>":
		if !lt.isInt() || !rt.isInt() {
			bad()
		}
		g.emit(0x48, 0x89, 0xD9) // mov rcx, rbx (счётчик в cl)
		switch {
		case ex.Op == "<<":
			g.emit(0x48, 0xD3, 0xE0) // shl rax, cl
		case lt.isUnsigned():
			g.emit(0x48, 0xD3, 0xE8) // shr rax, cl
		default:
			g.emit(0x48, 0xD3, 0xF8) // sar rax, cl
		}
		if lt.isU64() {
			return tU64
		}
		return tI64
	case "==", "!=", "<", "<=", ">", ">=":
		eq := ex.Op == "==" || ex.Op == "!="
		ok := (lt.isInt() && rt.isInt()) ||
			(eq && lt.Name == "bool" && rt.Name == "bool") ||
			(eq && ptrCompat(lt, rt))
		if !ok {
			g.fail("нельзя сравнить %s %s %s", lt, ex.Op, rt)
		}
		unsigned := lt.isU64() || rt.isU64()
		g.emit(0x48, 0x39, 0xD8) // cmp rax, rbx
		var cc byte
		switch ex.Op {
		case "==":
			cc = 0x94
		case "!=":
			cc = 0x95
		case "<":
			cc = 0x9C
			if unsigned {
				cc = 0x92 // setb
			}
		case "<=":
			cc = 0x9E
			if unsigned {
				cc = 0x96 // setbe
			}
		case ">":
			cc = 0x9F
			if unsigned {
				cc = 0x97 // seta
			}
		case ">=":
			cc = 0x9D
			if unsigned {
				cc = 0x93 // setae
			}
		}
		g.emit(0x0F, cc, 0xC0)         // setcc al
		g.emit(0x48, 0x0F, 0xB6, 0xC0) // movzx rax, al
		return tBool
	}
	g.fail("неизвестный оператор %s", ex.Op)
	return nil
}

func (g *Gen) genCallExpr(ex *CallE) *Type {
	if ex.Pkg == "" && builtinNames[ex.Name] {
		if hwBuiltins[ex.Name] && g.target == "hosted" {
			g.fail("%s() — инструкция железа, она есть только в $kernel и $boot", ex.Name)
		}
		if hostBuiltins[ex.Name] && g.target != "hosted" {
			g.fail("%s() работает через систему Linux и недоступна в $kernel/$boot", ex.Name)
		}
		switch ex.Name {
		case "print":
			return g.genCall("print", "_print", []*Type{tStr}, tVoid, ex.Args)
		case "puti":
			return g.genCall("puti", "_puti", []*Type{tI64}, tVoid, ex.Args)
		case "exit":
			return g.genCall("exit", "_exit", []*Type{tI64}, tVoid, ex.Args)
		case "alloc":
			return g.genCall("alloc", "_alloc", []*Type{tI64}, ptrTo(tU8), ex.Args)
		}
		return g.genIntrinsic(ex)
	}
	f := g.findFunc(ex.Pkg, ex.Name)
	if f == nil {
		g.fail("вызов неизвестной функции: %s", ex.Name)
	}
	if f.hasAttr("interrupt") || f.hasAttr("exception") {
		g.fail("обработчик прерывания %s нельзя вызывать, его адрес берёт fnaddr(%s)", f.Name, f.Name)
	}
	ptypes := make([]*Type, len(f.Params))
	for i, p := range f.Params {
		ptypes[i] = p.Typ
	}
	return g.genCall(f.Name, g.fnLabel(f), ptypes, f.Ret, ex.Args)
}

var argPops = [][]byte{{0x5F}, {0x5E}, {0x5A}, {0x59}, {0x41, 0x58}, {0x41, 0x59}} // rdi rsi rdx rcx r8 r9

func (g *Gen) genCall(name, label string, ptypes []*Type, ret *Type, args []Expr) *Type {
	if len(args) != len(ptypes) {
		g.fail("%s принимает аргументов: %d, передано: %d", name, len(ptypes), len(args))
	}
	for i := len(args) - 1; i >= 0; i-- {
		g.exprTo(args[i], ptypes[i], "аргумент "+strconv.Itoa(i+1)+" функции "+name)
		g.emit(0x50) // push rax
	}
	for i := 0; i < len(args) && i < 6; i++ {
		g.emit(argPops[i]...) // первые 6 аргументов в регистры, остальные остаются на стеке
	}
	g.call(label)
	if len(args) > 6 {
		g.emit(0x48, 0x81, 0xC4) // add rsp, 8*(n-6)
		g.u32(uint32(8 * (len(args) - 6)))
	}
	return ret
}

// число или указатель: аргумент встроенных инструкций (без строгой проверки размера)
func (g *Gen) wordArg(e Expr, what string) *Type {
	t := g.genExpr(e)
	if !t.isInt() && !t.isPtr() {
		g.fail("%s: нужно число или указатель, а не %s", what, t)
	}
	return t
}

func (g *Gen) wantArgs(ex *CallE, min, max int) {
	if len(ex.Args) >= min && len(ex.Args) <= max {
		return
	}
	if min == max {
		g.fail("%s() принимает аргументов: %d, передано: %d", ex.Name, min, len(ex.Args))
	}
	g.fail("%s() принимает от %d до %d аргументов, передано: %d", ex.Name, min, max, len(ex.Args))
}

// Встроенные инструкции и системные вызовы (результат в rax)
func (g *Gen) genIntrinsic(ex *CallE) *Type {
	n := ex.Name
	switch n {
	case "inb", "inw", "inl":
		g.wantArgs(ex, 1, 1)
		g.wordArg(ex.Args[0], n)
		g.emit(0x89, 0xC2) // mov edx, eax
		g.emit(0x31, 0xC0) // xor eax, eax
		switch n {
		case "inb":
			g.emit(0xEC) // in al, dx
			return tU8
		case "inw":
			g.emit(0x66, 0xED) // in ax, dx
			return tU16
		}
		g.emit(0xED) // in eax, dx
		return tU32
	case "outb", "outw", "outl":
		g.wantArgs(ex, 2, 2)
		g.wordArg(ex.Args[1], n) // значение
		g.emit(0x50)             // push rax
		g.wordArg(ex.Args[0], n) // порт
		g.emit(0x89, 0xC2)       // mov edx, eax
		g.emit(0x58)             // pop rax
		switch n {
		case "outb":
			g.emit(0xEE) // out dx, al
		case "outw":
			g.emit(0x66, 0xEF) // out dx, ax
		default:
			g.emit(0xEF) // out dx, eax
		}
		return tVoid
	case "cli":
		g.wantArgs(ex, 0, 0)
		g.emit(0xFA)
		return tVoid
	case "sti":
		g.wantArgs(ex, 0, 0)
		g.emit(0xFB)
		return tVoid
	case "hlt":
		g.wantArgs(ex, 0, 0)
		g.emit(0xF4)
		return tVoid
	case "pause":
		g.wantArgs(ex, 0, 0)
		g.emit(0xF3, 0x90)
		return tVoid
	case "readcr0", "readcr2", "readcr3", "readcr4":
		g.wantArgs(ex, 0, 0)
		modrm := map[string]byte{"readcr0": 0xC0, "readcr2": 0xD0, "readcr3": 0xD8, "readcr4": 0xE0}[n]
		g.emit(0x0F, 0x20, modrm) // mov rax, crN
		return tU64
	case "writecr0", "writecr3", "writecr4":
		g.wantArgs(ex, 1, 1)
		g.wordArg(ex.Args[0], n)
		modrm := map[string]byte{"writecr0": 0xC0, "writecr3": 0xD8, "writecr4": 0xE0}[n]
		g.emit(0x0F, 0x22, modrm) // mov crN, rax
		return tVoid
	case "rdmsr":
		g.wantArgs(ex, 1, 1)
		g.wordArg(ex.Args[0], n)
		g.emit(0x89, 0xC1)                   // mov ecx, eax
		g.emit(0x0F, 0x32)                   // rdmsr
		g.emit(0x48, 0xC1, 0xE2, 0x20)       // shl rdx, 32
		g.emit(0x48, 0x09, 0xD0)             // or rax, rdx
		return tU64
	case "wrmsr":
		g.wantArgs(ex, 2, 2)
		g.wordArg(ex.Args[1], n) // значение
		g.emit(0x50)
		g.wordArg(ex.Args[0], n) // номер MSR
		g.emit(0x89, 0xC1)       // mov ecx, eax
		g.emit(0x58)             // pop rax
		g.emit(0x48, 0x89, 0xC2) // mov rdx, rax
		g.emit(0x48, 0xC1, 0xEA, 0x20) // shr rdx, 32
		g.emit(0x0F, 0x30)       // wrmsr
		return tVoid
	case "lidt", "lgdt", "invlpg":
		g.wantArgs(ex, 1, 1)
		g.wordArg(ex.Args[0], n)
		switch n {
		case "lidt":
			g.emit(0x0F, 0x01, 0x18) // lidt [rax]
		case "lgdt":
			g.emit(0x0F, 0x01, 0x10) // lgdt [rax]
		default:
			g.emit(0x0F, 0x01, 0x38) // invlpg [rax]
		}
		return tVoid
	case "rdtsc":
		g.wantArgs(ex, 0, 0)
		g.emit(0x0F, 0x31)             // rdtsc
		g.emit(0x48, 0xC1, 0xE2, 0x20) // shl rdx, 32
		g.emit(0x48, 0x09, 0xD0)       // or rax, rdx
		return tU64
	case "fnaddr":
		g.wantArgs(ex, 1, 1)
		var f *Func
		switch a := ex.Args[0].(type) {
		case *VarE:
			f = g.findFunc("", a.Name)
		case *FieldE:
			if pv, ok := a.X.(*VarE); ok {
				f = g.findFunc(pv.Name, a.Name)
			}
		}
		if f == nil {
			g.fail("fnaddr ожидает имя функции: fnaddr(handler) или fnaddr(пакет.handler)")
		}
		g.emit(0x48, 0xB8) // mov rax, <адрес функции>
		g.special64('c', g.fnLabel(f))
		return tU64
	case "callfn":
		// callfn(адрес, арг1, ... арг6) -> i64 : вызов по указателю на функцию
		g.wantArgs(ex, 1, 7)
		args := ex.Args[1:]
		for i := len(args) - 1; i >= 0; i-- {
			g.wordArg(args[i], "аргумент callfn")
			g.emit(0x50)
		}
		g.wordArg(ex.Args[0], "адрес в callfn")
		g.emit(0x49, 0x89, 0xC3) // mov r11, rax
		for i := 0; i < len(args) && i < 6; i++ {
			g.emit(argPops[i]...)
		}
		g.emit(0x41, 0xFF, 0xD3) // call r11
		if len(args) > 6 {
			g.emit(0x48, 0x81, 0xC4)
			g.u32(uint32(8 * (len(args) - 6)))
		}
		return tI64
	case "syscall":
		// syscall(номер, a1..a6) -> i64 : прямой системный вызов Linux
		g.wantArgs(ex, 1, 7)
		for i := len(ex.Args) - 1; i >= 0; i-- {
			g.wordArg(ex.Args[i], "аргумент syscall")
			g.emit(0x50)
		}
		sysPops := [][]byte{{0x58}, {0x5F}, {0x5E}, {0x5A}, {0x41, 0x5A}, {0x41, 0x58}, {0x41, 0x59}} // rax rdi rsi rdx r10 r8 r9
		for i := 0; i < len(ex.Args); i++ {
			g.emit(sysPops[i]...)
		}
		g.emit(0x0F, 0x05)
		return tI64
	case "argc":
		g.wantArgs(ex, 0, 0)
		g.emit(0x48, 0xB8)
		g.special64('B', "__sp0")
		g.emit(0x48, 0x8B, 0x00) // mov rax, [rax]  (сохранённый rsp)
		g.emit(0x48, 0x8B, 0x00) // mov rax, [rax]  (argc)
		return tI64
	case "argv":
		g.wantArgs(ex, 1, 1)
		g.wordArg(ex.Args[0], n)
		g.emit(0x48, 0x89, 0xC3) // mov rbx, rax
		g.emit(0x48, 0xB8)
		g.special64('B', "__sp0")
		g.emit(0x48, 0x8B, 0x00)             // mov rax, [rax]
		g.emit(0x48, 0x8D, 0x44, 0xD8, 0x08) // lea rax, [rax+rbx*8+8]
		g.emit(0x48, 0x8B, 0x00)             // mov rax, [rax]  (char* argv[i])
		return ptrTo(tU8)
	case "make":
		g.fail("make(T, n) принимает тип первым аргументом: make(i64, 10)")
	}
	g.fail("неизвестная встроенная функция %s", n)
	return nil
}

// ---------- Финальная сборка ----------

const stackSize = 0x10000 // стек ядра/загрузчика за образом и bss

func align16(v uint64) uint64 { return (v + 15) &^ 15 }

func put32(b []byte, pos int, v uint32) {
	for i := 0; i < 4; i++ {
		b[pos+i] = byte(v >> (8 * i))
	}
}

func put64(b []byte, pos int, v uint64) {
	for i := 0; i < 8; i++ {
		b[pos+i] = byte(v >> (8 * i))
	}
}

// Patch раскладывает код+данные по базе base, патчит все фиксапы и считает g.Extra.
// За образом (code+data) идут bss и стек: их адреса тоже вычисляются здесь.
func (g *Gen) Patch(base uint64) []byte {
	dataBase := base + uint64(len(g.code))
	image := append(append([]byte{}, g.code...), g.data...)
	imageEnd := base + uint64(len(image))
	bssBase := align16(imageEnd)
	stackTop := align16(bssBase + uint64(g.bssSize) + stackSize)
	g.Extra = stackTop - imageEnd

	for _, fx := range g.fix {
		switch fx.kind {
		case 'r':
			tgt, ok := g.labels[fx.target]
			if !ok {
				panic("нет метки: " + fx.target)
			}
			put32(image, fx.pos, uint32(int32(tgt-(fx.pos+4))))
		case 'c': // абсолютный адрес метки кода
			tgt, ok := g.labels[fx.target]
			if !ok {
				panic("нет метки: " + fx.target)
			}
			put64(image, fx.pos, base+uint64(tgt))
		case 'a':
			off, ok := g.dlabels[fx.target]
			if !ok {
				panic("нет данных: " + fx.target)
			}
			put64(image, fx.pos, dataBase+uint64(off))
		case 'B': // адрес в bss
			off, ok := g.bssLabels[fx.target]
			if !ok {
				panic("нет bss: " + fx.target)
			}
			put64(image, fx.pos, bssBase+uint64(off))
		case 'G':
			put64(image, fx.pos, bssBase)
		case 'Z':
			put32(image, fx.pos, uint32(g.bssSize))
		case 'S':
			put64(image, fx.pos, stackTop)
		case 'p':
			off, ok := g.dlabels[fx.target]
			if !ok {
				panic("нет данных: " + fx.target)
			}
			addr := dataBase + uint64(off)
			instrEnd := base + uint64(fx.pos+4)
			put32(image, fx.pos, uint32(int32(int64(addr)-int64(instrEnd))))
		}
	}
	return image
}
