package highlight

import (
	"path/filepath"
	"strings"
)

// langDef drives the fallback scanner for non-Go files. It is deliberately
// simple: enough that config and build files are not a wall of grey, without
// pretending to be a real parser.
type langDef struct {
	name         string
	lineComments []string
	blockStart   string
	blockEnd     string
	quotes       string // each byte is a string delimiter
	escape       bool   // backslash escapes inside strings
	keywords     map[string]bool
}

func words(s string) map[string]bool {
	m := make(map[string]bool)
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

var (
	langC = &langDef{
		name: "c", lineComments: []string{"//"}, blockStart: "/*", blockEnd: "*/",
		quotes: "\"'", escape: true,
	}
	langJava = &langDef{
		name: "java", lineComments: []string{"//"}, blockStart: "/*", blockEnd: "*/",
		quotes: "\"'", escape: true,
		keywords: words(`abstract assert boolean break byte case catch char class const continue
			default do double else enum extends final finally float for goto if implements import
			instanceof int interface long native new package private protected public return short
			static strictfp super switch synchronized this throw throws transient try void volatile
			while var record sealed permits yield true false null`),
	}
	langJS = &langDef{
		name: "javascript", lineComments: []string{"//"}, blockStart: "/*", blockEnd: "*/",
		quotes: "\"'`", escape: true,
		keywords: words(`async await break case catch class const continue debugger default delete
			do else export extends finally for from function get if import in instanceof let new of
			return set static super switch this throw try typeof var void while with yield true false
			null undefined interface type enum implements declare namespace readonly public private
			protected abstract as satisfies keyof infer`),
	}
	langPython = &langDef{
		name: "python", lineComments: []string{"#"}, quotes: "\"'", escape: true,
		keywords: words(`and as assert async await break class continue def del elif else except
			finally for from global if import in is lambda nonlocal not or pass raise return try
			while with yield True False None match case self`),
	}
	langShell = &langDef{
		name: "shell", lineComments: []string{"#"}, quotes: "\"'", escape: true,
		keywords: words(`if then else elif fi for while until do done case esac function return
			in select time coproc local export readonly declare unset shift source exit set trap`),
	}
	langYAML = &langDef{
		name: "yaml", lineComments: []string{"#"}, quotes: "\"'", escape: true,
		keywords: words(`true false null yes no on off`),
	}
	langJSON = &langDef{
		name: "json", quotes: "\"", escape: true,
		keywords: words(`true false null`),
	}
	langTOML = &langDef{
		name: "toml", lineComments: []string{"#"}, quotes: "\"'", escape: true,
		keywords: words(`true false`),
	}
	langSQL = &langDef{
		name: "sql", lineComments: []string{"--"}, blockStart: "/*", blockEnd: "*/",
		quotes: "\"'", escape: true,
		keywords: words(`SELECT FROM WHERE INSERT UPDATE DELETE CREATE DROP ALTER TABLE INDEX VIEW
			JOIN LEFT RIGHT INNER OUTER ON GROUP BY ORDER HAVING LIMIT OFFSET UNION ALL DISTINCT AS
			AND OR NOT NULL IS IN EXISTS BETWEEN LIKE VALUES SET INTO PRIMARY KEY FOREIGN REFERENCES
			DEFAULT CONSTRAINT UNIQUE CASCADE RETURNING WITH CASE WHEN THEN ELSE END`),
	}
	langProto = &langDef{
		name: "protobuf", lineComments: []string{"//"}, blockStart: "/*", blockEnd: "*/",
		quotes: "\"'", escape: true,
		keywords: words(`syntax package import option message enum service rpc returns repeated
			optional required reserved oneof map extend stream public weak true false
			double float int32 int64 uint32 uint64 sint32 sint64 fixed32 fixed64 bool string bytes`),
	}
	langMarkup = &langDef{
		name: "markup", blockStart: "<!--", blockEnd: "-->", quotes: "\"'",
	}
	langCSS = &langDef{
		name: "css", blockStart: "/*", blockEnd: "*/", quotes: "\"'", escape: true,
	}
	langMake = &langDef{
		name: "make", lineComments: []string{"#"}, quotes: "\"'", escape: true,
		keywords: words(`ifeq ifneq ifdef ifndef else endif include define endef export unexport
			override .PHONY`),
	}
	langDocker = &langDef{
		name: "dockerfile", lineComments: []string{"#"}, quotes: "\"'", escape: true,
		keywords: words(`FROM RUN CMD LABEL MAINTAINER EXPOSE ENV ADD COPY ENTRYPOINT VOLUME USER
			WORKDIR ARG ONBUILD STOPSIGNAL HEALTHCHECK SHELL AS`),
	}
)

var byExt = map[string]*langDef{
	".java": langJava, ".kt": langJava, ".kts": langJava, ".scala": langJava, ".groovy": langJava,
	".js": langJS, ".mjs": langJS, ".cjs": langJS, ".jsx": langJS,
	".ts": langJS, ".tsx": langJS,
	".py": langPython, ".pyi": langPython,
	".sh": langShell, ".bash": langShell, ".zsh": langShell, ".ksh": langShell,
	".yaml": langYAML, ".yml": langYAML,
	".json": langJSON, ".jsonc": langJS,
	".toml": langTOML, ".ini": langTOML, ".cfg": langTOML, ".conf": langTOML, ".properties": langTOML,
	".sql":   langSQL,
	".proto": langProto,
	".xml":   langMarkup, ".html": langMarkup, ".htm": langMarkup, ".svg": langMarkup, ".xsd": langMarkup,
	".css": langCSS, ".scss": langCSS, ".less": langCSS,
	".c": langC, ".h": langC, ".cc": langC, ".cpp": langC, ".hpp": langC, ".cxx": langC,
	".rs": langC, ".swift": langC, ".dart": langC, ".gradle": langJava,
}

var byName = map[string]*langDef{
	"makefile": langMake, "gnumakefile": langMake, "dockerfile": langDocker,
	".gitignore":    {name: "text", lineComments: []string{"#"}},
	".dockerignore": {name: "text", lineComments: []string{"#"}},
	".env":          langShell,
}

func lookupLang(path string) (*langDef, bool) {
	base := strings.ToLower(filepath.Base(path))
	if l, ok := byName[base]; ok {
		return l, true
	}
	if strings.HasPrefix(base, "dockerfile") {
		return langDocker, true
	}
	if l, ok := byExt[strings.ToLower(filepath.Ext(path))]; ok {
		return l, true
	}
	return nil, false
}

// scanGeneric walks the buffer once, tracking whether it is inside a string or
// block comment. Being stateful across newlines is the whole point: a naive
// per-line pass mis-colours every line after a multi-line comment opens.
func scanGeneric(src string, lang *langDef) []span {
	var spans []span
	i, n := 0, len(src)

	for i < n {
		// Block comment.
		if lang.blockStart != "" && strings.HasPrefix(src[i:], lang.blockStart) {
			end := strings.Index(src[i+len(lang.blockStart):], lang.blockEnd)
			stop := n
			if end >= 0 {
				stop = i + len(lang.blockStart) + end + len(lang.blockEnd)
			}
			spans = append(spans, span{i, stop, ClassComment})
			i = stop
			continue
		}

		// Line comment, running to end of line.
		if lc := matchLineComment(src[i:], lang); lc > 0 {
			stop := strings.IndexByte(src[i:], '\n')
			if stop < 0 {
				stop = n
			} else {
				stop += i
			}
			spans = append(spans, span{i, stop, ClassComment})
			i = stop
			continue
		}

		// String literal. Unterminated strings stop at the newline so one bad
		// quote cannot swallow the rest of the file.
		if c := src[i]; strings.IndexByte(lang.quotes, c) >= 0 {
			stop := scanString(src, i, c, lang.escape)
			spans = append(spans, span{i, stop, ClassString})
			i = stop
			continue
		}

		// Number.
		if isDigit(src[i]) && (i == 0 || !isIdentChar(src[i-1])) {
			j := i
			for j < n && (isDigit(src[j]) || src[j] == '.' || src[j] == 'x' ||
				src[j] == 'X' || isHex(src[j]) || src[j] == '_') {
				j++
			}
			spans = append(spans, span{i, j, ClassNumber})
			i = j
			continue
		}

		// Identifier, checked against the keyword table.
		if isIdentStart(src[i]) {
			j := i
			for j < n && isIdentChar(src[j]) {
				j++
			}
			if lang.keywords != nil && lang.keywords[src[i:j]] {
				spans = append(spans, span{i, j, ClassKeyword})
			}
			i = j
			continue
		}

		i++
	}
	return spans
}

func matchLineComment(s string, lang *langDef) int {
	for _, lc := range lang.lineComments {
		if strings.HasPrefix(s, lc) {
			return len(lc)
		}
	}
	return 0
}

func scanString(src string, start int, quote byte, escape bool) int {
	for j := start + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			if escape {
				j++
			}
		case '\n':
			if quote != '`' { // backtick strings may span lines
				return j
			}
		case quote:
			return j + 1
		}
	}
	return len(src)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isHex(c byte) bool   { return c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }
func isIdentStart(c byte) bool {
	return c == '_' || c == '.' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
func isIdentChar(c byte) bool { return isIdentStart(c) || isDigit(c) }
