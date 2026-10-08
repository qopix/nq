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
	scopes []map[string]*local
	frame  int
	brkLbl []string
	cntLbl []string
}

func NewGen(mode string) *Gen {
	return &Gen{
		labels:  map[string]int{},
		dlabels: map[string]int{},
		strLbl:  map[string]string{},
		mode:    mode,
		target:  "hosted",
	}
}

func (g *Gen) fail(format string, a ...any) {
	file := ""
	if g.cur != nil {
		file = g.cur.File
	}
	panic(fmt.Sprintf("%s:%d: %s", file, g.line, fmt.Sprintf(format, a...)))
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

var builtinNames = map[string]bool{"print": true, "puti": true, "exit": true, "alloc": true, "make": true}

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
				g.cur, g.line = f, f.Line
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
			g.line = d.Line
			for _, f := range d.Fields {
				g.checkValueType(f.Typ, "поле "+d.Name+"."+f.Name)
				if f.Typ.Name == "void" {
					g.fail("поле %s.%s не может быть void", d.Name, f.Name)
				}
			}
		}
	}
	mf := mainProg.byName["main"]
	if mf == nil {
		panic(mainProg.Path + ": нет функции main")
	}
	g.cur, g.line = mf, mf.Line
	if len(mf.Params) != 0 {
		g.fail("main не принимает параметров")
	}
	if mf.Ret.Name != "i64" && mf.Ret.Name != "void" {
		g.fail("main должна возвращать i64 или ничего")
	}

	// _start: вызвать main и завершиться
	g.mark("_start")
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

func (g *Gen) genFunc(f *Func) {
	g.cur = f
	g.line = f.Line
	g.mark(g.fnLabel(f))
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
	if len(f.Params) > 6 {
		g.fail("больше 6 параметров пока не поддерживается")
	}
	g.checkValueType(f.Ret, "результат "+f.Name)
	for _, prm := range f.Params {
		g.checkValueType(prm.Typ, "параметр "+prm.Name)
	}
	for i, prm := range f.Params {
		if _, dup := g.scopes[0][prm.Name]; dup {
			g.fail("параметр %s указан дважды", prm.Name)
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
func (g *Gen) exprTo(e Expr, want *Type, what string) {
	if n, ok := e.(*NumE); ok && want.Name == "u8" {
		if n.Val < 0 || n.Val > 255 {
			g.fail("число %d не помещается в u8 (%s)", n.Val, what)
		}
		g.movImm(n.Val)
		return
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
		g.emit(0x48, 0x89, 0xC3) // mov rbx, rax
		g.emit(0x58)             // pop rax
		if pt.Elem.size() == 1 {
			g.emit(0x88, 0x18) // mov [rax], bl
		} else {
			g.emit(0x48, 0x89, 0x18) // mov [rax], rbx
		}
	case *FAssignS:
		def := g.genStructAddr(st.X.X)
		f := def.field(st.X.Name)
		if f == nil {
			g.fail("у структуры %s нет поля %s", def.Name, st.X.Name)
		}
		g.addOff(f.Off)
		g.emit(0x50) // push rax (адрес поля)
		g.exprTo(st.V, f.Typ, "запись в поле "+def.Name+"."+f.Name)
		g.emit(0x48, 0x89, 0xC3) // mov rbx, rax
		g.emit(0x58)             // pop rax
		if f.Typ.size() == 1 {
			g.emit(0x88, 0x18) // mov [rax], bl
		} else {
			g.emit(0x48, 0x89, 0x18) // mov [rax], rbx
		}
	case *IAssignS:
		pt := g.genIndexAddr(st.X)
		if pt.Elem.Name == "struct" {
			g.fail("структуру целиком присвоить нельзя: пиши p[i].поле = значение")
		}
		g.emit(0x50) // push rax (адрес элемента)
		g.exprTo(st.V, pt.Elem, "запись по индексу")
		g.emit(0x48, 0x89, 0xC3) // mov rbx, rax
		g.emit(0x58)             // pop rax
		if pt.Elem.size() == 1 {
			g.emit(0x88, 0x18) // mov [rax], bl
		} else {
			g.emit(0x48, 0x89, 0x18) // mov [rax], rbx
		}
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

// loadElem: rax = *(T*)rax
func (g *Gen) loadElem(t *Type) {
	if t.size() == 1 {
		g.emit(0x48, 0x0F, 0xB6, 0x00) // movzx rax, byte [rax]
	} else {
		g.emit(0x48, 0x8B, 0x00) // mov rax, [rax]
	}
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
		if to.Name == "u8" {
			g.emit(0x0F, 0xB6, 0xC0) // movzx eax, al
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
		if t.Elem.size() == 1 {
			g.emit(0x48, 0x0F, 0xB6, 0x00) // movzx rax, byte [rax]
		} else if t.Elem.size() == 8 {
			g.emit(0x48, 0x8B, 0x00) // mov rax, [rax]
		} else {
			g.fail("нельзя разыменовать %s", t)
		}
		return t.Elem
	}
	g.fail("неизвестный унарный оператор %s", ex.Op)
	return nil
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
		return tI64
	case "*", "/", "%", "&", "|":
		if !lt.isInt() || !rt.isInt() {
			bad()
		}
		switch ex.Op {
		case "*":
			g.emit(0x48, 0x0F, 0xAF, 0xC3)
		case "/":
			g.emit(0x48, 0x99)       // cqo
			g.emit(0x48, 0xF7, 0xFB) // idiv rbx
		case "%":
			g.emit(0x48, 0x99)
			g.emit(0x48, 0xF7, 0xFB)
			g.emit(0x48, 0x89, 0xD0) // mov rax, rdx
		case "&":
			g.emit(0x48, 0x21, 0xD8)
		case "|":
			g.emit(0x48, 0x09, 0xD8)
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
		g.emit(0x48, 0x39, 0xD8) // cmp rax, rbx
		switch ex.Op {
		case "==":
			g.emit(0x0F, 0x94, 0xC0)
		case "!=":
			g.emit(0x0F, 0x95, 0xC0)
		case "<":
			g.emit(0x0F, 0x9C, 0xC0)
		case "<=":
			g.emit(0x0F, 0x9E, 0xC0)
		case ">":
			g.emit(0x0F, 0x9F, 0xC0)
		case ">=":
			g.emit(0x0F, 0x9D, 0xC0)
		}
		g.emit(0x48, 0x0F, 0xB6, 0xC0) // movzx rax, al
		return tBool
	}
	g.fail("неизвестный оператор %s", ex.Op)
	return nil
}

func (g *Gen) genCallExpr(ex *CallE) *Type {
	if ex.Pkg == "" && builtinNames[ex.Name] {
		switch ex.Name {
		case "print":
			return g.genCall("print", "_print", []*Type{tStr}, tVoid, ex.Args)
		case "puti":
			return g.genCall("puti", "_puti", []*Type{tI64}, tVoid, ex.Args)
		case "exit":
			return g.genCall("exit", "_exit", []*Type{tI64}, tVoid, ex.Args)
		case "alloc":
			if g.target != "hosted" {
				g.fail("alloc есть только в обычных программах (не в $kernel/$boot)")
			}
			return g.genCall("alloc", "_alloc", []*Type{tI64}, ptrTo(tU8), ex.Args)
		}
	}
	f := g.findFunc(ex.Pkg, ex.Name)
	if f == nil {
		g.fail("вызов неизвестной функции: %s", ex.Name)
	}
	ptypes := make([]*Type, len(f.Params))
	for i, p := range f.Params {
		ptypes[i] = p.Typ
	}
	return g.genCall(f.Name, g.fnLabel(f), ptypes, f.Ret, ex.Args)
}

func (g *Gen) genCall(name, label string, ptypes []*Type, ret *Type, args []Expr) *Type {
	if len(args) != len(ptypes) {
		g.fail("%s принимает аргументов: %d, передано: %d", name, len(ptypes), len(args))
	}
	if len(args) > 6 {
		g.fail("больше 6 аргументов пока не поддерживается")
	}
	for i := len(args) - 1; i >= 0; i-- {
		g.exprTo(args[i], ptypes[i], "аргумент "+strconv.Itoa(i+1)+" функции "+name)
		g.emit(0x50) // push rax
	}
	pops := [][]byte{{0x5F}, {0x5E}, {0x5A}, {0x59}, {0x41, 0x58}, {0x41, 0x59}}
	for i := 0; i < len(args); i++ {
		g.emit(pops[i]...)
	}
	g.call(label)
	return ret
}

// ---------- Финальная сборка ----------

// Patch раскладывает код+данные по базе base и патчит все фиксапы.
func (g *Gen) Patch(base uint64) []byte {
	dataBase := base + uint64(len(g.code))
	image := append(append([]byte{}, g.code...), g.data...)
	for _, fx := range g.fix {
		switch fx.kind {
		case 'r':
			tgt, ok := g.labels[fx.target]
			if !ok {
				panic("нет метки: " + fx.target)
			}
			disp := int32(tgt - (fx.pos + 4))
			for i := 0; i < 4; i++ {
				image[fx.pos+i] = byte(disp >> (8 * i))
			}
		case 'a':
			off, ok := g.dlabels[fx.target]
			if !ok {
				panic("нет данных: " + fx.target)
			}
			addr := dataBase + uint64(off)
			for i := 0; i < 8; i++ {
				image[fx.pos+i] = byte(addr >> (8 * i))
			}
		case 'p':
			off, ok := g.dlabels[fx.target]
			if !ok {
				panic("нет данных: " + fx.target)
			}
			addr := dataBase + uint64(off)
			instrEnd := base + uint64(fx.pos+4)
			disp := int32(int64(addr) - int64(instrEnd))
			for i := 0; i < 4; i++ {
				image[fx.pos+i] = byte(disp >> (8 * i))
			}
		}
	}
	return image
}


