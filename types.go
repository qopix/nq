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
}

func (d *StructDef) size() int {
	if len(d.Fields) == 0 {
		return 8
	}
	return 8 * len(d.Fields)
}

func (d *StructDef) field(name string) *Field {
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

func (t *Type) isInt() bool { return t.Name == "i64" || t.Name == "u8" }
func (t *Type) isPtr() bool { return t.Name == "ptr" }

// size — размер значения в памяти.
func (t *Type) size() int {
	switch t.Name {
	case "u8", "bool":
		return 1
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
	if want.Name == "i64" && have.Name == "u8" {
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
	case "bool":
		return tBool
	case "void":
		return tVoid
	}
	return nil
}
