// Package sharedtables 共享表写入守卫：Java 拥有的共享表只能由各自的 Go 属主包写，其它地方一律不许写。
//
// 规则（设计依据 docs/design/xiaoqi-2026-10-30/00-需求分流与设计方案.md §9.5「写 Java 共享表仍走现有 pkg」）：
//
//	afv_billing_*                                  只有 pkg/billing 可写
//	afv_creation_task / afv_creation_task_text / afv_exec_submission   只有 pkg/taskcenter 可写
//	afv_asset / afv_asset_item                     只有 pkg/asset 可写
//	afv_task_event                                 只有 pkg/taskevent 可写
//	afv_notification                               只有 pkg/notify 可写（Java 站内通知中心，Go 写定向通知）
//
// 检测手段（静态，只看非 _test.go 源码）：
//  1. SQL 字符串：把整条字符串拼接（含 const）还原后，匹配 INSERT INTO / REPLACE INTO / UPDATE / DELETE FROM 受保护表；
//  2. bun 链式调用：NewInsert/NewUpdate/NewDelete 链上 Table/TableExpr/ModelTableExpr 带受保护表名，
//     或 Model(...) 的类型在同包里用 bun:"table:受保护表" 标注；bundb.UpdateIf/TransitStatus/Upsert 同理；
//  3. goctl 生成的 model：含 "insert into %s" 之类模板且常量表名是受保护表。
//
// 局限：表名若靠变量 / fmt 拼进 SQL 则看不见，所以新代码请把表名写成字面量。
// 有正当理由的例外登记在 allowlist.txt（文件|表|理由），按「文件 + 表」放行，不按行号，避免改代码就失效。
package sharedtables

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Rule 一张（或一组）受保护表及其属主包目录（相对仓库根）。
type Rule struct {
	Pattern *regexp.Regexp // 匹配整个表名（已含 ^$）
	Owner   string         // 属主包目录，如 pkg/billing
}

// Rules 受保护表。
var Rules = []Rule{
	{regexp.MustCompile(`^afv_billing_[a-z0-9_]+$`), "pkg/billing"},
	{regexp.MustCompile(`^afv_creation_task(_text)?$`), "pkg/taskcenter"},
	{regexp.MustCompile(`^afv_exec_submission$`), "pkg/taskcenter"},
	{regexp.MustCompile(`^afv_asset(_item)?$`), "pkg/asset"},
	{regexp.MustCompile(`^afv_task_event$`), "pkg/taskevent"},
	{regexp.MustCompile(`^afv_notification$`), "pkg/notify"}, // Java 站内通知中心（V1.0.99）：Go 只经 pkg/notify 写定向通知
}

func ruleOf(table string) (Rule, bool) {
	for _, r := range Rules {
		if r.Pattern.MatchString(table) {
			return r, true
		}
	}
	return Rule{}, false
}

// Violation 一处越权写入。
type Violation struct {
	File  string // 相对仓库根，斜杠分隔
	Line  int
	Table string
	Owner string
	How   string // 检测到的写法，便于定位
}

func (v Violation) String() string {
	return fmt.Sprintf("%s:%d 写了共享表 %s（%s），属主是 %s", v.File, v.Line, v.Table, v.How, v.Owner)
}

// Allow 一条例外。
type Allow struct {
	File, Table, Reason string
}

// LoadAllowlist 读 allowlist.txt。每行「文件|表|理由」，# 开头为注释；理由不能为空。
func LoadAllowlist(path string) ([]Allow, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Allow
	for i, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		p := strings.SplitN(ln, "|", 3)
		if len(p) != 3 || strings.TrimSpace(p[0]) == "" || strings.TrimSpace(p[1]) == "" || strings.TrimSpace(p[2]) == "" {
			return nil, fmt.Errorf("%s 第 %d 行格式不对，应为「文件|表|理由」且三段都不能为空：%q", path, i+1, ln)
		}
		out = append(out, Allow{strings.TrimSpace(p[0]), strings.TrimSpace(p[1]), strings.TrimSpace(p[2])})
	}
	return out, nil
}

var skipDirs = map[string]bool{".git": true, "vendor": true, "node_modules": true, "bin": true, "data": true, "logs": true}

// Scan 扫描 root 下所有非测试 Go 文件，返回扣掉属主包与 allow 之后的违规，以及没有被用到的 allow 条目（过期例外）。
func Scan(root string, allow []Allow) (viol []Violation, staleAllow []Allow, err error) {
	var files []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && p != root {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(files)

	fset := token.NewFileSet()
	consts = map[string]string{}
	conflict := map[string]bool{}
	type parsed struct {
		rel, dir string
		f        *ast.File
	}
	var all []parsed
	// 目录 -> 类型名 -> 受保护表（bun:"table:..." 标注）
	models := map[string]map[string]string{}
	for _, p := range files {
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return nil, nil, fmt.Errorf("解析 %s：%w", p, perr)
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		dir := filepath.ToSlash(filepath.Dir(rel))
		all = append(all, parsed{rel, dir, f})
		collectModels(f, dir, models)
		collectConsts(f, conflict)
	}
	for n := range conflict {
		delete(consts, n)
	}

	used := map[string]bool{}
	allowed := map[string]bool{}
	for _, a := range allow {
		allowed[a.File+"|"+a.Table] = true
	}
	seen := map[string]bool{}
	for _, pf := range all {
		for _, v := range scanFile(fset, pf.f, pf.rel, models) {
			r, _ := ruleOf(v.Table)
			if inOwner(pf.dir, r.Owner) {
				continue
			}
			if k := v.File + "|" + v.Table; allowed[k] {
				used[k] = true
				continue
			}
			v.Owner = r.Owner
			key := fmt.Sprintf("%s:%d|%s|%s", v.File, v.Line, v.Table, v.How)
			if !seen[key] {
				seen[key] = true
				viol = append(viol, v)
			}
		}
	}
	for _, a := range allow {
		if !used[a.File+"|"+a.Table] {
			staleAllow = append(staleAllow, a)
		}
	}
	sort.Slice(viol, func(i, j int) bool {
		if viol[i].File != viol[j].File {
			return viol[i].File < viol[j].File
		}
		return viol[i].Line < viol[j].Line
	})
	return viol, staleAllow, nil
}

func inOwner(dir, owner string) bool { return dir == owner || strings.HasPrefix(dir, owner+"/") }

var bunTableTag = regexp.MustCompile(`bun:"[^"]*\btable:([A-Za-z0-9_]+)`)

func collectModels(f *ast.File, dir string, models map[string]map[string]string) {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, s := range gd.Specs {
			ts, ok := s.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, fld := range st.Fields.List {
				if fld.Tag == nil {
					continue
				}
				tag, _ := strconv.Unquote(fld.Tag.Value)
				if m := bunTableTag.FindStringSubmatch(tag); m != nil {
					if _, ok := ruleOf(m[1]); ok {
						if models[dir] == nil {
							models[dir] = map[string]string{}
						}
						models[dir][ts.Name.Name] = m[1]
					}
				}
			}
		}
	}
}

const tbl = "`?(afv_[a-z0-9_]+)`?"

var (
	writeSQL = []*regexp.Regexp{
		regexp.MustCompile(`(?is)\b(?:INSERT\s+(?:IGNORE\s+)?INTO|REPLACE\s+INTO)\s+(?:[A-Za-z0-9_]+\.)?` + tbl + `\b`),
		regexp.MustCompile(`(?is)\bUPDATE\s+(?:LOW_PRIORITY\s+|IGNORE\s+)*(?:[A-Za-z0-9_]+\.)?` + tbl + `\b`),
		regexp.MustCompile(`(?is)\bDELETE\s+(?:[A-Za-z0-9_]+\s+)?FROM\s+(?:[A-Za-z0-9_]+\.)?` + tbl + `\b`),
		// 多表 UPDATE：UPDATE a JOIN afv_x b ON ...
		regexp.MustCompile(`(?is)\bUPDATE\s+[A-Za-z0-9_` + "`" + `]+(?:\s+(?:AS\s+)?[A-Za-z0-9_]+)?\s+(?:(?:INNER|LEFT|RIGHT)\s+)?JOIN\s+` + tbl + `\b`),
	}
	goctlWrite = regexp.MustCompile(`(?i)\b(insert\s+into|update|delete\s+from)\s+%s\b`)
	bunNew     = map[string]bool{"NewInsert": true, "NewUpdate": true, "NewDelete": true}
	bunTblFn   = map[string]bool{"Table": true, "TableExpr": true, "ModelTableExpr": true}
	bundbFn    = map[string]bool{"UpdateIf": true, "TransitStatus": true, "Upsert": true}
	tableWord  = regexp.MustCompile(`^\s*` + tbl + `(?:\s+(?:AS\s+)?[A-Za-z0-9_]+)?\s*$`)
)

func scanFile(fset *token.FileSet, f *ast.File, rel string, models map[string]map[string]string) []Violation {
	var out []Violation
	add := func(pos token.Pos, table, how string) {
		if _, ok := ruleOf(table); ok {
			out = append(out, Violation{File: rel, Line: fset.Position(pos).Line, Table: table, How: how})
		}
	}
	dir := filepath.ToSlash(filepath.Dir(rel))

	// 1) 字符串（还原整条 + 拼接）
	var lits []struct {
		pos token.Pos
		s   string
	}
	ast.Inspect(f, func(n ast.Node) bool {
		e, ok := n.(ast.Expr)
		if !ok {
			return true
		}
		if s, ok := constString(e); ok {
			lits = append(lits, struct {
				pos token.Pos
				s   string
			}{e.Pos(), s})
			return false // 整条拼接只处理一次，不再下钻到子字面量
		}
		return true
	})
	hasGoctlTpl := false
	for _, l := range lits {
		for _, re := range writeSQL {
			for _, m := range re.FindAllStringSubmatch(l.s, -1) {
				add(l.pos, m[1], "SQL 字符串")
			}
		}
		if goctlWrite.MatchString(l.s) {
			hasGoctlTpl = true
		}
	}
	if hasGoctlTpl { // goctl 生成的 model：模板里 `%s` 的表名来自常量
		for _, l := range lits {
			if m := tableWord.FindStringSubmatch(l.s); m != nil && !strings.ContainsAny(l.s, "(),") && strings.HasPrefix(strings.Trim(l.s, "` "), "afv_") {
				add(l.pos, m[1], "goctl model（insert/update/delete 模板 + 表名常量）")
			}
		}
	}

	// 2) bun 链式 / bundb 助手
	ast.Inspect(f, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := callName(ce)
		switch {
		case bundbFn[name]:
			for _, a := range ce.Args {
				if s, ok := constString(a); ok {
					if m := tableWord.FindStringSubmatch(s); m != nil {
						add(ce.Pos(), m[1], "bundb."+name)
					}
				}
				if t := modelTable(a, dir, models); t != "" {
					add(ce.Pos(), t, "bundb."+name+" 的 bun model")
				}
			}
		case bunTblFn[name] || name == "Model":
			if !chainHasWrite(ce) {
				return true
			}
			for _, a := range ce.Args {
				if name == "Model" {
					if t := modelTable(a, dir, models); t != "" {
						add(ce.Pos(), t, "bun 写链 Model(...)")
					}
				} else if s, ok := constString(a); ok {
					if m := tableWord.FindStringSubmatch(s); m != nil {
						add(ce.Pos(), m[1], "bun 写链 "+name)
					}
				}
			}
		}
		return true
	})
	return out
}

// callName 调用的方法 / 函数名（x.Foo(...) 或 Foo(...)）。
func callName(ce *ast.CallExpr) string {
	switch fn := ce.Fun.(type) {
	case *ast.SelectorExpr:
		return fn.Sel.Name
	case *ast.Ident:
		return fn.Name
	}
	return ""
}

// chainHasWrite 链上（向左一路到根）有没有 NewInsert / NewUpdate / NewDelete。
func chainHasWrite(ce *ast.CallExpr) bool {
	var cur ast.Expr = ce
	for {
		c, ok := cur.(*ast.CallExpr)
		if !ok {
			return false
		}
		if bunNew[callName(c)] {
			return true
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		cur = sel.X
	}
}

// modelTable 从 (*T)(nil) / &T{} / T{} / (*pkg.T)(nil) 里取类型名，查同包标注的受保护表。
func modelTable(e ast.Expr, dir string, models map[string]map[string]string) string {
	var tname string
	var walk func(ast.Expr)
	walk = func(x ast.Expr) {
		switch v := x.(type) {
		case *ast.ParenExpr:
			walk(v.X)
		case *ast.StarExpr:
			walk(v.X)
		case *ast.UnaryExpr:
			walk(v.X)
		case *ast.CallExpr: // (*T)(nil)
			walk(v.Fun)
		case *ast.CompositeLit:
			walk(v.Type)
		case *ast.Ident:
			tname = v.Name
		case *ast.SelectorExpr: // pkg.T：按类型名在所有目录里找（受保护 model 的类型名在仓库内应唯一）
			tname = "." + v.Sel.Name
		}
	}
	walk(e)
	if tname == "" {
		return ""
	}
	if strings.HasPrefix(tname, ".") {
		for _, m := range models {
			if t, ok := m[tname[1:]]; ok {
				return t
			}
		}
		return ""
	}
	return models[dir][tname]
}

// consts 仓库内所有包级字符串常量（名字 -> 值；同名不同值的丢弃，宁可漏报不误报）。Scan 开始时重建。
var consts = map[string]string{}

func collectConsts(f *ast.File, conflict map[string]bool) {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				bl, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					continue
				}
				val, err := strconv.Unquote(bl.Value)
				if err != nil {
					continue
				}
				if old, dup := consts[n.Name]; dup && old != val {
					conflict[n.Name] = true
				}
				consts[n.Name] = val
			}
		}
	}
}

// constString 把字符串字面量 / 字面量拼接（+）/ 括号还原成一个字符串。含非字面量（变量、调用）则失败。
func constString(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return constString(v.X)
	case *ast.Ident: // 同仓库的字符串常量（如 const TableAsset = "afv_asset"）
		s, ok := consts[v.Name]
		return s, ok
	case *ast.SelectorExpr:
		s, ok := consts[v.Sel.Name]
		return s, ok
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, ok1 := constString(v.X)
		r, ok2 := constString(v.Y)
		if ok1 && ok2 {
			return l + r, true
		}
		// 一侧是变量（如 "SELECT " + cols + " FROM afv_x"）：保留字面量部分，仍然能识别写语句
		if ok1 || ok2 {
			return l + " " + r, true
		}
	}
	return "", false
}
