package highlight

import (
	"go/scanner"
	"go/token"
)

// goPredeclaredTypes and goBuiltins are the identifiers Go treats specially
// but which the scanner reports as plain IDENT.
var goPredeclaredTypes = map[string]bool{
	"any": true, "bool": true, "byte": true, "comparable": true,
	"complex64": true, "complex128": true, "error": true, "float32": true,
	"float64": true, "int": true, "int8": true, "int16": true, "int32": true,
	"int64": true, "rune": true, "string": true, "uint": true, "uint8": true,
	"uint16": true, "uint32": true, "uint64": true, "uintptr": true,
}

var goBuiltins = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
	"true": true, "false": true, "iota": true, "nil": true,
}

// scanGo tokenises Go source with the standard library scanner.
//
// This is exact rather than heuristic: it handles backtick raw strings
// containing quotes, block comments containing code, escaped quotes, and
// unicode identifiers — the cases a regex highlighter reliably gets wrong.
func scanGo(src string) []span {
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))

	var s scanner.Scanner
	// Syntax errors are ignored: a diff may legitimately contain a file that
	// does not compile, and best-effort colouring still beats none.
	s.Init(file, []byte(src), func(token.Position, string) {}, scanner.ScanComments)

	var spans []span
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		// The scanner synthesises semicolons at line ends; they occupy no
		// source text and must not produce a span.
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}

		text := lit
		if text == "" {
			text = tok.String()
		}
		offset := file.Offset(pos)
		if offset < 0 || offset+len(text) > len(src) {
			continue
		}

		class := classifyGo(tok, lit)
		if class == "" {
			continue // ordinary identifiers and punctuation stay unstyled
		}
		spans = append(spans, span{start: offset, end: offset + len(text), class: class})
	}
	return spans
}

func classifyGo(tok token.Token, lit string) string {
	switch {
	case tok == token.COMMENT:
		return ClassComment
	case tok == token.STRING, tok == token.CHAR:
		return ClassString
	case tok == token.INT, tok == token.FLOAT, tok == token.IMAG:
		return ClassNumber
	case tok.IsKeyword():
		return ClassKeyword
	case tok == token.IDENT:
		if goPredeclaredTypes[lit] {
			return ClassType
		}
		if goBuiltins[lit] {
			return ClassBuiltin
		}
	}
	return ""
}
