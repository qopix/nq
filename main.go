package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
)

const version = "0.4.4"

// логотип Nq: чёрный бейдж с буквами и фигурные скобки по бокам
const logo = ` ╭────────────╮
 │ █▄ █ ▄▀▀▄  │}
 │ █ ▀█ ▀▄▄█ ^│)
{│         █ *│
(╰────────────╯
`

const usage = logo + usageBody

const usageBody = `nqc — компилятор языка Nq (без C-прослоек, свой ELF-writer)

Использование:
  nqc build <file.nq> [-o out]   собрать
  nqc run   <file.nq>            собрать и сразу запустить (если это обычная программа)
  nqc check <file.nq>            проверить синтаксис и типы, ничего не писать
  nqc keys                       показать 25 ключевых слов
  nqc version                    версия и логотип
  nqc inst [папка|файл]          найти все импорты проекта и скачать недостающие библиотеки
  nqc inst github.com/u/r[@тег]  поставить одну библиотеку (и её зависимости)
  nqc list | rm <путь> | path    установленные библиотеки

Режим и цель задаются в НАЧАЛЕ исходника, не флагами:
  $hight   обычная программа, всегда запускается на ПК: ./prog
  $mid     то же + небезопасные операции (cast ptr<->число, арифметика указателей);
           с $kernel — ядро ОС (на ПК через ./ не запустится)
  $low     всё из $mid + asm("hex-байты");
           с $kernel — ядро, с $boot — загрузчик (образ диска для QEMU/BIOS)
  $kernel / $boot можно писать второй строкой или на той же: "$low boot"
`

var keywordList = []string{"fn", "let", "var", "const", "if", "else", "while", "for",
	"return", "break", "continue", "import", "as", "type", "struct", "ptr",
	"deref", "addr", "sizeof", "cast", "extern", "asm", "true", "false", "nil"}

func main() {
	args := os.Args[1:]
	var err error

	if len(args) == 0 {
		fmt.Print(usage)
		os.Exit(1)
	} else {
		switch args[0] {
		case "build":
			err = cmdBuild(args[1:], false)
		case "run":
			err = cmdBuild(args[1:], true)
		case "check":
			err = cmdCheck(args[1:])
		case "inst", "install", "get":
			err = cmdInst(args[1:])
		case "list":
			err = instList()
		case "rm":
			if len(args) < 2 {
				err = fmt.Errorf("nqc rm <github.com/user/repo>")
			} else {
				err = instRemove(args[1])
			}
		case "path":
			fmt.Println(pkgRoot())
		case "version", "-v", "--version":
			fmt.Printf("%snqc %s — язык Nq, режимы $hight / $mid / $low\n", logo, version)
		case "keys":
			fmt.Printf("Nq: %d ключевых слов:\n%s\n", len(keywordList), strings.Join(keywordList, " "))
		case "-h", "--help", "help":
			fmt.Print(usage)
		default:
			fmt.Print(usage)
			os.Exit(1)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "nqc: ошибка:", err)
		os.Exit(1)
	}
}

// recoverErr превращает панику в ошибку. Наши собственные сообщения (строки) показываем как есть;
// всё остальное — ошибка в самом компиляторе: говорим об этом прямо, стек по NQ_DEBUG=1.
func recoverErr(err *error) {
	r := recover()
	if r == nil {
		return
	}
	if s, ok := r.(string); ok {
		*err = fmt.Errorf("%s", s)
		return
	}
	if os.Getenv("NQ_DEBUG") != "" {
		fmt.Fprintln(os.Stderr, string(debug.Stack()))
	}
	*err = fmt.Errorf("внутренняя ошибка компилятора: %v\n  это баг nqc, а не твоего кода; запусти с NQ_DEBUG=1 и сообщи: https://github.com/qopix/nq/issues", r)
}

// compileFile: загрузить, проверить и скомпилировать. Паники парсера/генератора -> error.
func compileFile(file string) (g *Gen, progs []*Program, err error) {
	defer recoverErr(&err)
	progs, err = loadProgram(file)
	if err != nil {
		return nil, nil, err
	}
	return Compile(progs), progs, nil
}

func cmdCheck(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("укажи файл .nq")
	}
	g, progs, err := compileFile(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("OK: $%s, цель %s, файлов: %d, код %d байт\n", progs[0].Mode, g.target, len(progs), len(g.code))
	return nil
}

func cmdBuild(args []string, run bool) (err error) {
	defer recoverErr(&err)
	if len(args) == 0 {
		return fmt.Errorf("укажи файл .nq")
	}
	file := args[0]
	out := ""
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "-o" && i+1 < len(args):
			out = args[i+1]
			i++
		default:
			return fmt.Errorf("неизвестный аргумент %s (режим задаётся директивой $... в начале файла)", args[i])
		}
	}

	g, progs, err := compileFile(file)
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(file, ".nq")

	var img []byte
	var kind string
	switch g.target {
	case "kernel":
		if out == "" {
			out = base + ".elf"
		}
		code := g.Patch(kernelBase + elfHdrSize)
		img = WriteELF(code, kernelBase, g.Extra)
		kind = "ядро ELF64 (нужен загрузчик)"
	case "boot":
		if out == "" {
			out = base + ".img"
		}
		code := g.Patch(0x7E00)
		if top := 0x7E00 + uint64(len(code)) + g.Extra; top > 0x9F000 {
			return fmt.Errorf("$boot: программа вместе с bss и стеком не помещается в память до 0x9F000 (нужно до 0x%X)", top)
		}
		img = WriteBoot(code)
		kind = "образ загрузочного диска"
	default:
		if out == "" {
			out = base
		}
		code := g.Patch(elfBase + elfHdrSize)
		img = WriteELF(code, elfBase, g.Extra)
		kind = "программа ELF64"
	}
	if err := os.WriteFile(out, img, 0o755); err != nil {
		return err
	}
	fmt.Printf("nqc: собрано %s ($%s, %s, %d байт)\n", out, progs[0].Mode, kind, len(img))

	if run {
		switch g.target {
		case "boot":
			q, lerr := exec.LookPath("qemu-system-x86_64")
			if lerr != nil {
				return fmt.Errorf("это загрузчик, через ./ он не запускается. Поставь qemu-system-x86_64 и запусти: qemu-system-x86_64 -drive format=raw,file=%s -serial stdio", out)
			}
			qargs := []string{"-drive", "format=raw,file=" + out, "-serial", "stdio"}
			if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
				qargs = append(qargs, "-display", "none")
			}
			cmd := exec.Command(q, qargs...)
			cmd.Stdout, cmd.Stdin, cmd.Stderr = os.Stdout, os.Stdin, os.Stderr
			return cmd.Run()
		case "kernel":
			return fmt.Errorf("это ядро ОС, через ./ оно не запускается: его должен загрузить загрузчик")
		}
		abs, _ := filepath.Abs(out)
		cmd := exec.Command(abs)
		if runtime.GOARCH != "amd64" {
			// телефон/ARM: программы Nq — x86-64, запускаем через эмулятор
			q, lerr := exec.LookPath("qemu-x86_64")
			if lerr != nil {
				return fmt.Errorf("эта машина не x86-64 (%s), а Nq собирает x86-64. Поставь эмулятор: pkg install qemu-user-x86-64 (Termux) или apt install qemu-user", runtime.GOARCH)
			}
			cmd = exec.Command(q, abs)
		}
		cmd.Stdout, cmd.Stdin, cmd.Stderr = os.Stdout, os.Stdin, os.Stderr
		return cmd.Run()
	}
	return nil
}
