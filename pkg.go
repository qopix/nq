package main

import (
	"archive/zip"
	"bytes"
	"embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Встроенная стандартная библиотека (написана на самом Nq) зашита в бинарник nqc.
//
//go:embed lib/std/*.nq
var stdFS embed.FS

// ---------- Пути ----------

// NQPKG — куда ставятся пакеты (как GOPATH у Go). По умолчанию ~/.nq/pkg
func pkgRoot() string {
	if v := os.Getenv("NQPKG"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".nq", "pkg")
}

// ---------- Импорты ----------

type srcFile struct {
	key     string // уникальный ключ (для "каждый файл один раз")
	display string // что показывать в ошибках
	data    []byte
	dir     string // откуда считать относительные импорты
	trusted bool   // встроенная stdlib
}

func fileSrc(path string) (*srcFile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	return &srcFile{key: abs, display: abs, data: data, dir: filepath.Dir(abs)}, nil
}

func importName(path string) string {
	p := strings.TrimSuffix(path, ".nq")
	if i := strings.LastIndexAny(p, "/."); i >= 0 {
		p = p[i+1:]
	}
	if i := strings.Index(p, "@"); i >= 0 {
		p = p[:i]
	}
	b := []byte(p)
	for i, c := range b {
		if !isAlpha(c) && !isDigit(c) {
			b[i] = '_'
		}
	}
	return string(b)
}

// resolveImport находит файл для import "path".
//
//	import "std.str"               встроенная библиотека (lib/std/str.nq внутри nqc)
//	import "util.nq" / "./util"    файл проекта, рядом с импортирующим файлом
//	import "sub/util"              файл в подпапке проекта
//	import "github.com/u/r"        пакет, установленный командой `nqc inst github.com/u/r`
func resolveImport(path, fromDir string) (*srcFile, error) {
	// 1. стандартная библиотека
	if strings.HasPrefix(path, "std.") || strings.HasPrefix(path, "std/") {
		name := strings.TrimPrefix(strings.TrimPrefix(path, "std."), "std/")
		name = strings.TrimSuffix(name, ".nq")
		if dir := os.Getenv("NQLIB"); dir != "" { // можно подменить на свою папку
			full := filepath.Join(dir, "std", name+".nq")
			if data, err := os.ReadFile(full); err == nil {
				return &srcFile{key: "std:" + name, display: full, data: data, dir: filepath.Dir(full), trusted: true}, nil
			}
		}
		data, err := stdFS.ReadFile("lib/std/" + name + ".nq")
		if err != nil {
			return nil, fmt.Errorf("нет встроенной библиотеки %q", path)
		}
		return &srcFile{key: "std:" + name, display: "<std." + name + ">", data: data, dir: fromDir, trusted: true}, nil
	}

	clean := strings.TrimSuffix(path, ".nq")

	// 2. файл проекта
	for _, c := range []string{
		filepath.Join(fromDir, clean+".nq"),
		filepath.Join(fromDir, clean, "main.nq"),
	} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return fileSrc(c)
		}
	}

	// 3. установленный пакет
	base := clean[strings.LastIndex(clean, "/")+1:]
	for _, c := range []string{
		filepath.Join(pkgRoot(), clean+".nq"),
		filepath.Join(pkgRoot(), clean, "main.nq"),
		filepath.Join(pkgRoot(), clean, base+".nq"),
		filepath.Join(pkgRoot(), clean, "lib.nq"),
	} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return fileSrc(c)
		}
	}

	hint := ""
	if strings.Contains(path, "/") && strings.Contains(strings.Split(path, "/")[0], ".") {
		hint = ". Установи его: nqc inst " + path
	}
	return nil, fmt.Errorf("не найден импорт %q (искал в проекте и в %s)%s", path, pkgRoot(), hint)
}

type loader struct {
	byKey  map[string]*Program
	order  []*Program
	nextID int
}

// loadProgram парсит главный файл и рекурсивно все импорты (каждый файл один раз).
// Результат: progs[0] — главный файл.
func loadProgram(mainFile string) ([]*Program, error) {
	src, err := fileSrc(mainFile)
	if err != nil {
		return nil, err
	}
	l := &loader{byKey: map[string]*Program{}}
	l.load(src)
	progs := l.order

	mainProg := progs[0]
	if mainProg.Mode == "" {
		return nil, fmt.Errorf("%s: файл должен начинаться с директивы $hight, $mid или $low", mainProg.Path)
	}
	for _, p := range progs[1:] {
		if p.Mode != "" && p.Mode != mainProg.Mode {
			return nil, fmt.Errorf("%s: файл помечен $%s, а программа — $%s", p.Path, p.Mode, mainProg.Mode)
		}
		if p.Target != "" {
			return nil, fmt.Errorf("%s: $%s можно указывать только в главном файле", p.Path, p.Target)
		}
	}
	return progs, nil
}

func (l *loader) load(src *srcFile) *Program {
	if p, ok := l.byKey[src.key]; ok {
		return p
	}
	prog := NewParser(NewLexer(string(src.data)).Tokenize(), src.display).parseProgram()
	prog.ID = fmt.Sprintf("p%d", l.nextID)
	l.nextID++
	prog.Trusted = src.trusted
	l.byKey[src.key] = prog
	l.order = append(l.order, prog)
	for _, f := range prog.Funcs {
		f.Prog = prog
	}

	for _, imp := range prog.Imports {
		dep, err := resolveImport(imp.Path, src.dir)
		if err != nil {
			panic(fmt.Sprintf("%s:%d: %v", src.display, imp.Line, err))
		}
		child := l.load(dep)
		alias := imp.Alias
		if alias == "" {
			alias = importName(imp.Path)
		}
		if _, dup := prog.Alias[alias]; dup {
			panic(fmt.Sprintf("%s:%d: имя импорта %q использовано дважды (используй: import \"...\" as другое)", src.display, imp.Line, alias))
		}
		prog.Alias[alias] = child
	}
	return prog
}

// ---------- nqc inst: установка библиотек ----------

const instUsage = `nqc inst — установка библиотек

  nqc inst                       просмотреть проект в текущей папке и скачать все недостающие библиотеки
  nqc inst project/ | main.nq    то же для указанной папки или файла
  nqc inst github.com/u/r[@тег]  поставить (или обновить) одну библиотеку и её зависимости
  nqc list                       что установлено
  nqc rm github.com/u/r          удалить
  nqc path                       папка с пакетами ($NQPKG)
`

// isRemote: похоже ли на путь внешней библиотеки (github.com/user/repo...)
func isRemote(path string) bool {
	p := strings.TrimPrefix(strings.TrimPrefix(path, "https://"), "http://")
	parts := strings.Split(strings.Trim(p, "/"), "/")
	return len(parts) >= 3 && strings.Contains(parts[0], ".")
}

// repoOf: из github.com/u/r@v1/sub/dir получить github.com/u/r@v1
func repoOf(path string) string {
	p := strings.TrimPrefix(strings.TrimPrefix(path, "https://"), "http://")
	parts := strings.Split(strings.Trim(p, "/"), "/")
	return strings.Join(parts[:3], "/")
}

func cmdInst(args []string) error {
	if len(args) == 0 {
		return instProject(".")
	}
	for _, a := range args {
		if _, err := os.Stat(a); err != nil && isRemote(a) {
			if err := instRepo(repoOf(a)); err != nil {
				return err
			}
			// зависимости только что поставленной библиотеки
			p := strings.TrimPrefix(strings.TrimPrefix(repoOf(a), "https://"), "http://")
			ref := ""
			if i := strings.LastIndex(p, "@"); i > 0 {
				p, ref = p[:i], p[i:]
			}
			if err := instProject(filepath.Join(pkgRoot(), filepath.FromSlash(p)+ref)); err != nil {
				return err
			}
			continue
		}
		if err := instProject(a); err != nil {
			return err
		}
	}
	return nil
}

func listNqFiles(root string) []string {
	var out []string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			n := info.Name()
			if p != root && (strings.HasPrefix(n, ".") || n == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".nq") {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// importsOf читает .nq файл и возвращает его импорты (ошибки разбора -> error)
func importsOf(path string) (imps []ImportDecl, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	prog := NewParser(NewLexer(string(data)).Tokenize(), path).parseProgram()
	return prog.Imports, nil
}

// instProject просматривает все .nq файлы проекта (и файлы скачанных библиотек),
// находит импорты внешних библиотек и сам скачивает всё, чего не хватает.
func instProject(target string) error {
	st, err := os.Stat(target)
	if err != nil {
		return err
	}
	var queue []string
	if st.IsDir() {
		queue = listNqFiles(target)
		if len(queue) == 0 {
			return fmt.Errorf("в %s нет ни одного .nq файла", target)
		}
	} else {
		queue = []string{target}
	}

	seen := map[string]bool{}
	tried := map[string]bool{}
	var missing []string
	installed := 0
	checked := 0

	for len(queue) > 0 {
		file := queue[0]
		queue = queue[1:]
		abs, _ := filepath.Abs(file)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		checked++

		imps, err := importsOf(abs)
		if err != nil {
			fmt.Fprintln(os.Stderr, "nqc inst: пропускаю файл с ошибкой:", err)
			continue
		}
		dir := filepath.Dir(abs)
		for _, imp := range imps {
			dep, err := resolveImport(imp.Path, dir)
			if err != nil && isRemote(imp.Path) {
				repo := repoOf(imp.Path)
				if !tried[repo] {
					tried[repo] = true
					fmt.Printf("nqc inst: %s нужен файлу %s\n", repo, filepath.Base(abs))
					if e := instRepo(repo); e != nil {
						fmt.Fprintln(os.Stderr, "nqc inst: не удалось установить", repo+":", e)
					} else {
						installed++
					}
				}
				dep, err = resolveImport(imp.Path, dir)
			}
			if err != nil {
				missing = append(missing, fmt.Sprintf("%s:%d: %s", abs, imp.Line, imp.Path))
				continue
			}
			if !dep.trusted && !strings.HasPrefix(dep.key, "std:") {
				queue = append(queue, dep.key) // зависимости зависимостей
			}
		}
	}

	fmt.Printf("nqc inst: просмотрено файлов: %d, установлено библиотек: %d\n", checked, installed)
	if len(missing) > 0 {
		return fmt.Errorf("не найдены импорты:\n  %s", strings.Join(missing, "\n  "))
	}
	fmt.Println("nqc inst: все импорты на месте")
	return nil
}

func instRepo(src string) error {
	path, ref := src, ""
	if i := strings.LastIndex(src, "@"); i > 0 {
		path, ref = src[:i], src[i+1:]
	}
	path = strings.TrimPrefix(strings.TrimPrefix(path, "https://"), "http://")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 || !strings.Contains(parts[0], ".") {
		return fmt.Errorf("путь пакета должен быть вида github.com/user/repo[@ветка]")
	}
	host, owner := parts[0], parts[1]
	repo := strings.TrimSuffix(parts[2], ".git")

	dst := filepath.Join(pkgRoot(), host, owner, repo)
	if ref != "" {
		dst += "@" + ref
	}
	if _, err := os.Stat(dst); err == nil {
		fmt.Println("nqc inst: обновляю", dst)
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	url := "https://" + host + "/" + owner + "/" + repo + ".git"
	if _, err := exec.LookPath("git"); err == nil {
		fmt.Printf("nqc inst: git clone %s -> %s\n", url, dst)
		args := []string{"clone", "--depth", "1"}
		if ref != "" {
			args = append(args, "--branch", ref)
		}
		args = append(args, url, dst)
		cmd := exec.Command("git", args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git clone не удался: %v", err)
		}
	} else if host == "github.com" {
		fmt.Printf("nqc inst: git не найден, скачиваю архив github.com/%s/%s -> %s\n", owner, repo, dst)
		if err := downloadGithubZip(owner, repo, ref, dst); err != nil {
			os.RemoveAll(dst)
			return err
		}
	} else {
		return fmt.Errorf("для %s нужен установленный git", host)
	}

	if !hasNq(dst) {
		fmt.Println("nqc inst: предупреждение: в пакете нет ни одного .nq файла")
	}
	name := host + "/" + owner + "/" + repo
	if ref != "" {
		name += "@" + ref
	}
	fmt.Printf("nqc inst: готово. Теперь можно писать: import \"%s\"\n", name)
	return nil
}

func hasNq(dir string) bool {
	found := false
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".nq") {
			found = true
		}
		return nil
	})
	return found
}

func downloadGithubZip(owner, repo, ref, dst string) error {
	var urls []string
	base := "https://codeload.github.com/" + owner + "/" + repo + "/zip/"
	if ref != "" {
		urls = []string{base + "refs/heads/" + ref, base + "refs/tags/" + ref}
	} else {
		urls = []string{base + "refs/heads/main", base + "refs/heads/master"}
	}
	client := &http.Client{Timeout: 90 * time.Second}
	var data []byte
	var lastErr error
	for _, u := range urls {
		resp, err := client.Get(u)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			lastErr = fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
			continue
		}
		data, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		lastErr = nil
		break
	}
	if data == nil {
		return fmt.Errorf("не удалось скачать пакет: %v", lastErr)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	root := filepath.Clean(dst) + string(os.PathSeparator)
	for _, f := range zr.File {
		// отрезаем верхнюю папку архива (repo-main/)
		name := f.Name
		if i := strings.Index(name, "/"); i >= 0 {
			name = name[i+1:]
		} else {
			continue
		}
		if name == "" {
			continue
		}
		out := filepath.Join(dst, filepath.FromSlash(name))
		if !strings.HasPrefix(out+string(os.PathSeparator), root) { // защита от ../
			return fmt.Errorf("небезопасный путь в архиве: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(out, 0o755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		w, err := os.Create(out)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(w, rc)
		rc.Close()
		w.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func instList() error {
	root := pkgRoot()
	var out []string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}
		if info.Name() == ".git" {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(root, p)
		if strings.Count(rel, string(os.PathSeparator)) == 2 { // host/owner/repo
			out = append(out, filepath.ToSlash(rel))
			return filepath.SkipDir
		}
		return nil
	})
	sort.Strings(out)
	if len(out) == 0 {
		fmt.Println("nqc inst: пока ничего не установлено (", root, ")")
		return nil
	}
	for _, o := range out {
		fmt.Println(o)
	}
	return nil
}

func instRemove(path string) error {
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return fmt.Errorf("плохой путь пакета: %s", path)
	}
	dst := filepath.Join(pkgRoot(), clean)
	if _, err := os.Stat(dst); err != nil {
		return fmt.Errorf("пакет %s не установлен", path)
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	fmt.Println("nqc inst: удалён", path)
	return nil
}
