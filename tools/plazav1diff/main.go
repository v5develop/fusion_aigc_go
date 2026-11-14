// Command plazav1diff creaft v1 兼容三接口的黄金比对（dual-run diff）：同一组请求同时打 Java 与 Go，
// 规范化后逐字段比较，输出差异报告；退出码 0 = 无硬差异，1 = 有硬差异，2 = 运行错误（网络 / 语料 / 非预期响应）。
//
//	go run ./tools/plazav1diff -java http://127.0.0.1:8080 -go http://127.0.0.1:8888 [-token xxx] [-report diff.md]
//
// 比较口径（DESIGN 9.1、9.5）：
//   - HTTP 状态必须相同；响应体按 JSON 结构比较（对象键序不要求，Java 的分页 Map.of 键序本来就不确定），数字按字面量比；
//   - payload 子树逐字节比较（键序、数字写法、转义都要一样）；
//   - 顶层 timestamp / requestId 忽略；统计字段（usageCount / uses / previousUses / trend / isNew）与由它派生的头条名次按容差放过；
//   - 其余必须相等。语料不含登记过的有意偏差请求（page<1 等），见 README。
//
// 用法、语料格式、怎么据此决定切流：tools/plazav1diff/README.md。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Corpus 语料文件。
type Corpus struct {
	Comment  string    `json:"comment"`
	Rules    Rules     `json:"rules"`
	Requests []Request `json:"requests"`
}

// Request 一条语料。query 里可以用占位符（运行时先问 Java 拿真实数据再填，见 README）。
type Request struct {
	Name    string            `json:"name"`
	Path    string            `json:"path"`
	Query   string            `json:"query"`
	Headers map[string]string `json:"headers"`
	Auth    bool              `json:"auth"` // 需要 -token：带 Authorization: Bearer <token>
}

type response struct {
	Status int
	Body   []byte
}

// Result 一条语料的比较结果。
type Result struct {
	Req     Request
	URL     string
	Skipped string
	Err     string
	Diffs   []Diff
	Tries   int
}

func (r Result) hard() int {
	n := 0
	for _, d := range r.Diffs {
		if !d.Tolerated {
			n++
		}
	}
	return n
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

func defaultCorpus() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		if p := filepath.Join(filepath.Dir(file), "corpus.json"); fileExists(p) {
			return p
		}
	}
	return "tools/plazav1diff/corpus.json"
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func run(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("plazav1diff", flag.ContinueOnError)
	fs.SetOutput(stdout)
	javaBase := fs.String("java", os.Getenv("PLAZA_DIFF_JAVA"), "Java 上游 base URL（如 http://127.0.0.1:8080）；也可用环境变量 PLAZA_DIFF_JAVA")
	goBase := fs.String("go", os.Getenv("PLAZA_DIFF_GO"), "Go 网关 base URL（如 http://127.0.0.1:8888）；也可用环境变量 PLAZA_DIFF_GO")
	token := fs.String("token", os.Getenv("PLAZA_DIFF_TOKEN"), "可选：用户 access token（语料里 auth=true 的条目才带；不给就跳过这些条目）")
	corpusPath := fs.String("corpus", defaultCorpus(), "语料文件")
	report := fs.String("report", "", "可选：把差异表写成 Markdown 文件")
	retries := fs.Int("retries", 2, "出现硬差异时重取的次数（两边缓存刚好换代时会抖一次）")
	retryWait := fs.Duration("retry-wait", 3*time.Second, "重取前等待")
	timeout := fs.Duration("timeout", 15*time.Second, "单个请求超时")
	only := fs.String("only", "", "可选：只跑名字匹配该正则的条目")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *javaBase == "" || *goBase == "" {
		fmt.Fprintln(stdout, "plazav1diff: 必须给出 -java 与 -go")
		return 2
	}
	raw, err := os.ReadFile(*corpusPath)
	if err != nil {
		fmt.Fprintf(stdout, "plazav1diff: 读语料失败: %v\n", err)
		return 2
	}
	var corpus Corpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		fmt.Fprintf(stdout, "plazav1diff: 语料不是合法 JSON: %v\n", err)
		return 2
	}
	var filter *regexp.Regexp
	if *only != "" {
		if filter, err = regexp.Compile(*only); err != nil {
			fmt.Fprintf(stdout, "plazav1diff: -only 不是合法正则: %v\n", err)
			return 2
		}
	}
	cl := &client{http: &http.Client{Timeout: *timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}, token: *token}

	vars, err := resolveVars(cl, *javaBase, corpus.Requests)
	if err != nil {
		fmt.Fprintf(stdout, "plazav1diff: 从 Java 取占位符数据失败: %v\n", err)
		return 2
	}

	var results []Result
	for _, req := range corpus.Requests {
		if filter != nil && !filter.MatchString(req.Name) {
			continue
		}
		results = append(results, compareOne(cl, *javaBase, *goBase, req, vars, corpus.Rules, *retries, *retryWait))
	}
	code := printReport(stdout, *javaBase, *goBase, results)
	if *report != "" {
		if err := os.WriteFile(*report, []byte(markdown(*javaBase, *goBase, results)), 0o644); err != nil {
			fmt.Fprintf(stdout, "plazav1diff: 写报告失败: %v\n", err)
			return 2
		}
		fmt.Fprintf(stdout, "报告已写入 %s\n", *report)
	}
	return code
}

type client struct {
	http  *http.Client
	token string
}

func (c *client) get(base, path, query string, req Request) (response, error) {
	u := strings.TrimRight(base, "/") + path
	if query != "" {
		u += "?" + query
	}
	hr, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return response{}, err
	}
	hr.Header.Set("User-Agent", "plazav1diff")
	hr.Header.Set("Accept", "application/json")
	for k, v := range req.Headers {
		hr.Header.Set(k, v)
	}
	if req.Auth && c.token != "" {
		hr.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(hr)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return response{Status: resp.StatusCode, Body: body}, err
}

// ---------------------------------------------------------------- 占位符

var placeholder = regexp.MustCompile(`\{([a-zA-Z]+)(?::(\d+))?\}`)

// resolveVars 语料用到哪些占位符就问 Java 拿哪些数据（Java 是被对照的基准，用它的数据造请求最公平）。
//
//	{last:S} / {beyond:S}   size=S 时的末页 / 末页+1（越界页）
//	{tag0} {tag1} {tagLast} 标签清单里的第 1、2、最后一个
//	{tagCJK} {tagEmoji}     第一个含中文 / 含增补字符（emoji 等）的标签
//	{htag}                  第一条头条的第一个标签
//	{hkw} {hkwMixed}        第一条头条标题的前两个字；及其大小写互换（只对 ASCII 字母有效）
func resolveVars(c *client, javaBase string, reqs []Request) (map[string]string, error) {
	need := map[string]bool{}
	for _, r := range reqs {
		for _, m := range placeholder.FindAllStringSubmatch(r.Query, -1) {
			need[m[0]] = true
		}
	}
	vars := map[string]string{}
	if len(need) == 0 {
		return vars, nil
	}
	var tags []string
	if needsAny(need, "tag") {
		resp, err := c.get(javaBase, "/api/v1/plaza-templates/tags", "", Request{})
		if err != nil {
			return nil, err
		}
		var env struct {
			Data []string `json:"data"`
		}
		if err := json.Unmarshal(resp.Body, &env); err != nil {
			return nil, fmt.Errorf("tags 响应: %v", err)
		}
		tags = env.Data
	}
	setIf := func(k string, ok bool, v string) {
		if ok {
			vars[k] = url.QueryEscape(v)
		}
	}
	setIf("{tag0}", len(tags) > 0, at(tags, 0))
	setIf("{tag1}", len(tags) > 1, at(tags, 1))
	setIf("{tagLast}", len(tags) > 0, at(tags, len(tags)-1))
	for _, t := range tags {
		if _, done := vars["{tagCJK}"]; !done && strings.IndexFunc(t, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0 {
			vars["{tagCJK}"] = url.QueryEscape(t)
		}
		if _, done := vars["{tagEmoji}"]; !done && strings.IndexFunc(t, func(r rune) bool { return r > 0xFFFF }) >= 0 {
			vars["{tagEmoji}"] = url.QueryEscape(t)
		}
	}
	if needsAny(need, "{htag}", "{hkw") {
		resp, err := c.get(javaBase, "/api/v1/plaza-headlines", "", Request{})
		if err != nil {
			return nil, err
		}
		var env struct {
			Data struct {
				Items []struct {
					Title string   `json:"title"`
					Tags  []string `json:"tags"`
				} `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal(resp.Body, &env); err != nil {
			return nil, fmt.Errorf("头条响应: %v", err)
		}
		if items := env.Data.Items; len(items) > 0 {
			setIf("{htag}", len(items[0].Tags) > 0, at(items[0].Tags, 0))
			if kw := firstRunes(items[0].Title, 2); kw != "" {
				vars["{hkw}"] = url.QueryEscape(kw)
				vars["{hkwMixed}"] = url.QueryEscape(swapCase(kw))
			}
		}
	}
	for k := range need {
		m := placeholder.FindStringSubmatch(k)
		if m[1] != "last" && m[1] != "beyond" {
			continue
		}
		size := m[2]
		resp, err := c.get(javaBase, "/api/v1/plaza-templates/list", "page=1&size="+size, Request{})
		if err != nil {
			return nil, err
		}
		var env struct {
			Data struct {
				Pages int64 `json:"pages"`
			} `json:"data"`
		}
		if err := json.Unmarshal(resp.Body, &env); err != nil {
			return nil, fmt.Errorf("列表响应: %v", err)
		}
		last := max(env.Data.Pages, 1)
		vars["{last:"+size+"}"] = strconv.FormatInt(last, 10)
		vars["{beyond:"+size+"}"] = strconv.FormatInt(env.Data.Pages+1, 10)
	}
	return vars, nil
}

func needsAny(need map[string]bool, subs ...string) bool {
	for k := range need {
		for _, s := range subs {
			if strings.Contains(k, s) {
				return true
			}
		}
	}
	return false
}

func at(ss []string, i int) string {
	if i < 0 || i >= len(ss) {
		return ""
	}
	return ss[i]
}

func firstRunes(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

func swapCase(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf && unicode.IsUpper(r):
			sb.WriteRune(unicode.ToLower(r))
		case r < utf8.RuneSelf && unicode.IsLower(r):
			sb.WriteRune(unicode.ToUpper(r))
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func fill(query string, vars map[string]string) (string, string) {
	missing := ""
	out := placeholder.ReplaceAllStringFunc(query, func(k string) string {
		if v, ok := vars[k]; ok {
			return v
		}
		missing = k
		return k
	})
	return out, missing
}

// ---------------------------------------------------------------- 比较

func compareOne(c *client, javaBase, goBase string, req Request, vars map[string]string, rules Rules, retries int, wait time.Duration) Result {
	res := Result{Req: req}
	q, missing := fill(req.Query, vars)
	res.URL = req.Path
	if q != "" {
		res.URL += "?" + q
	}
	if missing != "" {
		res.Skipped = "Java 数据里取不到占位符 " + missing
		return res
	}
	if req.Auth && c.token == "" {
		res.Skipped = "需要 -token"
		return res
	}
	for try := 0; try <= retries; try++ {
		if try > 0 {
			time.Sleep(wait)
		}
		res.Tries = try + 1
		jr, err := c.get(javaBase, req.Path, q, req)
		if err != nil {
			res.Err = "Java 请求失败: " + err.Error()
			return res
		}
		gr, err := c.get(goBase, req.Path, q, req)
		if err != nil {
			res.Err = "Go 请求失败: " + err.Error()
			return res
		}
		diffs := compareResponses(jr, gr, rules)
		if res.Diffs == nil || hardCount(diffs) < hardCount(res.Diffs) {
			res.Diffs = diffs
		}
		if hardCount(res.Diffs) == 0 {
			break
		}
	}
	return res
}

func hardCount(ds []Diff) int { return Result{Diffs: ds}.hard() }

func compareResponses(jr, gr response, rules Rules) []Diff {
	diffs := []Diff{}
	if jr.Status != gr.Status {
		diffs = append(diffs, Diff{Path: "(HTTP 状态)", Java: strconv.Itoa(jr.Status), Go: strconv.Itoa(gr.Status), Note: "HTTP 状态不同"})
	}
	jt, jerr := parseTree(jr.Body)
	gt, gerr := parseTree(gr.Body)
	switch {
	case jerr != nil && gerr != nil:
		if string(jr.Body) != string(gr.Body) {
			diffs = append(diffs, Diff{Path: "(响应体)", Java: clip(string(jr.Body)), Go: clip(string(gr.Body)), Note: "两边都不是 JSON 且原文不同"})
		}
	case jerr != nil || gerr != nil:
		diffs = append(diffs, Diff{Path: "(响应体)", Java: clip(string(jr.Body)), Go: clip(string(gr.Body)), Note: "只有一边是 JSON"})
	default:
		diffs = append(diffs, Compare(jt, gt, rules)...)
	}
	return diffs
}

func clip(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// ---------------------------------------------------------------- 报告

func printReport(w io.Writer, javaBase, goBase string, results []Result) int {
	var hard, tol, skipped, errs, badReq int
	fmt.Fprintf(w, "plazav1diff  java=%s  go=%s  语料 %d 条\n", javaBase, goBase, len(results))
	for _, r := range results {
		switch {
		case r.Skipped != "":
			skipped++
			fmt.Fprintf(w, "[跳过] %s  %s（%s）\n", r.Req.Name, r.URL, r.Skipped)
			continue
		case r.Err != "":
			errs++
			fmt.Fprintf(w, "[错误] %s  %s：%s\n", r.Req.Name, r.URL, r.Err)
			continue
		}
		h := r.hard()
		if h > 0 {
			badReq++
		}
		hard += h
		tol += len(r.Diffs) - h
		if len(r.Diffs) == 0 {
			continue
		}
		tag := "[容差]"
		if h > 0 {
			tag = "[差异]"
		}
		fmt.Fprintf(w, "%s %s  GET %s  （取 %d 次）\n", tag, r.Req.Name, r.URL, r.Tries)
		for _, d := range r.Diffs {
			mark := "  !"
			if d.Tolerated {
				mark = "  ~"
			}
			fmt.Fprintf(w, "%s %s  java=%s  go=%s  %s\n", mark, d.Path, clip(d.Java), clip(d.Go), d.Note)
		}
	}
	fmt.Fprintf(w, "汇总：硬差异 %d 条（%d 个请求）；容差内 %d 条；跳过 %d；错误 %d\n", hard, badReq, tol, skipped, errs)
	switch {
	case errs > 0:
		fmt.Fprintln(w, "结论：有请求没跑通（退出码 2），先排查网络 / 地址 / 服务状态")
		return 2
	case hard > 0:
		fmt.Fprintln(w, "结论：存在硬差异（退出码 1），不能切流")
		return 1
	}
	fmt.Fprintln(w, "结论：0 硬差异（退出码 0）")
	return 0
}

func markdown(javaBase, goBase string, results []Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# plazav1diff 报告\n\n- 时间：%s\n- Java：`%s`\n- Go：`%s`\n\n", time.Now().Format(time.RFC3339), javaBase, goBase)
	sb.WriteString("| 条目 | 请求 | 路径 | Java 值 | Go 值 | 说明 | 判定 |\n|---|---|---|---|---|---|---|\n")
	esc := func(s string) string {
		return strings.NewReplacer("|", `\|`, "\n", " ").Replace(clip(s))
	}
	rows := 0
	for _, r := range results {
		switch {
		case r.Skipped != "":
			fmt.Fprintf(&sb, "| %s | `%s` | | | | 跳过：%s | 跳过 |\n", r.Req.Name, esc(r.URL), esc(r.Skipped))
			rows++
		case r.Err != "":
			fmt.Fprintf(&sb, "| %s | `%s` | | | | %s | 错误 |\n", r.Req.Name, esc(r.URL), esc(r.Err))
			rows++
		}
		for _, d := range r.Diffs {
			verdict := "**硬差异**"
			if d.Tolerated {
				verdict = "容差内"
			}
			fmt.Fprintf(&sb, "| %s | `%s` | `%s` | `%s` | `%s` | %s | %s |\n", r.Req.Name, esc(r.URL), esc(d.Path), esc(d.Java), esc(d.Go), esc(d.Note), verdict)
			rows++
		}
	}
	if rows == 0 {
		sb.WriteString("| — | — | — | — | — | 无差异 | 通过 |\n")
	}
	return sb.String()
}
