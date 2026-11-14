package main

import (
	"bytes"
	"math"
	"strconv"
	"strings"
)

// Rules 比较规则（语料文件的 rules 段）。
type Rules struct {
	// Ignore 顶层忽略的键（每次请求都不同的 timestamp / requestId）。
	Ignore []string `json:"ignore"`
	// Raw 这些键的子树按原文逐字节比较（payload：键序、数字写法、转义都必须一样）。
	Raw []string `json:"raw"`
	// Volatile 随时间变化的统计字段（两边各有 10 分钟缓存，相位不同）：数字按容差比，null / 布尔变化要有同级数字差异撑腰。
	Volatile []string `json:"volatile"`
	// OrderDerived 由统计派生的名次（头条 rank）：只有本响应里出现了容差内的统计差异时才放过。
	OrderDerived []string `json:"orderDerived"`
	// Align 数组按某个键对齐再比（头条 items 按 plazaTemplateId），顺序差异同 OrderDerived 处理。
	Align map[string]string `json:"align"`
	// VolatileAbs / VolatileRel 统计字段的容差：|Δ| ≤ max(Abs, Rel × max(|java|, |go|))。
	VolatileAbs float64 `json:"volatileAbs"`
	VolatileRel float64 `json:"volatileRel"`
}

// Diff 一条差异。
type Diff struct {
	Path      string
	Java      string
	Go        string
	Note      string
	Tolerated bool // 容差内（不计入退出码）

	kind diffKind
}

type diffKind uint8

const (
	kindHard          diffKind = iota
	kindVolatileNum            // 统计数字，容差内
	kindVolatileOther          // 统计字段的 null / 布尔 / 超差数字
	kindOrder                  // 名次 / 对齐数组顺序
)

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func lastKey(path string) string {
	p := path
	if i := strings.LastIndexAny(p, ".]"); i >= 0 {
		p = p[i+1:]
	}
	return p
}

func parentPath(path string) string {
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[:i]
	}
	return ""
}

type comparer struct {
	r     Rules
	diffs []Diff
}

func (c *comparer) add(d Diff) { c.diffs = append(c.diffs, d) }

// Compare 比较两份 JSON 响应体（已解析的树），返回分好类的差异。
func Compare(java, gov *node, r Rules) []Diff {
	c := &comparer{r: r}
	c.walk("", java, gov, true)
	return classify(c.diffs)
}

func (c *comparer) walk(path string, j, g *node, root bool) {
	key := lastKey(path)
	switch {
	case j == nil && g == nil:
		return
	case j == nil || g == nil:
		c.add(Diff{Path: path, Java: j.scalar(), Go: g.scalar(), Note: "一边缺失"})
		return
	}
	if has(c.r.Raw, key) {
		if !bytes.Equal(j.Raw, g.Raw) {
			c.add(Diff{Path: path, Java: string(j.Raw), Go: string(g.Raw), Note: "原文不一致（键序 / 数字写法 / 转义）"})
		}
		return
	}
	if j.Kind != g.Kind {
		k := kindHard
		if has(c.r.Volatile, key) {
			k = kindVolatileOther
		}
		c.add(Diff{Path: path, Java: j.scalar(), Go: g.scalar(), Note: "类型不同", kind: k})
		return
	}
	switch j.Kind {
	case 'o':
		seen := map[string]bool{}
		for _, m := range j.Obj {
			seen[m.Key] = true
			if root && has(c.r.Ignore, m.Key) {
				continue
			}
			c.walk(join(path, m.Key), m.Val, g.get(m.Key), false)
		}
		for _, m := range g.Obj {
			if !seen[m.Key] && !(root && has(c.r.Ignore, m.Key)) {
				c.walk(join(path, m.Key), nil, m.Val, false)
			}
		}
	case 'a':
		if by, ok := c.r.Align[path]; ok {
			c.aligned(path, by, j, g)
			return
		}
		if len(j.Arr) != len(g.Arr) {
			c.add(Diff{Path: path, Java: "长度 " + strconv.Itoa(len(j.Arr)), Go: "长度 " + strconv.Itoa(len(g.Arr)), Note: "数组长度不同"})
		}
		for i := 0; i < min(len(j.Arr), len(g.Arr)); i++ {
			c.walk(path+"["+strconv.Itoa(i)+"]", j.Arr[i], g.Arr[i], false)
		}
	default:
		if bytes.Equal(j.Raw, g.Raw) {
			return
		}
		d := Diff{Path: path, Java: j.scalar(), Go: g.scalar()}
		switch {
		case has(c.r.OrderDerived, key):
			d.kind, d.Note = kindOrder, "名次（由统计派生）"
		case has(c.r.Volatile, key):
			d.kind, d.Note = kindVolatileOther, "统计字段"
			if j.Kind == 'n' && c.withinTolerance(j, g) {
				d.kind, d.Note = kindVolatileNum, "统计字段（容差内）"
			}
		}
		c.add(d)
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func (c *comparer) withinTolerance(j, g *node) bool {
	a, err1 := strconv.ParseFloat(string(j.Raw), 64)
	b, err2 := strconv.ParseFloat(string(g.Raw), 64)
	if err1 != nil || err2 != nil {
		return false
	}
	return math.Abs(a-b) <= math.Max(c.r.VolatileAbs, c.r.VolatileRel*math.Max(math.Abs(a), math.Abs(b)))
}

func (c *comparer) aligned(path, by string, j, g *node) {
	idOf := func(n *node) string { return n.get(by).scalar() }
	gm := map[string]*node{}
	for _, e := range g.Arr {
		gm[idOf(e)] = e
	}
	jm := map[string]bool{}
	var jo, gom []string
	for _, e := range j.Arr {
		id := idOf(e)
		jm[id] = true
		if ge, ok := gm[id]; ok {
			jo = append(jo, id)
			c.walk(path+"["+by+"="+id+"]", e, ge, false)
		} else {
			c.add(Diff{Path: path + "[" + by + "=" + id + "]", Java: "存在", Go: "缺失", Note: "条目只在 Java"})
		}
	}
	for _, e := range g.Arr {
		id := idOf(e)
		if jm[id] {
			gom = append(gom, id)
		} else {
			c.add(Diff{Path: path + "[" + by + "=" + id + "]", Java: "缺失", Go: "存在", Note: "条目只在 Go"})
		}
	}
	if strings.Join(jo, ",") != strings.Join(gom, ",") {
		c.add(Diff{Path: path, Java: "顺序 " + strings.Join(jo, ","), Go: "顺序 " + strings.Join(gom, ","), Note: "顺序不同（按 " + by + "）", kind: kindOrder})
	}
}

// classify 统计字段的容差判定：
//   - 容差内的统计数字 → 放过；
//   - 统计字段的 null / 布尔 / 超差数字 → 同一对象里有容差内的统计数字差异才放过（trend / isNew 跟着 uses 变）；
//   - 名次与顺序 → 本响应里有任何容差内的统计差异才放过；
//   - 其余一律硬差异。
func classify(ds []Diff) []Diff {
	numParents := map[string]bool{}
	anyNum := false
	for _, d := range ds {
		if d.kind == kindVolatileNum {
			numParents[parentPath(d.Path)] = true
			anyNum = true
		}
	}
	for i := range ds {
		switch ds[i].kind {
		case kindVolatileNum:
			ds[i].Tolerated = true
		case kindVolatileOther:
			ds[i].Tolerated = numParents[parentPath(ds[i].Path)]
		case kindOrder:
			ds[i].Tolerated = anyNum
		}
	}
	return ds
}
