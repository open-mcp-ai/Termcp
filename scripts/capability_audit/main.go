// Command capability_audit regenerates compact source and permission catalogs
// under dist/audits. Large generated call inventories are build artifacts, not
// source files.
package main

import (
	"crypto/sha256"
	"encoding/csv"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/authorization"
)

type sourceFile struct {
	path    string
	lines   int
	digest  string
	methods int
	calls   int
}

type functionRow struct {
	file, line, name, receiver string
}

type callRow struct {
	file, line, caller, callee string
}

func main() {
	out := flag.String("out", filepath.Join("dist", "audits"), "output directory")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	files, functions, calls := scan(".", func(path string) bool {
		path = filepath.ToSlash(path)
		return path == ".git" || path == "dist" || path == ".idea" || path == ".pi-tools"
	})
	if err := writeSourceCSV(filepath.Join(out, "source-files.csv"), files); err != nil {
		return err
	}
	if err := writeFunctionCSV(filepath.Join(out, "functions.csv"), functions); err != nil {
		return err
	}
	if err := writeCallCSV(filepath.Join(out, "call-expressions.csv"), calls); err != nil {
		return err
	}
	if err := writeCatalogCSV(filepath.Join(out, "permission-catalog.csv")); err != nil {
		return err
	}
	return writeSummary(filepath.Join(out, "summary.md"), files, functions, calls)
}

func scan(root string, skip func(string) bool) ([]sourceFile, []functionRow, []callRow) {
	var files []sourceFile
	var functions []functionRow
	var calls []callRow
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if path != root && skip(filepath.ToSlash(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, raw, 0)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(raw)
		record := sourceFile{
			path: filepath.ToSlash(path), lines: strings.Count(string(raw), "\n"),
			digest: fmt.Sprintf("%x", sum),
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := fn.Name.Name
			receiver := receiverName(fn.Recv)
			full := name
			if receiver != "" {
				full = receiver + "." + name
				record.methods++
			}
			functions = append(functions, functionRow{
				file: record.path, line: strconv.Itoa(fset.Position(fn.Pos()).Line),
				name: name, receiver: receiver,
			})
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || call.Fun == nil {
					return true
				}
				calls = append(calls, callRow{
					file: record.path, line: strconv.Itoa(fset.Position(call.Pos()).Line),
					caller: full, callee: calleeName(call.Fun),
				})
				record.calls++
				return true
			})
		}
		files = append(files, record)
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	sort.Slice(functions, func(i, j int) bool {
		if functions[i].file != functions[j].file {
			return functions[i].file < functions[j].file
		}
		return functions[i].line < functions[j].line
	})
	sort.Slice(calls, func(i, j int) bool {
		if calls[i].file != calls[j].file {
			return calls[i].file < calls[j].file
		}
		if calls[i].line != calls[j].line {
			return calls[i].line < calls[j].line
		}
		return calls[i].callee < calls[j].callee
	})
	return files, functions, calls
}

func receiverName(fields *ast.FieldList) string {
	if fields == nil || len(fields.List) == 0 {
		return ""
	}
	var out strings.Builder
	_ = formatNode(&out, fields.List[0].Type)
	return out.String()
}

func calleeName(expr ast.Expr) string {
	switch value := expr.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return calleeName(value.X) + "." + value.Sel.Name
	case *ast.IndexExpr:
		return calleeName(value.X)
	case *ast.IndexListExpr:
		return calleeName(value.X)
	case *ast.ParenExpr:
		return calleeName(value.X)
	case *ast.StarExpr:
		return calleeName(value.X)
	default:
		return fmt.Sprintf("%T", expr)
	}
}

func formatNode(w *strings.Builder, expr ast.Expr) error {
	switch value := expr.(type) {
	case *ast.StarExpr:
		w.WriteByte('*')
		return formatNode(w, value.X)
	case *ast.Ident:
		w.WriteString(value.Name)
		return nil
	case *ast.SelectorExpr:
		if err := formatNode(w, value.X); err != nil {
			return err
		}
		w.WriteByte('.')
		w.WriteString(value.Sel.Name)
		return nil
	case *ast.IndexExpr:
		return formatNode(w, value.X)
	case *ast.IndexListExpr:
		return formatNode(w, value.X)
	default:
		w.WriteString(fmt.Sprintf("%T", expr))
		return nil
	}
}

func writeSourceCSV(path string, rows []sourceFile) error {
	return writeCSV(path, []string{"file", "lines", "sha256", "methods", "call_expressions"}, func(w *csv.Writer) error {
		for _, row := range rows {
			if err := w.Write([]string{row.path, strconv.Itoa(row.lines), row.digest, strconv.Itoa(row.methods), strconv.Itoa(row.calls)}); err != nil {
				return err
			}
		}
		return nil
	})
}

func writeFunctionCSV(path string, rows []functionRow) error {
	return writeCSV(path, []string{"file", "line", "receiver", "function"}, func(w *csv.Writer) error {
		for _, row := range rows {
			if err := w.Write([]string{row.file, row.line, row.receiver, row.name}); err != nil {
				return err
			}
		}
		return nil
	})
}

func writeCallCSV(path string, rows []callRow) error {
	return writeCSV(path, []string{"file", "line", "caller", "callee"}, func(w *csv.Writer) error {
		for _, row := range rows {
			if err := w.Write([]string{row.file, row.line, row.caller, row.callee}); err != nil {
				return err
			}
		}
		return nil
	})
}

func writeCatalogCSV(path string) error {
	return writeCSV(path, []string{"id", "family", "risk", "global", "access_paths", "dependencies", "constraint_kinds"}, func(w *csv.Writer) error {
		for _, op := range authorization.Catalog() {
			access := make([]string, len(op.AccessPaths))
			for i, value := range op.AccessPaths {
				access[i] = string(value)
			}
			kinds := make([]string, len(op.ConstraintKinds))
			for i, value := range op.ConstraintKinds {
				kinds[i] = string(value)
			}
			if err := w.Write([]string{
				op.ID, op.Family, string(op.Risk), strconv.FormatBool(op.Global),
				strings.Join(access, "|"), strings.Join(op.Dependencies, "|"), strings.Join(kinds, "|"),
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func writeCSV(path string, header []string, rows func(*csv.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write(header); err != nil {
		return err
	}
	if err := rows(w); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}

func writeSummary(path string, files []sourceFile, functions []functionRow, calls []callRow) error {
	var lines, callCount int
	for _, file := range files {
		lines += file.lines
		callCount += file.calls
	}
	body := fmt.Sprintf(`# Capability audit artifacts

Generated by:

`+"```sh"+`
go run ./scripts/capability_audit -out dist/audits
`+"```"+`

- Go source files: %d
- Go source lines: %d
- Function/method declarations: %d
- Call expressions: %d
- Permission catalog: %d operations

Files:

- source-files.csv
- functions.csv
- call-expressions.csv
- permission-catalog.csv

These are derived artifacts and are intentionally ignored by Git.
`, len(files), lines, len(functions), callCount, len(authorization.Catalog()))
	return os.WriteFile(path, []byte(body), 0o644)
}
