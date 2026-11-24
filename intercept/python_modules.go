package intercept

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

var pythonVersionName = regexp.MustCompile(`^python([0-9]+\.[0-9]+)(?:[a-z]*)$`)
var pythonModuleName = regexp.MustCompile(`^[A-Za-z_][A-Za-z_0-9]*(?:\.[A-Za-z_][A-Za-z_0-9]*)*$`)

// Find module entry sources from the interpreter's filesystem layout. Never
// execute Python, import the requested module, or run site/startup hooks.
func pythonModuleReferences(name, module, dir string, args, wrapperArgs []*syntax.Word, call *syntax.CallExpr, generated map[string]scriptReference, constants map[string]shellConstant) []scriptReference {
	problem := func(message string) []scriptReference { return []scriptReference{{problem: message}} }
	if !pythonModuleName.MatchString(module) {
		return problem("Python 모듈 이름을 정적으로 해석할 수 없음")
	}
	env := map[string]string{}
	for _, key := range []string{"PATH", "PYTHONPATH", "PYTHONHOME", "PYTHONUSERBASE", "HOME", "PYTHONSAFEPATH"} {
		env[key] = os.Getenv(key)
	}
	valueOf := func(word *syntax.Word) (string, bool) {
		if value, known := staticWord(word); known {
			return value, true
		}
		fields, known := constantWordFields(word, constants)
		if known && len(fields) == 1 {
			return fields[0], true
		}
		return "", false
	}
	for _, assign := range call.Assigns {
		if assign.Name == nil {
			continue
		}
		if _, relevant := env[assign.Name.Value]; !relevant {
			continue
		}
		value, known := valueOf(assign.Value)
		if assign.Value == nil {
			value, known = "", true
		}
		if !known {
			return problem("Python 모듈 검색 경로에 알 수 없는 환경 확장이 있음")
		}
		env[assign.Name.Value] = value
	}
	for _, word := range wrapperArgs {
		value, known := valueOf(word)
		if !known {
			continue
		}
		// These wrappers can change PATH/HOME or discard/unset inherited
		// Python search settings. Do not resolve against the parent process
		// environment when the child's environment is not modeled.
		switch value {
		case "sudo", "-i", "--ignore-environment", "-u", "--unset", "-c", "-p":
			return problem("래퍼 명령이 Python 모듈 실행 환경을 바꿔 소스 진입점을 확인할 수 없음")
		}
		if strings.HasPrefix(value, "--unset=") || filepath.Base(value) == "sudo" {
			return problem("래퍼 명령이 Python 모듈 실행 환경을 바꿔 소스 진입점을 확인할 수 없음")
		}
		key, value, assignment := strings.Cut(value, "=")
		if assignment {
			if _, relevant := env[key]; relevant {
				env[key] = value
			}
		}
	}
	isolated, ignoreEnv, safePath, noSite, noUser := false, false, false, false, false
	for i := 1; i < len(args); i++ {
		value, known := staticWord(args[i])
		if !known || value == "-m" {
			break
		}
		if !strings.HasPrefix(value, "-") || strings.HasPrefix(value, "--") {
			continue
		}
		flags := strings.TrimPrefix(value, "-")
	flagsLoop:
		for j := 0; j < len(flags); j++ {
			switch flags[j] {
			case 'I':
				isolated = true
			case 'E':
				ignoreEnv = true
			case 'P':
				safePath = true
			case 'S':
				noSite = true
			case 's':
				noUser = true
			case 'W', 'X':
				if j == len(flags)-1 {
					i++
				}
				break flagsLoop
			}
		}
	}
	ignoreEnv = ignoreEnv || isolated
	safePath = safePath || isolated || (!ignoreEnv && env["PYTHONSAFEPATH"] != "")
	if !safePath && dir == "" {
		return problem("실행 디렉터리가 불확실해 실제 Python 모듈을 확인할 수 없음")
	}
	executable := name
	if !strings.Contains(name, "/") {
		if env["PATH"] == os.Getenv("PATH") {
			var err error
			executable, err = exec.LookPath(name)
			if err != nil {
				return problem("Python 인터프리터와 모듈 소스를 찾을 수 없음")
			}
		} else {
			executable = ""
			for _, entry := range filepath.SplitList(env["PATH"]) {
				if !filepath.IsAbs(entry) {
					if dir == "" {
						return problem("Python 인터프리터 검색 경로가 알 수 없는 디렉터리에 의존함")
					}
					entry = filepath.Join(dir, entry)
				}
				candidate := filepath.Join(entry, name)
				if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 {
					executable = candidate
					break
				}
			}
			if executable == "" {
				return problem("Python 인터프리터와 모듈 소스를 찾을 수 없음")
			}
		}
	} else if !filepath.IsAbs(executable) {
		if dir == "" {
			return problem("Python 인터프리터 경로가 알 수 없는 디렉터리에 의존함")
		}
		executable = filepath.Join(dir, executable)
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return problem("Python 인터프리터 경로를 읽을 수 없음")
	}
	if !nativeInterpreterFile(resolved) {
		return problem("Python 진입점이 확정 가능한 로컬 인터프리터가 아니어서 모듈을 정적으로 찾을 수 없음")
	}
	version := pythonVersionName.FindStringSubmatch(filepath.Base(resolved))
	if len(version) != 2 {
		version = pythonVersionName.FindStringSubmatch(filepath.Base(executable))
	}
	if len(version) != 2 {
		return problem("Python 버전과 모듈 디렉터리를 정적으로 확정할 수 없음")
	}
	prefix := filepath.Dir(filepath.Dir(executable))
	base := filepath.Dir(filepath.Dir(resolved))
	library := filepath.Join(base, "lib", "python"+version[1])
	if !ignoreEnv && (env["PYTHONHOME"] != "" || env["PYTHONUSERBASE"] != "") {
		return problem("Python 사용자 정의 라이브러리 디렉터리가 확정 가능한 모듈 출처를 제공하지 않음")
	}
	roots := []string{}
	seen := map[string]bool{}
	addRoot := func(root string) {
		root = filepath.Clean(root)
		if !seen[root] {
			roots = append(roots, root)
			seen[root] = true
		}
	}
	if !safePath {
		addRoot(dir)
	}
	if !ignoreEnv {
		for _, root := range filepath.SplitList(env["PYTHONPATH"]) {
			if root == "" {
				continue
			}
			if !filepath.IsAbs(root) {
				if dir == "" {
					return problem("Python 모듈 검색 경로가 알 수 없는 디렉터리에 의존함")
				}
				root = filepath.Join(dir, root)
			}
			addRoot(root)
		}
	}
	addRoot(library)
	venv := false
	includeSystem := true
	if config, err := readScript(filepath.Join(prefix, "pyvenv.cfg")); err == nil {
		venv = true
		includeSystem = false
		for _, line := range strings.Split(string(config), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok && strings.TrimSpace(key) == "include-system-site-packages" {
				includeSystem = strings.TrimSpace(value) == "true"
			}
		}
	}
	if !noSite {
		if !isolated && !noUser && (!venv || includeSystem) && env["HOME"] != "" {
			addRoot(filepath.Join(env["HOME"], ".local", "lib", "python"+version[1], "site-packages"))
		}
		if venv {
			addRoot(filepath.Join(prefix, "lib", "python"+version[1], "site-packages"))
		}
		if !venv || includeSystem {
			addRoot(filepath.Join(library, "site-packages"))
			// Debian's system interpreter adds dist-packages; a /usr/local Python
			// does not inherit those paths merely because they exist.
			if site, err := readScript(filepath.Join(library, "site.py")); err == nil && strings.Contains(string(site), "dist-packages") {
				addRoot(filepath.Join("/usr/local/lib", "python"+version[1], "dist-packages"))
				addRoot(filepath.Join(base, "lib", "python3", "dist-packages"))
				addRoot(filepath.Join(library, "dist-packages"))
			}
		}
	}
	refs, err := moduleEntryReferences(module, roots, generated)
	if err != nil {
		return problem(err.Error())
	}
	return refs
}

func moduleEntryReferences(module string, roots []string, generated map[string]scriptReference) ([]scriptReference, error) {
	exists := func(path string) bool {
		if _, ok := generated[path]; ok {
			return true
		}
		info, err := os.Stat(path)
		return err == nil && info.Mode().IsRegular()
	}
	reference := func(path string) scriptReference {
		if ref, ok := generated[path]; ok {
			return ref
		}
		return scriptReference{path: path}
	}
	parts := strings.Split(module, ".")
	for _, root := range roots {
		var refs []scriptReference
		dir := root
		matched := true
		for i, part := range parts {
			packageDir := filepath.Join(dir, part)
			init := filepath.Join(packageDir, "__init__.py")
			if exists(init) {
				refs = append(refs, reference(init))
				dir = packageDir
				if i == len(parts)-1 {
					main := filepath.Join(dir, "__main__.py")
					if !exists(main) {
						return nil, fmt.Errorf("Python 모듈 %s 에 읽을 수 있는 __main__.py 가 없음", module)
					}
					refs = append(refs, reference(main))
					return refs, nil
				}
				continue
			}
			source := filepath.Join(dir, part+".py")
			if exists(source) {
				if i != len(parts)-1 {
					return nil, fmt.Errorf("Python 모듈 %s 의 상위 모듈이 읽을 수 있는 패키지가 아님", module)
				}
				refs = append(refs, reference(source))
				return refs, nil
			}
			// A regular parent package shadows later search roots even when the
			// requested child is absent; do not substitute an unrelated module.
			if len(refs) > 0 {
				return nil, fmt.Errorf("Python 모듈 %s 의 소스를 찾을 수 없음", module)
			}
			matched = false
			break
		}
		if matched {
			return refs, nil
		}
	}
	return nil, fmt.Errorf("Python 모듈 %s 의 소스를 찾을 수 없음", module)
}

func nativeInterpreterFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	header := make([]byte, 4)
	if _, err := io.ReadFull(file, header); err != nil {
		return false
	}
	switch string(header) {
	case "\x7fELF", "\xfe\xed\xfa\xce", "\xfe\xed\xfa\xcf", "\xce\xfa\xed\xfe", "\xcf\xfa\xed\xfe", "\xca\xfe\xba\xbe", "\xbe\xba\xfe\xca":
		return true
	}
	return header[0] == 'M' && header[1] == 'Z'
}
