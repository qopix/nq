package main

import (
	"fmt"
	"strconv"
	"strings"
)

// ---------- AST ----------

type Expr interface{}

type NumE struct{ Val int64 }
type StrE struct{ Val string }
type VarE struct{ Name string }
type BoolE struct{ Val bool }
type NilE struct{}
type BinE struct {
	Op   string
	L, R Expr
}
type UnE struct { // "-", "!", "addr", "deref"
	Op string
	X  Expr
}
type SizeofE struct {
	T *Type // либо тип...
	X Expr  // ...либо выражение
}
type CallE struct {
	Pkg  string // псевдоним импорта: str.slen(s); "" = без префикса
	Name string
	Args []Expr
}
type CastE struct {
	T *Type
	X Expr
}

type IndexE struct { // p[i] == deref(p + i), без проверки границ
	X Expr
	I Expr
}
type MakeE struct { // make(T, n) -> ptr<T>, нулевая память
	T *Type
	N Expr
}

type FieldE struct { // p.x  (p: ptr<Структура>)
	X    Expr
	Name string
}

type Pos struct{ Line int }

func (p *Pos) Ln() int     { return p.Line }
func (p *Pos) SetLn(l int) { p.Line = l }

type Stmt interface {
	Ln() int
	SetLn(int)
}

type LetS struct {
	Pos
	Name string
	Typ  *Type // nil = вывести из значения
	Mut  bool  // var = изменяемая, let = нет
	Init Expr
}
type AssignS struct {
	Pos
	Name string
	X    Expr
}
type DAssignS struct { // deref(p) = x
	Pos
	Ptr Expr
	X   Expr
}
type IAssignS struct { // p[i] = x
	Pos
	X *IndexE
	V Expr
}
type FAssignS struct { // p.x = v
	Pos
	X *FieldE
	V Expr
}
type ExprS struct {
	Pos
	X Expr
}
type RetS struct {
	Pos
	X Expr
}
type IfS struct {
	Pos
	Cond Expr
	Then []Stmt
	Else []Stmt
}
type WhileS struct {
	Pos
	Cond Expr
	Body []Stmt
}
type ForS struct {
	Pos
	Init Stmt
	Cond Expr
	Post Stmt
	Body []Stmt
}
type BreakS struct{ Pos }
type ContS struct{ Pos }
type AsmS struct {
	Pos
	Code []byte
}

type Param struct {
	Name string
	Typ  *Type
}

type Func struct {
	Name   string
	Params []Param
	Ret    *Type
	Body   []Stmt
	File   string
	Line   int
	Prog   *Program
}

type ImportDecl struct {
	Path  string
	Alias string
	Line  int
}

type Program struct {
	Path    string
	ID      string // p0 (главный), p1, ...
	Mode    string // "low" | "mid" | "hight" ("" у импортируемых файлов без директивы)
	Target  string // "" (обычная программа) | "kernel" | "boot"
	Trusted bool   // встроенная stdlib: можно небезопасные операции в любом режиме
	Funcs   []*Func
	Consts  map[string]int64
	Structs map[string]*StructDef
	Imports []ImportDecl
	Alias   map[string]*Program
	byName  map[string]*Func
}

// ---------- Parser ----------

type Parser struct {
	toks []Tok
	pos  int
	file string
	prog *Program
}

func NewParser(toks []Tok, file string) *Parser { return &Parser{toks: toks, file: file} }

func (p *Parser) peek() Tok { return p.toks[p.pos] }
func (p *Parser) peek2() Tok {
	if p.pos+1 < len(p.toks) {
		return p.toks[p.pos+1]
	}
	return p.toks[p.pos]
}
func (p *Parser) next() Tok {
	t := p.toks[p.pos]
	if t.Kind != "eof" {
		p.pos++
	}
	return t
}

func (p *Parser) err(t Tok, msg string) {
	panic(fmt.Sprintf("%s:%d: %s (около %q)", p.file, t.Line, msg, t.Val))
}

func (p *Parser) isPunct(v string) bool {
	t := p.peek()
	return t.Kind == "punct" && t.Val == v
}

func (p *Parser) isWord(v string) bool {
	t := p.peek()
	return t.Kind == "ident" && t.Val == v
}

func (p *Parser) expectPunct(v string) Tok {
	t := p.next()
	if t.Kind != "punct" || t.Val != v {
		p.err(t, "ожидалось '"+v+"'")
	}
	return t
}

func (p *Parser) acceptPunct(v string) bool {
	if p.isPunct(v) {
		p.pos++
		return true
	}
	return false
}

func (p *Parser) expectName(what string) Tok {
	t := p.next()
	if t.Kind != "ident" {
		p.err(t, "ожидалось "+what)
	}
	if keywords[t.Val] {
		p.err(t, "'"+t.Val+"' — ключевое слово, его нельзя использовать как "+what)
	}
	return t
}

func (p *Parser) parseProgram() *Program {
	prog := &Program{Consts: map[string]int64{}, Structs: map[string]*StructDef{}, Alias: map[string]*Program{}, byName: map[string]*Func{}, Path: p.file}

	p.prog = prog

	// директивы: $hight / $mid / $low и необязательно kernel / boot
	for p.isPunct("$") {
		p.next()
		w := p.next()
		if w.Kind != "ident" {
			p.err(w, "после $ ожидалось hight, mid, low, kernel или boot")
		}
		switch w.Val {
		case "hight", "high", "mid", "low":
			m := w.Val
			if m == "high" {
				m = "hight"
			}
			if prog.Mode != "" {
				p.err(w, "режим уже задан директивой $"+prog.Mode)
			}
			prog.Mode = m
		case "kernel", "boot":
			prog.Target = w.Val
		default:
			p.err(w, "неизвестная директива (есть $hight, $mid, $low, $kernel, $boot)")
		}
		for p.peek().Kind == "ident" && (p.peek().Val == "kernel" || p.peek().Val == "boot") {
			prog.Target = p.next().Val
		}
	}

	for p.peek().Kind != "eof" {
		t := p.peek()
		switch {
		case t.Kind == "ident" && t.Val == "import":
			p.next()
			s := p.next()
			if s.Kind != "str" {
				p.err(s, "после import ожидалась строка с путём")
			}
			d := ImportDecl{Path: s.Val, Line: s.Line}
			if p.isWord("as") {
				p.next()
				d.Alias = p.expectName("псевдоним импорта").Val
			}
			prog.Imports = append(prog.Imports, d)
		case t.Kind == "ident" && t.Val == "const":
			p.next()
			name := p.expectName("имя константы")
			p.expectPunct("=")
			e := p.parseExpr()
			v, ok := constEval(e, prog.Consts)
			if !ok {
				p.err(name, "значение константы должно вычисляться на этапе компиляции")
			}
			prog.Consts[name.Val] = v
		case t.Kind == "ident" && t.Val == "type":
			p.next()
			name := p.expectName("имя типа")
			if kw := p.next(); kw.Kind != "ident" || kw.Val != "struct" {
				p.err(kw, "после имени типа ожидалось struct: type Имя struct { поле: тип }")
			}
			if _, dup := prog.Structs[name.Val]; dup {
				p.err(name, "тип "+name.Val+" объявлен дважды")
			}
			def := &StructDef{Name: name.Val, Prog: prog, Line: name.Line}
			prog.Structs[name.Val] = def
			p.expectPunct("{")
			for !p.acceptPunct("}") {
				fn := p.expectName("имя поля")
				if def.field(fn.Val) != nil {
					p.err(fn, "поле "+fn.Val+" объявлено дважды")
				}
				p.acceptPunct(":")
				ft := p.parseType()
				def.Fields = append(def.Fields, &Field{Name: fn.Val, Typ: ft, Off: 8 * len(def.Fields)})
				if !p.acceptPunct(",") {
					p.acceptPunct(";")
				}
			}
		case t.Kind == "ident" && t.Val == "fn":
			f := p.parseFunc()
			f.Prog = prog
			if _, dup := prog.byName[f.Name]; dup {
				p.err(t, "функция "+f.Name+" объявлена дважды")
			}
			prog.byName[f.Name] = f
			prog.Funcs = append(prog.Funcs, f)
		case t.Kind == "punct" && t.Val == ";":
			p.next()
		default:
			p.err(t, "на верхнем уровне допускаются только fn, type, const, import")
		}
	}
	return prog
}

func constEval(e Expr, consts map[string]int64) (int64, bool) {
	switch x := e.(type) {
	case *NumE:
		return x.Val, true
	case *VarE:
		v, ok := consts[x.Name]
		return v, ok
	case *UnE:
		if x.Op == "-" {
			v, ok := constEval(x.X, consts)
			return -v, ok
		}
	case *BinE:
		a, ok1 := constEval(x.L, consts)
		b, ok2 := constEval(x.R, consts)
		if !ok1 || !ok2 {
			return 0, false
		}
		switch x.Op {
		case "+":
			return a + b, true
		case "-":
			return a - b, true
		case "*":
			return a * b, true
		case "/":
			if b != 0 {
				return a / b, true
			}
		case "%":
			if b != 0 {
				return a % b, true
			}
		case "&":
			return a & b, true
		case "|":
			return a | b, true
		}
	}
	return 0, false
}

func parseNum(s string) int64 {
	var v int64
	var err error
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		var u uint64
		u, err = strconv.ParseUint(s[2:], 16, 64)
		v = int64(u)
	} else {
		v, err = strconv.ParseInt(s, 10, 64)
	}
	if err != nil {
		panic("плохое число: " + s)
	}
	return v
}

// type = i64 | u8 | bool | void | ptr | ptr<type>
func (p *Parser) parseType() *Type {
	t := p.next()
	if t.Kind != "ident" {
		p.err(t, "ожидался тип (i64, u8, bool, ptr<T>)")
	}
	if t.Val == "ptr" {
		if p.acceptPunct("<") {
			e := p.parseType()
			p.expectPunct(">")
			return ptrTo(e)
		}
		return tStr
	}
	if ty := typeByName(t.Val); ty != nil {
		return ty
	}
	// имя структуры (проверяется позже, когда загружены все импорты): Имя или пакет.Имя
	pkg, name := "", t.Val
	if p.isPunct(".") && p.peek2().Kind == "ident" {
		p.next()
		pkg = name
		name = p.next().Val
	}
	return &Type{Name: "struct", Ref: name, Pkg: pkg, Prog: p.prog}
}

func (p *Parser) parseFunc() *Func {
	p.next() // fn
	name := p.expectName("имя функции")
	f := &Func{Name: name.Val, Ret: tI64, File: p.file, Line: name.Line}
	p.expectPunct("(")
	for !p.acceptPunct(")") {
		pn := p.expectName("имя параметра")
		p.acceptPunct(":")
		pt := p.parseType()
		if pt.Name == "void" {
			p.err(pn, "параметр не может быть void")
		}
		f.Params = append(f.Params, Param{pn.Val, pt})
		if !p.acceptPunct(",") {
			p.expectPunct(")")
			break
		}
	}
	// тип возврата: `-> T` или просто `T`; без него — i64
	if p.acceptPunct("->") {
		f.Ret = p.parseType()
	} else if p.peek().Kind == "ident" {
		f.Ret = p.parseType()
	}
	f.Body = p.parseBlock()
	return f
}

func (p *Parser) parseBlock() []Stmt {
	p.expectPunct("{")
	var out []Stmt
	for !p.acceptPunct("}") {
		if p.peek().Kind == "eof" {
			p.err(p.peek(), "не закрыта '{'")
		}
		out = append(out, p.parseStmt())
		p.acceptPunct(";")
	}
	return out
}

func (p *Parser) parseStmt() Stmt {
	line := p.peek().Line
	s := p.stmt()
	s.SetLn(line)
	return s
}

func (p *Parser) stmt() Stmt {
	t := p.peek()
	if t.Kind == "ident" {
		switch t.Val {
		case "let", "var":
			p.next()
			name := p.expectName("имя переменной")
			ls := &LetS{Name: name.Val, Mut: t.Val == "var"}
			if p.acceptPunct(":") {
				ls.Typ = p.parseType()
			} else if p.peek().Kind == "ident" {
				ls.Typ = p.parseType()
			}
			p.expectPunct("=")
			ls.Init = p.parseExpr()
			return ls
		case "return":
			p.next()
			if p.isPunct("}") || p.isPunct(";") {
				return &RetS{X: nil}
			}
			return &RetS{X: p.parseExpr()}
		case "if":
			p.next()
			cond := p.parseExpr()
			then := p.parseBlock()
			var els []Stmt
			if p.isWord("else") {
				p.next()
				if p.isWord("if") {
					els = []Stmt{p.parseStmt()}
				} else {
					els = p.parseBlock()
				}
			}
			return &IfS{Cond: cond, Then: then, Else: els}
		case "while":
			p.next()
			cond := p.parseExpr()
			return &WhileS{Cond: cond, Body: p.parseBlock()}
		case "for":
			p.next()
			var init Stmt
			if p.isWord("let") || p.isWord("var") {
				init = p.parseStmt()
			} else {
				init = p.parseSimpleStmt()
			}
			p.expectPunct(";")
			cond := p.parseExpr()
			p.expectPunct(";")
			post := p.parseSimpleStmt()
			return &ForS{Init: init, Cond: cond, Post: post, Body: p.parseBlock()}
		case "break":
			p.next()
			return &BreakS{}
		case "continue":
			p.next()
			return &ContS{}
		case "asm":
			p.next()
			p.expectPunct("(")
			s := p.next()
			if s.Kind != "str" {
				p.err(s, "asm ожидает строку с hex-байтами, например asm(\"fa f4\")")
			}
			p.expectPunct(")")
			var code []byte
			for _, w := range strings.Fields(s.Val) {
				n, err := strconv.ParseUint(strings.TrimPrefix(w, "0x"), 16, 8)
				if err != nil {
					p.err(s, "в asm допустимы только байты в hex: "+w)
				}
				code = append(code, byte(n))
			}
			return &AsmS{Code: code}
		}
	}
	return p.parseSimpleStmt()
}

// присваивание, составное присваивание, deref-присваивание или вызов
func (p *Parser) parseSimpleStmt() Stmt {
	line := p.peek().Line
	s := p.simpleStmt()
	s.SetLn(line)
	return s
}

var compoundOps = map[string]string{"+=": "+", "-=": "-", "*=": "*", "/=": "/", "%=": "%"}

func (p *Parser) simpleStmt() Stmt {
	t := p.peek()
	if t.Kind == "ident" && !keywords[t.Val] {
		n := p.peek2()
		if n.Kind == "punct" && n.Val == "=" {
			p.next()
			p.next()
			return &AssignS{Name: t.Val, X: p.parseExpr()}
		}
		if n.Kind == "punct" && compoundOps[n.Val] != "" {
			p.next()
			p.next()
			return &AssignS{Name: t.Val, X: &BinE{compoundOps[n.Val], &VarE{t.Val}, p.parseExpr()}}
		}
	}
	if t.Kind == "ident" && !keywords[t.Val] && p.peek2().Kind == "punct" && (p.peek2().Val == "[" || p.peek2().Val == ".") {
		save := p.pos
		lhs := p.parseUnary()
		isAssign := p.isPunct("=")
		isCompound := false
		if n := p.peek(); n.Kind == "punct" && compoundOps[n.Val] != "" {
			isCompound = true
		}
		if isAssign || isCompound {
			var rhs Expr
			if isAssign {
				p.next()
				rhs = p.parseExpr()
			} else {
				op := compoundOps[p.next().Val]
				rhs = &BinE{op, lhs, p.parseExpr()}
			}
			switch lv := lhs.(type) {
			case *IndexE:
				return &IAssignS{X: lv, V: rhs}
			case *FieldE:
				return &FAssignS{X: lv, V: rhs}
			}
			p.err(t, "в левой части присваивания можно: переменная, deref(p), p[i], p.поле")
		}
		p.pos = save
	}
	if (t.Kind == "punct" && t.Val == "*") || (t.Kind == "ident" && t.Val == "deref") {
		save := p.pos
		var ptrE Expr
		if t.Val == "*" {
			p.next()
			ptrE = p.parseUnary()
		} else {
			p.next()
			p.expectPunct("(")
			ptrE = p.parseExpr()
			p.expectPunct(")")
		}
		if p.acceptPunct("=") {
			return &DAssignS{Ptr: ptrE, X: p.parseExpr()}
		}
		if n := p.peek(); n.Kind == "punct" && compoundOps[n.Val] != "" {
			p.next()
			return &DAssignS{Ptr: ptrE, X: &BinE{compoundOps[n.Val], &UnE{"deref", ptrE}, p.parseExpr()}}
		}
		p.pos = save
	}
	if t.Kind == "ident" && keywords[t.Val] && t.Val != "addr" && t.Val != "deref" &&
		t.Val != "sizeof" && t.Val != "cast" {
		p.err(t, "здесь не ожидалось ключевое слово")
	}
	x := p.parseExpr()
	if _, ok := x.(*CallE); !ok {
		p.err(t, "выражение без эффекта: как оператор допустим только вызов функции")
	}
	return &ExprS{X: x}
}

// ---------- Выражения ----------
// приоритеты (выше = крепче): || < && < сравнения < | < & < + - < * / %

var binPrec = map[string]int{
	"||": 1, "&&": 2,
	"==": 3, "!=": 3, "<": 3, "<=": 3, ">": 3, ">=": 3,
	"|": 4, "&": 5,
	"+": 6, "-": 6,
	"*": 7, "/": 7, "%": 7,
}

func (p *Parser) parseExpr() Expr { return p.parseBin(1) }

func (p *Parser) parseBin(min int) Expr {
	l := p.parseUnary()
	for {
		t := p.peek()
		pr, ok := binPrec[t.Val]
		if t.Kind != "punct" || !ok || pr < min {
			return l
		}
		// `*` в начале новой строки — это начало оператора `*p = ...`, а не умножение
		if t.Val == "*" && t.Line > p.toks[p.pos-1].Line {
			return l
		}
		p.next()
		r := p.parseBin(pr + 1)
		l = &BinE{t.Val, l, r}
	}
}

func isTypeWord(s string) bool {
	return s == "ptr" || typeByName(s) != nil
}

func (p *Parser) parseUnary() Expr {
	t := p.peek()
	if t.Kind == "punct" {
		switch t.Val {
		case "-", "!":
			p.next()
			return &UnE{t.Val, p.parseUnary()}
		case "&":
			p.next()
			return &UnE{"addr", p.parseUnary()}
		case "*":
			p.next()
			return &UnE{"deref", p.parseUnary()}
		}
	}
	if t.Kind == "ident" && p.peek2().Kind == "punct" && p.peek2().Val == "(" {
		switch t.Val {
		case "addr", "deref":
			p.next()
			p.expectPunct("(")
			e := p.parseExpr()
			p.expectPunct(")")
			return &UnE{t.Val, e}
		case "sizeof":
			p.next()
			p.expectPunct("(")
			var s *SizeofE
			if n := p.peek(); n.Kind == "ident" && isTypeWord(n.Val) {
				s = &SizeofE{T: p.parseType()}
			} else {
				s = &SizeofE{X: p.parseExpr()}
			}
			p.expectPunct(")")
			return s
		case "make":
			p.next()
			p.expectPunct("(")
			typ := p.parseType()
			p.expectPunct(",")
			n := p.parseExpr()
			p.expectPunct(")")
			return &MakeE{T: typ, N: n}
		case "cast":
			p.next()
			p.expectPunct("(")
			typ := p.parseType()
			p.expectPunct(",")
			e := p.parseExpr()
			p.expectPunct(")")
			return &CastE{T: typ, X: e}
		}
	}
	return p.parsePostfix()
}

// primary { "[" expr "]" | "." имя }   (на новой строке — уже не продолжение)
func (p *Parser) parsePostfix() Expr {
	e := p.parsePrimary()
	for {
		sameLine := p.peek().Line == p.toks[p.pos-1].Line
		switch {
		case p.isPunct("[") && sameLine:
			p.next()
			i := p.parseExpr()
			p.expectPunct("]")
			e = &IndexE{X: e, I: i}
		case p.isPunct(".") && p.peek2().Kind == "ident" && sameLine:
			p.next()
			e = &FieldE{X: e, Name: p.next().Val}
		default:
			return e
		}
	}
}

func (p *Parser) parsePrimary() Expr {
	t := p.next()
	switch t.Kind {
	case "num":
		return &NumE{parseNum(t.Val)}
	case "str":
		return &StrE{t.Val}
	case "ident":
		switch t.Val {
		case "true":
			return &BoolE{true}
		case "false":
			return &BoolE{false}
		case "nil":
			return &NilE{}
		}
		pkg := ""
		name := t.Val
		if p.isPunct(".") && p.peek2().Kind == "ident" && p.pos+2 < len(p.toks) &&
			p.toks[p.pos+2].Kind == "punct" && p.toks[p.pos+2].Val == "(" {
			p.next()
			pkg = name
			name = p.next().Val
		}
		if p.acceptPunct("(") {
			call := &CallE{Pkg: pkg, Name: name}
			for !p.acceptPunct(")") {
				call.Args = append(call.Args, p.parseExpr())
				if !p.acceptPunct(",") {
					p.expectPunct(")")
					break
				}
			}
			return call
		}
		return &VarE{name}
	case "punct":
		if t.Val == "(" {
			e := p.parseExpr()
			p.expectPunct(")")
			return e
		}
	}
	p.err(t, "ожидалось выражение")
	return nil
}
