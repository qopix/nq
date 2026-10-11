package main

// ---------- Типы Nq ----------
// i64 (8 байт), u8 (1 байт), bool, ptr<T>, void.
// Голый `ptr` == ptr<i64> (так устроены строки: [длина:8][байты]).

type Type struct {
	Name string // "i64" "u8" "bool" "ptr" "void" "nil" "struct"
	Elem *Type  // для ptr

	// для struct: имя ищется лениво (после загрузки всех импортов)
	Ref  string
	Pkg  string
	Prog *Program
	Def  *StructDef
}

// Структуры живут в памяти; в переменных и параметрах используются через ptr<Имя>.
// Каждое поле занимает 8 байт (u8/bool читаются и пишутся одним байтом).
type Field struct {
	Name string
	Typ  *Type
	Off  int
}

type StructDef struct {
	Name   string
	Fields []*Field
	Prog   *Program
	Line   int
	Packed bool // @packed: без выравнивания (для дескрипторов железа)

	laidOut bool
	Size    int
}

// layout считает смещения как в C: поле выравнивается по своему размеру (до 8).
// Вызывается лениво, когда уже известны все типы.
func (d *StructDef) layout() {
	if d.laidOut {
		return
	}
	d.laidOut = true
	off, maxAlign := 0, 1
	for _, f := range d.Fields {
		sz := f.Typ.size()
		al := sz
		if al < 1 {
			al = 1
		}
		if al > 8 {
			al = 8
		}
		if d.Packed {
			al = 1
		}
		off = (off + al - 1) / al * al
		f.Off = off
		off += sz
		if al > maxAlign {
			maxAlign = al
		}
	}
	d.Size = (off + maxAlign - 1) / maxAlign * maxAlign
	if d.Size == 0 {
		d.Size = 8
	}
}

func (d *StructDef) size() int {
	d.layout()
	return d.Size
}

func (d *StructDef) hasField(name string) bool {
	for _, f := range d.Fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

func (d *StructDef) field(name string) *Field {
	d.layout()
	for _, f := range d.Fields {
		if f.Name == name {
			return f
		}
	}
	return nil
}

func lookupStruct(pr *Program, pkg, name string) *StructDef {
	if pkg != "" {
		if ip, ok := pr.Alias[pkg]; ok {
			return ip.Structs[name]
		}
		return nil
	}
	if d, ok := pr.Structs[name]; ok {
		return d
	}
	var found *StructDef
	for _, ip := range pr.Alias {
		if d, ok := ip.Structs[name]; ok {
			found = d
		}
	}
	return found
}

func (t *Type) def() *StructDef {
	if t.Def == nil && t.Prog != nil {
		t.Def = lookupStruct(t.Prog, t.Pkg, t.Ref)
	}
	return t.Def
}

var (
	tI64  = &Type{Name: "i64"}
	tU8   = &Type{Name: "u8"}
	tU16  = &Type{Name: "u16"}
	tU32  = &Type{Name: "u32"}
	tU64  = &Type{Name: "u64"}
	tI32  = &Type{Name: "i32"}
	tBool = &Type{Name: "bool"}
	tVoid = &Type{Name: "void"}
	tNil  = &Type{Name: "nil"}
	tStr  = &Type{Name: "ptr", Elem: tI64}
)

func ptrTo(t *Type) *Type { return &Type{Name: "ptr", Elem: t} }

func (t *Type) String() string {
	if t.Name == "struct" {
		if t.Pkg != "" {
			return t.Pkg + "." + t.Ref
		}
		return t.Ref
	}
	if t.Name == "ptr" {
		return "ptr<" + t.Elem.String() + ">"
	}
	return t.Name
}

// intInfo: разрядность и знаковость целых типов
func intInfo(name string) (bits int, signed bool, ok bool) {
	switch name {
	case "i64":
		return 64, true, true
	case "u64":
		return 64, false, true
	case "i32":
		return 32, true, true
	case "u32":
		return 32, false, true
	case "u16":
		return 16, false, true
	case "u8":
		return 8, false, true
	}
	return 0, false, false
}

func (t *Type) isInt() bool {
	_, _, ok := intInfo(t.Name)
	return ok
}

func (t *Type) isU64() bool { return t.Name == "u64" }

// беззнаковые типы уже расширены нулями до 64 бит, поэтому для них shr
func (t *Type) isUnsigned() bool {
	_, signed, ok := intInfo(t.Name)
	return ok && !signed
}

// fitsInt: помещается ли константа v в целый тип t
func fitsInt(t *Type, v int64) bool {
	switch t.Name {
	case "u8":
		return v >= 0 && v <= 0xFF
	case "u16":
		return v >= 0 && v <= 0xFFFF
	case "u32":
		return v >= 0 && v <= 0xFFFFFFFF
	case "i32":
		return v >= -0x80000000 && v <= 0x7FFFFFFF
	}
	return true // i64, u64 (u64 допускает hex-литералы до 0xFFFF...)
}
func (t *Type) isPtr() bool { return t.Name == "ptr" }

// size — размер значения в памяти.
func (t *Type) size() int {
	switch t.Name {
	case "u8", "bool":
		return 1
	case "u16":
		return 2
	case "u32", "i32":
		return 4
	case "void":
		return 0
	case "struct":
		if d := t.def(); d != nil {
			return d.size()
		}
	}
	return 8
}

func sameType(a, b *Type) bool {
	if a.Name != b.Name {
		return false
	}
	if a.Name == "ptr" {
		return sameType(a.Elem, b.Elem)
	}
	if a.Name == "struct" {
		da, db := a.def(), b.def()
		if da != nil && db != nil {
			return da == db
		}
		return a.Ref == b.Ref && a.Pkg == b.Pkg
	}
	return true
}

// assignable: можно ли значение типа have подставить туда, где нужен want.
func assignable(want, have *Type) bool {
	if sameType(want, have) {
		return true
	}
	// расширение целых: шире по разрядам, и знаковый может принять беззнаковый
	wb, ws, wok := intInfo(want.Name)
	hb, hs, hok := intInfo(have.Name)
	if wok && hok && wb > hb && (ws || !hs) {
		return true
	}
	if want.isPtr() && have.Name == "nil" {
		return true
	}
	return false
}

func typeByName(n string) *Type {
	switch n {
	case "i64", "int":
		return tI64
	case "u8", "byte":
		return tU8
	case "u16":
		return tU16
	case "u32":
		return tU32
	case "u64":
		return tU64
	case "i32":
		return tI32
	case "bool":
		return tBool
	case "void":
		return tVoid
	}
	return nil
}
