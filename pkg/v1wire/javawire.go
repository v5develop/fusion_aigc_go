package v1wire

// 本文件：creaft v1 兼容面「逐字节同形」所需的 Java 侧行为复刻（只追加，不改上面的 Envelope / OK / Fail）。
//
// v1 兼容路径（/api/v1/plaza-templates/list、/tags、/api/v1/plaza-headlines）由 Java 用 Jackson 2.17 写出，
// 数据是先把 JSON 列读成 LinkedHashMap 再写回去的，所以 Go 要对齐的不是「库里的原始字节」，而是：
//   - Jackson 的往返：对象键保持出现顺序、重复键「后值覆盖、位置不变」；整数原样（-0 → 0）；
//     小数先变 double 再按 Double.toString 写（1e10 → 1.0E10）；字符串按 Jackson 的转义表重写；
//   - Java 的 String.valueOf / String.trim / Character.isWhitespace / String.compareTo（UTF-16 码元序）；
//   - Spring MVC 的 @RequestParam 绑定（long 的 NumberUtils.parseNumber、List<String> 的单值逗号拆分）；
//   - CreaftV1RequestIdFilter 的 X-Request-Id 规整与 CommonResult 的键序 {code,reason?,msg,data,requestId,timestamp}。
//
// 设计：fusion-aigc/.scratch/agent-template-industry/DESIGN.md 9.1。

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// ---------------------------------------------------------------- Jackson 往返

// JKind JSON 值的种类（对应 Jackson 读成 Map<String,Object> 后的 Java 类型）。
type JKind uint8

const (
	JNull   JKind = iota
	JBool         // Boolean
	JInt          // Integer / Long / BigInteger：Num 为规范十进制
	JDouble       // Double：Num 为 Double.toString 的结果
	JString       // String
	JArray        // ArrayList
	JObject       // LinkedHashMap
)

// JMember 有序对象的一项。
type JMember struct {
	Key string
	Val JValue
}

// JValue Jackson 读入后的值（保序、不经 float64 丢精度）。
type JValue struct {
	Kind JKind
	Bool bool
	Num  string
	Str  string
	Arr  []JValue
	Obj  []JMember
}

// Get 取对象键（无此键 ok=false）。
func (v JValue) Get(key string) (JValue, bool) {
	for _, m := range v.Obj {
		if m.Key == key {
			return m.Val, true
		}
	}
	return JValue{}, false
}

// Without 返回去掉若干顶层键后的对象（其余键序不变）。
func (v JValue) Without(drop map[string]bool) JValue {
	out := JValue{Kind: JObject, Obj: make([]JMember, 0, len(v.Obj))}
	for _, m := range v.Obj {
		if !drop[m.Key] {
			out.Obj = append(out.Obj, m)
		}
	}
	return out
}

// ParseJava 按 Jackson（默认配置）把 JSON 文本读成值。尾随内容忽略（Jackson 默认 FAIL_ON_TRAILING_TOKENS=false）。
func ParseJava(raw []byte) (JValue, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return decodeJ(dec)
}

func decodeJ(dec *json.Decoder) (JValue, error) {
	tok, err := dec.Token()
	if err != nil {
		if err == io.EOF {
			return JValue{}, errors.New("v1wire: 空 JSON")
		}
		return JValue{}, err
	}
	switch t := tok.(type) {
	case nil:
		return JValue{Kind: JNull}, nil
	case bool:
		return JValue{Kind: JBool, Bool: t}, nil
	case string:
		return JValue{Kind: JString, Str: t}, nil
	case json.Number:
		return numberJ(string(t))
	case json.Delim:
		switch t {
		case '[':
			arr := []JValue{}
			for dec.More() {
				v, err := decodeJ(dec)
				if err != nil {
					return JValue{}, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return JValue{}, err
			}
			return JValue{Kind: JArray, Arr: arr}, nil
		case '{':
			obj := []JMember{}
			idx := map[string]int{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return JValue{}, err
				}
				key, _ := kt.(string)
				v, err := decodeJ(dec)
				if err != nil {
					return JValue{}, err
				}
				if i, dup := idx[key]; dup { // LinkedHashMap.put：后值覆盖，位置不变
					obj[i].Val = v
					continue
				}
				idx[key] = len(obj)
				obj = append(obj, JMember{key, v})
			}
			if _, err := dec.Token(); err != nil {
				return JValue{}, err
			}
			return JValue{Kind: JObject, Obj: obj}, nil
		}
	}
	return JValue{}, errors.New("v1wire: 非法 JSON")
}

func numberJ(lit string) (JValue, error) {
	if strings.ContainsAny(lit, ".eE") {
		f, err := strconv.ParseFloat(lit, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return JValue{}, err
		}
		return JValue{Kind: JDouble, Num: JavaDouble(f)}, nil
	}
	n, ok := new(big.Int).SetString(lit, 10)
	if !ok {
		return JValue{}, errors.New("v1wire: 非法整数 " + lit)
	}
	return JValue{Kind: JInt, Num: n.String()}, nil // "-0" → "0"（Integer 0）
}

// AppendJackson 按 Jackson 写出值（紧凑、无 HTML 转义）。
func AppendJackson(b []byte, v JValue) []byte {
	switch v.Kind {
	case JNull:
		return append(b, "null"...)
	case JBool:
		if v.Bool {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case JInt:
		return append(b, v.Num...)
	case JDouble:
		if v.Num == "NaN" || v.Num == "Infinity" || v.Num == "-Infinity" { // QUOTE_NON_NUMERIC_NUMBERS 默认开
			return AppendJacksonString(b, v.Num)
		}
		return append(b, v.Num...)
	case JString:
		return AppendJacksonString(b, v.Str)
	case JArray:
		b = append(b, '[')
		for i, e := range v.Arr {
			if i > 0 {
				b = append(b, ',')
			}
			b = AppendJackson(b, e)
		}
		return append(b, ']')
	case JObject:
		b = append(b, '{')
		for i, m := range v.Obj {
			if i > 0 {
				b = append(b, ',')
			}
			b = AppendJacksonString(b, m.Key)
			b = append(b, ':')
			b = AppendJackson(b, m.Val)
		}
		return append(b, '}')
	}
	return append(b, "null"...)
}

const upperHex = "0123456789ABCDEF"

// AppendJacksonString Jackson 的字符串转义：只转 " \ 与 0x00–0x1F（\b \t \n \f \r 用短转义，其余 \u00XX 大写）；
// 非 ASCII 原样（UTF-8），不转 < > & / U+2028。
func AppendJacksonString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b = append(b, '\\', '"')
		case c == '\\':
			b = append(b, '\\', '\\')
		case c >= 0x20:
			b = append(b, c)
		case c == '\b':
			b = append(b, '\\', 'b')
		case c == '\t':
			b = append(b, '\\', 't')
		case c == '\n':
			b = append(b, '\\', 'n')
		case c == '\f':
			b = append(b, '\\', 'f')
		case c == '\r':
			b = append(b, '\\', 'r')
		default:
			b = append(b, '\\', 'u', '0', '0', upperHex[c>>4], upperHex[c&0xF])
		}
	}
	return append(b, '"')
}

// JavaStringOf Java 的 String.valueOf(Object)：null → "null"，Map → {k=v, …}，List → [a, b]，Double → Double.toString。
func JavaStringOf(v JValue) string {
	var sb strings.Builder
	javaStringOf(&sb, v)
	return sb.String()
}

func javaStringOf(sb *strings.Builder, v JValue) {
	switch v.Kind {
	case JNull:
		sb.WriteString("null")
	case JBool:
		sb.WriteString(strconv.FormatBool(v.Bool))
	case JInt, JDouble:
		sb.WriteString(v.Num)
	case JString:
		sb.WriteString(v.Str)
	case JArray:
		sb.WriteByte('[')
		for i, e := range v.Arr {
			if i > 0 {
				sb.WriteString(", ")
			}
			javaStringOf(sb, e)
		}
		sb.WriteByte(']')
	case JObject:
		sb.WriteByte('{')
		for i, m := range v.Obj {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(m.Key)
			sb.WriteByte('=')
			javaStringOf(sb, m.Val)
		}
		sb.WriteByte('}')
	}
}

// JavaDouble Java 17+/21 的 Double.toString（JDK 19 起的最短表示算法）：
// 10^-3 ≤ |d| < 10^7 用普通小数（至少一位小数），否则 d.dddE±n；最短位数为 1 时在 1~2 位里取最接近真值的那个（4.9E-324）。
func JavaDouble(d float64) string {
	switch {
	case math.IsNaN(d):
		return "NaN"
	case math.IsInf(d, 1):
		return "Infinity"
	case math.IsInf(d, -1):
		return "-Infinity"
	case d == 0:
		if math.Signbit(d) {
			return "-0.0"
		}
		return "0.0"
	}
	sign := ""
	if d < 0 {
		sign, d = "-", -d
	}
	digits, exp := shortestDecimal(d)
	if d >= 1e-3 && d < 1e7 {
		var intPart, frac string
		if exp >= 0 {
			if len(digits) > exp+1 {
				intPart, frac = digits[:exp+1], digits[exp+1:]
			} else {
				intPart = digits + strings.Repeat("0", exp+1-len(digits))
			}
		} else {
			intPart, frac = "0", strings.Repeat("0", -exp-1)+digits
		}
		if frac == "" {
			frac = "0"
		}
		return sign + intPart + "." + frac
	}
	rest := digits[1:]
	if rest == "" {
		rest = "0"
	}
	return sign + digits[:1] + "." + rest + "E" + strconv.Itoa(exp)
}

// shortestDecimal 返回有效数字（无尾零）与首位的十进制指数：d = 0.digits × 10^(exp+1)。
func shortestDecimal(d float64) (string, int) {
	digits, exp := splitE(strconv.FormatFloat(d, 'e', -1, 64))
	if len(digits) != 1 {
		return digits, exp
	}
	// 最短为 1 位：Java 在长度 1~2 的候选里取最接近真值者（同距取末位偶数）
	two := strconv.FormatFloat(d, 'e', 1, 64)
	if f, err := strconv.ParseFloat(two, 64); err != nil || f != d {
		return digits, exp
	}
	d2, e2 := splitE(two)
	if len(d2) < 2 {
		return digits, exp
	}
	x := new(big.Rat).SetFloat64(d)
	r1, _ := new(big.Rat).SetString(digits + "e" + strconv.Itoa(exp))
	r2, _ := new(big.Rat).SetString(two)
	dist1 := new(big.Rat).Abs(new(big.Rat).Sub(r1, x))
	dist2 := new(big.Rat).Abs(new(big.Rat).Sub(r2, x))
	switch dist2.Cmp(dist1) {
	case -1:
		return d2, e2
	case 0:
		if (d2[len(d2)-1]-'0')%2 == 0 && (digits[0]-'0')%2 != 0 {
			return d2, e2
		}
	}
	return digits, exp
}

// splitE 拆 "d.ddde±XX"：去掉小数点与尾零。
func splitE(s string) (string, int) {
	i := strings.IndexByte(s, 'e')
	mant, ex := s[:i], s[i+1:]
	e, _ := strconv.Atoi(ex)
	digits := strings.Replace(mant, ".", "", 1)
	digits = strings.TrimRight(digits, "0")
	if digits == "" {
		digits = "0"
	}
	return digits, e
}

// ---------------------------------------------------------------- Java 字符串语义

// CompareUTF16 Java String.compareTo：按 UTF-16 码元逐个比较（增补字符的代理对 0xD800+ 小于 BMP 的 0xE000–0xFFFF）。
func CompareUTF16(a, b string) int {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	n := min(len(ua), len(ub))
	for i := range n {
		if ua[i] != ub[i] {
			return int(ua[i]) - int(ub[i])
		}
	}
	return len(ua) - len(ub)
}

// JavaTrim Java String.trim()：去掉首尾 ≤ U+0020 的字符（不是 Unicode 空白）。
func JavaTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r <= ' ' })
}

// JavaIsWhitespace Java Character.isWhitespace：Unicode 空格分隔符（不含不换行空格 U+00A0/U+2007/U+202F）、
// 行/段分隔符，以及 \t \n \u000B \f \r \u001C–\u001F。
func JavaIsWhitespace(r rune) bool {
	switch {
	case r >= '\t' && r <= '\r', r >= 0x1C && r <= 0x1F:
		return true
	case r == 0x00A0 || r == 0x2007 || r == 0x202F:
		return false
	}
	return unicode.In(r, unicode.Zs, unicode.Zl, unicode.Zp)
}

// HasText Spring StringUtils.hasText：非空且含至少一个非空白字符。
func HasText(s string) bool {
	for _, r := range s {
		if !JavaIsWhitespace(r) {
			return true
		}
	}
	return false
}

// JavaLowerRoot Java String.toLowerCase(Locale.ROOT)：在 Go 的简单映射之上补两处特殊规则
// （U+0130 → "i̇"；词尾 Σ → ς）。
func JavaLowerRoot(s string) string {
	if !strings.ContainsAny(s, "İΣ") {
		return strings.ToLower(s)
	}
	rs := []rune(s)
	var sb strings.Builder
	for i, r := range rs {
		switch r {
		case 0x0130:
			sb.WriteString("i̇")
		case 0x03A3:
			if finalSigma(rs, i) {
				sb.WriteRune(0x03C2)
			} else {
				sb.WriteRune(0x03C3)
			}
		default:
			sb.WriteRune(unicode.ToLower(r))
		}
	}
	return sb.String()
}

func finalSigma(rs []rune, i int) bool {
	cased := func(r rune) bool { return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) }
	ignorable := func(r rune) bool { return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) }
	j := i - 1
	for j >= 0 && ignorable(rs[j]) {
		j--
	}
	if j < 0 || !cased(rs[j]) {
		return false
	}
	k := i + 1
	for k < len(rs) && ignorable(rs[k]) {
		k++
	}
	return k >= len(rs) || !cased(rs[k])
}

// ---------------------------------------------------------------- Spring @RequestParam 绑定

// ErrSpringNumber 参数转 long 失败（Spring 的 MethodArgumentTypeMismatchException）。
var ErrSpringNumber = errors.New("v1wire: 参数格式不正确")

// SpringLong 复刻 @RequestParam(defaultValue=…) long：没传或恰好一个空串 → 默认值；多个值取第一个；
// 转换走 NumberUtils.parseNumber（去掉所有空白，支持 0x / # 十六进制与正负号）。
// 多值且第一个为空串在 Java 侧是 500（基本类型收到 null），这里按默认值处理（登记为有意偏差）。
func SpringLong(values []string, def int64) (int64, error) {
	if len(values) == 0 {
		return def, nil
	}
	s := values[0]
	if s == "" {
		return def, nil
	}
	return parseNumberLong(s)
}

func parseNumberLong(text string) (int64, error) {
	var sb strings.Builder
	for _, r := range text {
		if !JavaIsWhitespace(r) {
			sb.WriteRune(r)
		}
	}
	s := sb.String()
	idx := 0
	if strings.HasPrefix(s, "-") {
		idx = 1
	}
	rest := s[idx:]
	if strings.HasPrefix(rest, "0x") || strings.HasPrefix(rest, "0X") || strings.HasPrefix(rest, "#") { // Long.decode
		if strings.HasPrefix(rest, "#") {
			rest = rest[1:]
		} else {
			rest = rest[2:]
		}
		if rest == "" || rest[0] == '-' || rest[0] == '+' {
			return 0, ErrSpringNumber
		}
		if idx == 1 {
			rest = "-" + rest
		}
		n, err := strconv.ParseInt(rest, 16, 64)
		if err != nil {
			return 0, ErrSpringNumber
		}
		return n, nil
	}
	if s == "" || s == "+" || s == "-" {
		return 0, ErrSpringNumber
	}
	n, err := strconv.ParseInt(s, 10, 64) // Long.valueOf：可带 + / -，只收十进制数字
	if err != nil {
		return 0, ErrSpringNumber
	}
	return n, nil
}

// SpringStringList 复刻 @RequestParam(required=false) List<String>：
// 没传 → present=false；只传一个 → 按逗号拆开并逐个 String.trim（StringToCollectionConverter）；
// 传多个 → 原样（ArrayToCollectionConverter，不拆不 trim）。
func SpringStringList(values []string) (list []string, present bool) {
	switch len(values) {
	case 0:
		return nil, false
	case 1:
		s := values[0]
		if s == "" {
			return []string{}, true
		}
		parts := strings.Split(s, ",")
		out := make([]string, len(parts))
		for i, p := range parts {
			out[i] = JavaTrim(p)
		}
		return out, true
	}
	return append([]string(nil), values...), true
}

// ---------------------------------------------------------------- CommonResult 信封

// JavaRequestID 复刻 CreaftV1RequestIdFilter：入站 X-Request-Id（按 ISO-8859-1 解码、String.trim 后）
// 长 8~64 且全是字母数字或连字符才沿用，否则生成 32 位小写十六进制（uuid4 去横杠的格式）。
func JavaRequestID(r *http.Request) string {
	if raw, ok := r.Header["X-Request-Id"]; ok && len(raw) > 0 {
		rs := make([]rune, 0, len(raw[0]))
		for i := 0; i < len(raw[0]); i++ {
			rs = append(rs, rune(raw[0][i])) // Tomcat 按 ISO-8859-1 解码请求头
		}
		s := JavaTrim(string(rs))
		n := utf8.RuneCountInString(s)
		if n >= 8 && n <= 64 && strings.IndexFunc(s, func(c rune) bool {
			return !(unicode.IsLetter(c) || unicode.IsDigit(c) || c == '-')
		}) < 0 {
			return s
		}
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

// JavaFailure Java GlobalExceptionHandler 的一种失败应答。
type JavaFailure struct {
	Status int    // HTTP 状态
	Code   int    // ErrorCode 值
	Reason string // ErrorCode 常量名
	Msg    string
	Data   []byte // nil → null
}

func (f *JavaFailure) Error() string { return f.Reason + ": " + f.Msg }

// FieldViolationData Java FieldViolation.Errors 的 data：{"errors":[{"field","reason","message"}]}。
func FieldViolationData(field, reason, message string) []byte {
	b := []byte(`{"errors":[{"field":`)
	b = AppendJacksonString(b, field)
	b = append(b, `,"reason":`...)
	b = AppendJacksonString(b, reason)
	b = append(b, `,"message":`...)
	b = AppendJacksonString(b, message)
	return append(b, "}]}"...)
}

// WriteJavaOK 写 CommonResult.success(data)：{"code":0,"msg":"success","data":…,"requestId":…,"timestamp":…}。
func WriteJavaOK(w http.ResponseWriter, r *http.Request, data []byte) {
	writeJava(w, r, http.StatusOK, 0, "", "success", data)
}

// WriteJavaFail 写 CommonResult.error：{"code","reason","msg","data","requestId","timestamp"}。
func WriteJavaFail(w http.ResponseWriter, r *http.Request, f *JavaFailure) {
	writeJava(w, r, f.Status, f.Code, f.Reason, f.Msg, f.Data)
}

func writeJava(w http.ResponseWriter, r *http.Request, status, code int, reason, msg string, data []byte) {
	id := JavaRequestID(r)
	b := make([]byte, 0, 64+len(data))
	b = append(b, `{"code":`...)
	b = strconv.AppendInt(b, int64(code), 10)
	if reason != "" {
		b = append(b, `,"reason":`...)
		b = AppendJacksonString(b, reason)
	}
	b = append(b, `,"msg":`...)
	b = AppendJacksonString(b, msg)
	b = append(b, `,"data":`...)
	if data == nil {
		b = append(b, "null"...)
	} else {
		b = append(b, data...)
	}
	b = append(b, `,"requestId":`...)
	b = AppendJacksonString(b, id)
	b = append(b, `,"timestamp":`...)
	b = strconv.AppendInt(b, time.Now().UnixMilli(), 10)
	b = append(b, '}')
	h := w.Header()
	h.Set("X-Request-Id", id)
	h.Set("Content-Type", "application/json")
	h.Set("X-Jsoncase", "off") // jsoncase.SkipHeader：本响应不补 camel / snake 别名（头本身不会发给客户端）
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// SpringString 复刻 @RequestParam(required=false) String：没传 → present=false；
// 传多个 → 用逗号拼接（ArrayToStringConverter → CollectionToStringConverter）。
func SpringString(values []string) (s string, present bool) {
	if len(values) == 0 {
		return "", false
	}
	return strings.Join(values, ","), true
}
