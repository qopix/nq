package main

import (
	"bytes"
	"context"
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---------- вспомогательные функции ----------

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// compileSrc компилирует одиночный файл main.nq
func compileSrc(t *testing.T, src string) (*Gen, error) {
	t.Helper()
	dir := writeFiles(t, map[string]string{"main.nq": src})
	g, _, err := compileFile(filepath.Join(dir, "main.nq"))
	return g, err
}

func mustCompile(t *testing.T, src string) *Gen {
	t.Helper()
	g, err := compileSrc(t, src)
	if err != nil {
		t.Fatalf("ожидалась успешная компиляция, а вышла ошибка:\n%v\n--- исходник ---\n%s", err, src)
	}
	return g
}

func wantErr(t *testing.T, src, substr string) {
	t.Helper()
	_, err := compileSrc(t, src)
	if err == nil {
		t.Fatalf("ожидалась ошибка с %q, но компиляция прошла\n--- исходник ---\n%s", substr, src)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("ожидалась ошибка с %q, а получено:\n%v", substr, err)
	}
	if strings.Contains(err.Error(), "внутренняя ошибка") {
		t.Fatalf("внутренняя ошибка компилятора вместо понятного сообщения:\n%v", err)
	}
}

func canRun() bool { return runtime.GOOS == "linux" && runtime.GOARCH == "amd64" }

// buildBin собирает обычную программу и возвращает путь к ELF
func buildBin(t *testing.T, src string) string {
	t.Helper()
	g := mustCompile(t, src)
	code := g.Patch(elfBase + elfHdrSize)
	img := WriteELF(code, elfBase, g.Extra)
	path := filepath.Join(t.TempDir(), "prog")
	if err := os.WriteFile(path, img, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func runBin(t *testing.T, path string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("не удалось запустить программу: %v", err)
		}
	}
	return out.String(), code
}

// ---------- лексер ----------

func TestLexerPositions(t *testing.T) {
	toks := NewLexer("let x: u8 = 0xFF // комментарий\nvar y").Tokenize()
	want := []struct {
		val       string
		line, col int
	}{{"let", 1, 1}, {"x", 1, 5}, {":", 1, 6}, {"u8", 1, 8}, {"=", 1, 11}, {"0xFF", 1, 13}, {"var", 2, 1}, {"y", 2, 5}}
	if len(toks) != len(want)+1 {
		t.Fatalf("токенов %d, ожидалось %d", len(toks), len(want)+1)
	}
	for i, w := range want {
		if toks[i].Val != w.val || toks[i].Line != w.line || toks[i].Col != w.col {
			t.Errorf("токен %d: %+v, ожидалось %+v", i, toks[i], w)
		}
	}
}

func TestLexerErrors(t *testing.T) {
	wantErr(t, "$hight\nfn main() { print(\"abc) }", "незакрытая строка")
	wantErr(t, "$hight\nfn main() { let x = 5 ? 3 }", "неожиданный символ")
}

func TestDiagFormat(t *testing.T) {
	s := formatDiag("f.nq", 2, 5, "сообщение", []string{"a", "    bad"})
	if !strings.Contains(s, "f.nq:2:5: ошибка: сообщение") || !strings.Contains(s, "^") || !strings.Contains(s, "bad") {
		t.Fatalf("плохое форматирование диагностики:\n%s", s)
	}
}

// ---------- парсер и проверка типов ----------

func TestCompileErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"нет main", "$hight\nfn foo() {}", "нет функции main"},
		{"нет директивы", "fn main() {}", "должен начинаться с директивы"},
		{"не закрыта скобка", "$hight\nfn main() {", "не закрыта '{'"},
		{"функция дважды", "$hight\nfn a() {}\nfn a() {}\nfn main() {}", "объявлена дважды"},
		{"u8 переполнение", "$hight\nfn main() { let x: u8 = 300 }", "не помещается в u8"},
		{"присваивание let", "$hight\nfn main() { let x = 1\nx = 2 }", "объявлена через let"},
		{"условие не bool", "$hight\nfn main() { if 1 { } }", "должно быть bool"},
		{"неизвестная переменная", "$hight\nfn main() { puti(zzz) }", "неизвестная переменная"},
		{"неизвестная функция", "$hight\nfn main() { foo() }", "вызов неизвестной функции"},
		{"число аргументов", "$hight\nfn f(a: i64) {}\nfn main() { f(1, 2) }", "принимает аргументов"},
		{"break вне цикла", "$hight\nfn main() { break }", "break вне цикла"},
		{"return в void", "$hight\nfn f() -> void { return 1 }\nfn main() {}", "ничего не возвращает"},
		{"структура по значению", "$hight\ntype P struct { x: i64 }\nfn f(a: P) {}\nfn main() {}", "по значению"},
		{"неизвестный тип", "$hight\nfn f(a: Foo) {}\nfn main() {}", "неизвестный тип"},
		{"ptr в hight", "$hight\nfn main() { let a = cast(ptr<u8>, 5) }", "запрещено в $hight"},
		{"hight + kernel", "$hight\n$kernel\nfn main() {}", "нельзя вместе с $hight"},
		{"mid + boot", "$mid\n$boot\nfn main() {}", "нужен $low"},
		{"железо в обычной программе", "$mid\nfn main() { cli() }", "инструкция железа"},
		{"прерывание в обычной программе", "$mid\n@interrupt\nfn h() {}\nfn main() {}", "только в $kernel и $boot"},
		{"asm вне low", "$mid\nfn main() { asm(\"90\") }", "только в $low"},
		{"syscall в ядре", "$low\n$kernel\nfn main() { syscall(39) }", "недоступна в $kernel"},
		{"делить bool", "$hight\nfn main() { let a = true / false }", "нельзя применить"},
		{"неверное поле", "$hight\ntype P struct { x: i64 }\nfn main() { let p = make(P, 1)\nputi(p.zzz) }", "нет поля"},
		{"вызов обработчика", "$low\n$kernel\n@interrupt\nfn h() {}\nfn main() { h() }", "нельзя вызывать"},
		{"импорт не найден", "$hight\nimport \"нет/такого\"\nfn main() {}", "не найден импорт"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) { wantErr(t, c.src, c.want) })
	}
}

func TestCompileOK(t *testing.T) {
	cases := map[string]string{
		"арифметика":     "$hight\nfn main() { let a = 1 + 2 * 3 - 4 / 2 % 5\nputi(a) }",
		"битовые":        "$hight\nfn main() { puti((0xF0 | 0x0F) ^ 0xFF & ~1 << 2 >> 1) }",
		"типы":           "$hight\nfn main() { let a: i32 = -5\nlet b: u16 = 65535\nlet c: u32 = 1\nlet d: u64 = 0xFFFFFFFFFFFFFFFF\nputi(a) }",
		"ptr<ptr<u8>>":   "$mid\nfn f(p: ptr<ptr<u8>>) {}\nfn main() {}",
		"глобальные":     "$hight\nvar n: i64 = 1\nvar buf: [16]u8\nfn main() { n += 1\nbuf[0] = 1 }",
		"packed":         "$hight\n@packed\ntype H struct { a: u8, b: u32 }\nfn main() { puti(sizeof(H)) }",
		"больше 6":       "$hight\nfn f(a: i64, b: i64, c: i64, d: i64, e: i64, g: i64, h: i64, k: i64) -> i64 { return k }\nfn main() { puti(f(1,2,3,4,5,6,7,8)) }",
		"fnaddr callfn":  "$mid\nfn t(a: i64) -> i64 { return a }\nfn main() { puti(callfn(fnaddr(t), 5)) }",
		"ядро + прерыв.": "$low\n$kernel\n@interrupt\nfn h() {}\n@exception\nfn e(code: i64) {}\nfn main() { cli()\nhlt() }",
	}
	for name, src := range cases {
		src := src
		t.Run(name, func(t *testing.T) { mustCompile(t, src) })
	}
}

// ---------- импорты ----------

func TestImportsAndNamespaces(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"main.nq": "$hight\nimport \"util\"\nimport \"other\" as o\nfn main() { puti(util.twice(4) + o.twice(1)) }\n",
		"util.nq": "fn twice(x: i64) -> i64 { return x * 2 }\n",
		"other.nq": "fn twice(x: i64) -> i64 { return x * 20 }\n",
	})
	if _, _, err := compileFile(filepath.Join(dir, "main.nq")); err != nil {
		t.Fatalf("импорты с псевдонимом должны собираться: %v", err)
	}
	// одно и то же имя в двух импортах без префикса — ошибка
	dir = writeFiles(t, map[string]string{
		"main.nq": "$hight\nimport \"a\"\nimport \"b\"\nfn main() { puti(f()) }\n",
		"a.nq":    "fn f() -> i64 { return 1 }\n",
		"b.nq":    "fn f() -> i64 { return 2 }\n",
	})
	_, _, err := compileFile(filepath.Join(dir, "main.nq"))
	if err == nil || !strings.Contains(err.Error(), "в нескольких импортах") {
		t.Fatalf("ожидалась ошибка о неоднозначном имени, получено: %v", err)
	}
}

func TestImportCycle(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"main.nq": "$hight\nimport \"a\"\nfn main() {}\n",
		"a.nq":    "import \"b\"\nfn fa() {}\n",
		"b.nq":    "import \"a\"\nfn fb() {}\n",
	})
	_, _, err := compileFile(filepath.Join(dir, "main.nq"))
	if err == nil || !strings.Contains(err.Error(), "циклический импорт") {
		t.Fatalf("ожидался циклический импорт, получено: %v", err)
	}
}

func TestModeMismatchInImport(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"main.nq": "$hight\nimport \"lib\"\nfn main() {}\n",
		"lib.nq":  "$low\nfn f() {}\n",
	})
	_, _, err := compileFile(filepath.Join(dir, "main.nq"))
	if err == nil || !strings.Contains(err.Error(), "помечен $low") {
		t.Fatalf("ожидалась ошибка режима, получено: %v", err)
	}
}

// ---------- примеры из документации ----------

func TestExamplesCompile(t *testing.T) {
	var files []string
	for _, pat := range []string{"examples/*.nq", "examples/os/*.nq", "tests/qemu/*.nq"} {
		m, _ := filepath.Glob(pat)
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Fatal("не найдено ни одного примера")
	}
	for _, f := range files {
		f := f
		t.Run(f, func(t *testing.T) {
			if _, _, err := compileFile(f); err != nil {
				t.Fatalf("пример не компилируется:\n%v", err)
			}
		})
	}
}

// ---------- ELF ----------

func TestELFHeader(t *testing.T) {
	path := buildBin(t, "$hight\nfn main() { return 0 }")
	f, err := elf.Open(path)
	if err != nil {
		t.Fatalf("debug/elf не принял файл: %v", err)
	}
	defer f.Close()
	if f.Class != elf.ELFCLASS64 || f.Machine != elf.EM_X86_64 || f.Type != elf.ET_EXEC || f.Data != elf.ELFDATA2LSB {
		t.Fatalf("плохой заголовок: %v %v %v %v", f.Class, f.Machine, f.Type, f.Data)
	}
	if f.Entry != elfBase+elfHdrSize {
		t.Fatalf("точка входа 0x%x, ожидалась 0x%x", f.Entry, uint64(elfBase+elfHdrSize))
	}
	if len(f.Progs) != 1 || f.Progs[0].Type != elf.PT_LOAD || f.Progs[0].Flags != elf.PF_R|elf.PF_W|elf.PF_X {
		t.Fatalf("плохие сегменты: %+v", f.Progs)
	}
	p := f.Progs[0]
	if p.Memsz < p.Filesz || p.Vaddr != elfBase || p.Off != 0 {
		t.Fatalf("плохой сегмент: %+v", p)
	}
}

func TestKernelELF(t *testing.T) {
	g := mustCompile(t, "$low\n$kernel\nfn main() { hlt() }")
	code := g.Patch(kernelBase + elfHdrSize)
	img := WriteELF(code, kernelBase, g.Extra)
	path := filepath.Join(t.TempDir(), "k.elf")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Entry < 0xFFFFFFFF80000000 {
		t.Fatalf("ядро должно жить в старшей половине, точка входа 0x%x", f.Entry)
	}
	if f.Progs[0].Memsz < f.Progs[0].Filesz+stackSize {
		t.Fatalf("за образом ядра должен быть стек: %+v", f.Progs[0])
	}
}

// ---------- загрузчик ----------

func TestBootImage(t *testing.T) {
	g, _, err := compileFile("examples/os/kernel.nq")
	if err != nil {
		t.Fatal(err)
	}
	code := g.Patch(0x7E00)
	img := WriteBoot(code)
	if len(img)%512 != 0 {
		t.Fatalf("размер образа %d не кратен 512", len(img))
	}
	if img[510] != 0x55 || img[511] != 0xAA {
		t.Fatal("нет сигнатуры 55AA в конце загрузочного сектора")
	}
	sectors := int(img[bootSectorsOff]) | int(img[bootSectorsOff+1])<<8
	if sectors != len(img)/512-1 {
		t.Fatalf("в загрузчике записано %d секторов, а в образе %d", sectors, len(img)/512-1)
	}
	if !bytes.Equal(img[512:512+len(code)], code) {
		t.Fatal("программа в образе не совпадает с кодом")
	}
	if len(bootStub) > 510 {
		t.Fatalf("загрузчик не помещается в сектор: %d байт", len(bootStub))
	}
}

func TestBootTooBig(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("слишком большой образ должен вызывать ошибку")
		}
	}()
	WriteBoot(make([]byte, bootMaxSectors*512+1))
}

func TestInterruptStubBytes(t *testing.T) {
	g := mustCompile(t, "$low\n$kernel\n@interrupt\nfn h() {}\nfn main() { hlt() }")
	if !bytes.Contains(g.code, []byte{0x48, 0xCF}) {
		t.Fatal("в обработчике прерывания нет iretq (48 CF)")
	}
	g = mustCompile(t, "$low\n$kernel\n@exception\nfn e(code: i64) {}\nfn main() { hlt() }")
	if !bytes.Contains(g.code, []byte{0x48, 0x83, 0xC4, 0x08, 0x48, 0xCF}) {
		t.Fatal("в обработчике исключения нет add rsp,8; iretq")
	}
}

// ---------- запуск программ (только Linux x86-64) ----------

func TestRunPrograms(t *testing.T) {
	if !canRun() {
		t.Skip("запуск ELF возможен только на linux/amd64")
	}
	cases := []struct {
		name, src, out string
		code           int
	}{
		{"арифметика", "$hight\nfn main() {\nputi(6 * 7)\nprint(\"\\n\")\nreturn 3\n}", "42\n", 3},
		{"фибоначчи", "$hight\nfn fib(n: i64) -> i64 {\nif n < 2 { return n }\nreturn fib(n - 1) + fib(n - 2)\n}\nfn main() { puti(fib(10))\nprint(\"\\n\") }", "55\n", 0},
		{"указатели", "$hight\nfn main() {\nvar x = 41\nvar p = addr(x)\nderef(p) += 1\nputi(x)\nprint(\"\\n\")\n}", "42\n", 0},
		{"массив", "$hight\nfn main() {\nlet a = make(i64, 10)\nfor let i = 0; i < 10; i += 1 { a[i] = i * i }\nvar s = 0\nfor let i = 0; i < 10; i += 1 { s += a[i] }\nputi(s)\nprint(\"\\n\")\n}", "285\n", 0},
		{"u8 массив", "$hight\nfn main() {\nlet b = make(u8, 4)\nb[0] = 200\nb[1] = 55\nputi(b[0] + b[1])\nprint(\"\\n\")\n}", "255\n", 0},
		{"структуры", "$hight\ntype P struct { x: i64, y: i64 }\nfn d2(p: ptr<P>) -> i64 { return p.x * p.x + p.y * p.y }\nfn main() {\nlet p = make(P, 1)\np.x = 3\np.y = 4\nputi(d2(p))\nprint(\"\\n\")\nputi(sizeof(P))\nprint(\"\\n\")\n}", "25\n16\n", 0},
		{"упакованная структура", "$hight\n@packed\ntype H struct { a: u8, b: u32 }\ntype A struct { a: u8, b: u32 }\nfn main() { puti(sizeof(H))\nprint(\" \")\nputi(sizeof(A))\nprint(\"\\n\") }", "5 8\n", 0},
		{"глобальные", "$hight\nvar counter: i64 = 5\nfn inc() { counter += 1 }\nfn main() { inc()\ninc()\nputi(counter)\nprint(\"\\n\") }", "7\n", 0},
		{"битовые операции", "$hight\nfn main() {\nputi((0xF0 | 0x0F) ^ 0xFF)\nprint(\" \")\nputi(1 << 10)\nprint(\" \")\nputi(-8 >> 1)\nprint(\"\\n\")\n}", "0 1024 -4\n", 0},
		{"беззнаковые", "$hight\nfn main() {\nlet u: u64 = 0xFFFFFFFFFFFFFFFF\nputi(cast(i64, u >> 60))\nprint(\" \")\nif u > 1 { print(\"u\") } else { print(\"s\") }\nprint(\"\\n\")\n}", "15 u\n", 0},
		{"усечение u8", "$hight\nfn main() { let a: u8 = 250\nputi(cast(u8, a + 10))\nprint(\"\\n\") }", "4\n", 0},
		{"i32", "$hight\nfn main() { let x: i32 = -5\nputi(x)\nprint(\" \")\nputi(cast(i32, 4294967295))\nprint(\"\\n\") }", "-5 -1\n", 0},
		{"деление со знаком", "$hight\nfn main() { puti(-7 / 2)\nprint(\" \")\nputi(-7 % 2)\nprint(\"\\n\") }", "-3 -1\n", 0},
		{"8 параметров", "$hight\nfn s(a: i64, b: i64, c: i64, d: i64, e: i64, f: i64, g: i64, h: i64) -> i64 { return a + b + c + d + e + f + g + h }\nfn main() { puti(s(1, 2, 3, 4, 5, 6, 7, 8))\nprint(\"\\n\") }", "36\n", 0},
		{"короткое вычисление", "$hight\nfn boom() -> bool { exit(9)\nreturn true }\nfn main() { if false && boom() { print(\"bad\") }\nif true || boom() { print(\"ok\\n\") } }", "ok\n", 0},
		{"fnaddr callfn", "$mid\nfn t(a: i64, b: i64) -> i64 { return a * b }\nfn main() { puti(callfn(fnaddr(t), 6, 7))\nprint(\"\\n\") }", "42\n", 0},
		{"syscall write", "$mid\nimport \"std.os\"\nfn main() { sys_write(1, cbytes(\"hi\\n\"), 3) }", "hi\n", 0},
		{"std.str", "$hight\nimport \"std.str\"\nfn main() { if streq(\"abc\", \"abc\") && !streq(\"abc\", \"abd\") { print(\"ok\\n\") } }", "ok\n", 0},
		{"std.math", "$hight\nimport \"std.math\" as m\nfn main() { puti(m.pow(2, 10) + m.abs(-5) + m.max(1, 9))\nprint(\"\\n\") }", "1038\n", 0},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			path := buildBin(t, c.src)
			out, code := runBin(t, path)
			if out != c.out || code != c.code {
				t.Fatalf("вывод %q код %d, ожидалось %q код %d", out, code, c.out, c.code)
			}
		})
	}
}

func TestArgcArgv(t *testing.T) {
	if !canRun() {
		t.Skip("только linux/amd64")
	}
	src := "$mid\nimport \"std.os\"\nfn main() { puti(argc())\nprint(\" \")\nprint_cstr(argv(1))\nprint(\"\\n\") }"
	out, _ := runBin(t, buildBin(t, src), "hello", "world")
	if out != "3 hello\n" {
		t.Fatalf("вывод %q", out)
	}
}

// ---------- QEMU: загрузчик, ядро, прерывания ----------

func qemuBoot(t *testing.T, img []byte, extra ...string) (string, int) {
	t.Helper()
	q, err := exec.LookPath("qemu-system-x86_64")
	if err != nil {
		t.Skip("qemu-system-x86_64 не установлен")
	}
	path := filepath.Join(t.TempDir(), "boot.img")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-drive", "format=raw,file=" + path, "-serial", "stdio", "-display", "none",
		"-device", "isa-debug-exit,iobase=0xf4,iosize=0x04", "-no-reboot"}
	args = append(args, extra...)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, q, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return out.String(), code
}

func bootImage(t *testing.T, file string) []byte {
	t.Helper()
	g, _, err := compileFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return WriteBoot(g.Patch(0x7E00))
}

func TestQEMUBoot(t *testing.T) {
	out, code := qemuBoot(t, bootImage(t, "tests/qemu/boot_ok.nq"))
	for _, want := range []string{"NQ-BOOT-OK", "E820-OK", "INT3-OK", "DONE"} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе QEMU нет %q:\n%s", want, out)
		}
	}
	if code != 1 { // isa-debug-exit: (значение << 1) | 1
		t.Errorf("код выхода QEMU %d, ожидался 1", code)
	}
}

func TestQEMUBootLowMemory(t *testing.T) {
	out, _ := qemuBoot(t, bootImage(t, "tests/qemu/boot_ok.nq"), "-m", "8")
	if !strings.Contains(out, "NQ-BOOT-OK") {
		t.Errorf("при 8 МБ памяти загрузчик должен доходить до main:\n%s", out)
	}
}

func TestQEMUBootTruncatedImage(t *testing.T) {
	img := bootImage(t, "tests/qemu/boot_ok.nq")
	out, _ := qemuBoot(t, img[:512]) // оставили только загрузочный сектор: чтение диска должно провалиться
	if strings.Contains(out, "NQ-BOOT-OK") {
		t.Errorf("программа не должна запуститься из обрезанного образа:\n%s", out)
	}
}

// ---------- регрессии: кодировки инструкций ----------

// Ошибка из nq1: lea rdx,[rdx+rax*2] был закодирован как 48 8D 14 50 (= [rax+rdx*2]),
// из-за чего print в $kernel/$boot писал не в видеопамять.
func TestVGAStubAddressing(t *testing.T) {
	g := mustCompile(t, "$low\n$kernel\nfn main() { print(\"x\")\nputi(5) }")
	if bytes.Contains(g.code, []byte{0x48, 0x8D, 0x14, 0x50}) {
		t.Fatal("найден неверный lea rdx,[rax+rdx*2] (48 8D 14 50) в заглушках VGA")
	}
	if bytes.Count(g.code, []byte{0x48, 0x8D, 0x14, 0x42}) != 2 {
		t.Fatal("ожидалось два lea rdx,[rdx+rax*2] (48 8D 14 42): в print и в puti")
	}
}

// Байты проверены GNU as; тест не даёт им незаметно измениться.
func TestIntrinsicEncodings(t *testing.T) {
	cases := []struct {
		stmt string
		want []byte
	}{
		{"cli()", []byte{0xFA}},
		{"sti()", []byte{0xFB}},
		{"hlt()", []byte{0xF4}},
		{"pause()", []byte{0xF3, 0x90}},
		{"outb(0x80, 1)", []byte{0x89, 0xC2, 0x58, 0xEE}},
		{"outw(0x80, 1)", []byte{0x66, 0xEF}},
		{"outl(0x80, 1)", []byte{0xEF}},
		{"inb(0x60)", []byte{0x89, 0xC2, 0x31, 0xC0, 0xEC}},
		{"inw(0x60)", []byte{0x66, 0xED}},
		{"inl(0x60)", []byte{0xED}},
		{"readcr0()", []byte{0x0F, 0x20, 0xC0}},
		{"readcr2()", []byte{0x0F, 0x20, 0xD0}},
		{"readcr3()", []byte{0x0F, 0x20, 0xD8}},
		{"readcr4()", []byte{0x0F, 0x20, 0xE0}},
		{"writecr0(1)", []byte{0x0F, 0x22, 0xC0}},
		{"writecr3(1)", []byte{0x0F, 0x22, 0xD8}},
		{"writecr4(1)", []byte{0x0F, 0x22, 0xE0}},
		{"rdmsr(0x1B)", []byte{0x0F, 0x32, 0x48, 0xC1, 0xE2, 0x20, 0x48, 0x09, 0xD0}},
		{"wrmsr(0x1B, 1)", []byte{0x48, 0xC1, 0xEA, 0x20, 0x0F, 0x30}},
		{"lidt(1)", []byte{0x0F, 0x01, 0x18}},
		{"lgdt(1)", []byte{0x0F, 0x01, 0x10}},
		{"invlpg(1)", []byte{0x0F, 0x01, 0x38}},
		{"rdtsc()", []byte{0x0F, 0x31}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.stmt, func(t *testing.T) {
			g := mustCompile(t, "$low\n$kernel\nfn main() { "+c.stmt+" }")
			if !bytes.Contains(g.code, c.want) {
				t.Fatalf("в коде нет последовательности % X", c.want)
			}
		})
	}
}

// Дизассемблер objdump не должен находить в нашем коде недопустимых опкодов.
func TestDisassemblyHasNoBadOpcodes(t *testing.T) {
	od, err := exec.LookPath("objdump")
	if err != nil {
		t.Skip("objdump не установлен")
	}
	if ver, _ := exec.Command(od, "--version").CombinedOutput(); !strings.Contains(string(ver), "GNU objdump") {
		t.Skip("нужен GNU objdump (llvm-objdump не поддерживает -b binary)")
	}
	var files []string
	for _, pat := range []string{"examples/*.nq", "examples/os/*.nq", "tests/qemu/*.nq"} {
		m, _ := filepath.Glob(pat)
		files = append(files, m...)
	}
	for _, f := range files {
		f := f
		t.Run(f, func(t *testing.T) {
			g, _, cerr := compileFile(f)
			if cerr != nil {
				t.Fatal(cerr)
			}
			code := g.Patch(0x400078)
			path := filepath.Join(t.TempDir(), "code.bin")
			if err := os.WriteFile(path, code[:len(g.code)], 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(od, "-D", "-b", "binary", "-m", "i386:x86-64", "-M", "intel", path).CombinedOutput()
			if err != nil {
				t.Fatalf("objdump: %v\n%s", err, out)
			}
			if i := strings.Index(string(out), "(bad)"); i >= 0 {
				lo := i - 200
				if lo < 0 {
					lo = 0
				}
				t.Fatalf("objdump нашёл недопустимый опкод:\n%s", string(out)[lo:i+60])
			}
		})
	}
}

// Ошибка: константное выражение -8 >> 1 считалось логическим сдвигом (9223372036854775804 вместо -4).
func TestConstEvalShiftIsArithmetic(t *testing.T) {
	got, ok := constEval(&BinE{">>", &NumE{-8}, &NumE{1}}, nil)
	if !ok || got != -4 {
		t.Fatalf("-8 >> 1 = %d (ok=%v), ожидалось -4", got, ok)
	}
	got, ok = constEval(&BinE{"<<", &NumE{3}, &NumE{4}}, nil)
	if !ok || got != 48 {
		t.Fatalf("3 << 4 = %d, ожидалось 48", got)
	}
}
