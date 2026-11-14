package v1wire

import (
	"math"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// 期望值取自真实 Java 21 + Jackson 2.17.3 的输出（app/plaza/v1compat/testdata/java_golden.json，GoldenGen.java 生成）。

func TestJavaDouble(t *testing.T) {
	for in, want := range map[float64]string{
		1e10: "1.0E10", 0.0001: "1.0E-4", 1.5: "1.5", 1e7: "1.0E7", 9999999.0: "9999999.0", 0.001: "0.001",
		0.00099: "9.9E-4", 5e-324: "4.9E-324", math.MaxFloat64: "1.7976931348623157E308", 0.3333333333333333: "0.3333333333333333",
		100: "100.0", 1e23: "1.0E23", 2e-3: "0.002", 123456789012.5: "1.234567890125E11", -1.25e-7: "-1.25E-7", 2.5: "2.5",
		math.Copysign(0, -1): "-0.0", 0: "0.0", math.Inf(1): "Infinity", 1234567.0: "1234567.0", 0.1: "0.1",
	} {
		if got := JavaDouble(in); got != want {
			t.Errorf("JavaDouble(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestJacksonRoundTrip(t *testing.T) {
	for in, want := range map[string]string{
		`{"z": -0, "n": 12345678901234567890123, "f": 1e10}`:    `{"z":0,"n":12345678901234567890123,"f":1.0E10}`,
		`{"a": 1, "b": 2, "a": {"x": [1, 2.50, null, true]}}`:   `{"a":{"x":[1,2.5,null,true]},"b":2}`,
		`{"s": "a\"b\\c\u0001\u001f\b\f\n\r\t/<>& \u007f é😀😀"}`: "{\"s\":\"a\\\"b\\\\c\\u0001\\u001F\\b\\f\\n\\r\\t/<>& \u007f é😀😀\"}",
		`{"k": []}  trailing`: `{"k":[]}`,
	} {
		v, err := ParseJava([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := string(AppendJackson(nil, v)); got != want {
			t.Errorf("往返\nin:   %s\ngot:  %s\nwant: %s", in, got, want)
		}
	}
	v, _ := ParseJava([]byte(`{"m": {"x": [1, 2.5, null, true]}, "s": "t", "n": null}`))
	for k, want := range map[string]string{"m": "{x=[1, 2.5, null, true]}", "s": "t", "n": "null"} {
		got, _ := v.Get(k)
		if s := JavaStringOf(got); s != want {
			t.Errorf("String.valueOf(%s) = %q, want %q", k, s, want)
		}
	}
	if w := v.Without(map[string]bool{"m": true}); string(AppendJackson(nil, w)) != `{"s":"t","n":null}` {
		t.Errorf("Without: %s", AppendJackson(nil, w))
	}
}

func TestCompareUTF16(t *testing.T) {
	in := []string{"b", "a", "B", "Ａ", "ａ", "😀", "�", "海报", "电商", "𝒜", "z", "", " ", "中", "ｂ"}
	sort.SliceStable(in, func(i, j int) bool { return CompareUTF16(in[i], in[j]) < 0 })
	want := []string{"", " ", "B", "a", "b", "z", "中", "海报", "电商", "𝒜", "😀", "Ａ", "ａ", "ｂ", "�"}
	if strings.Join(in, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", in)
	}
}

func TestJavaStringHelpers(t *testing.T) {
	if JavaTrim("\t\u0001 a b \n") != "a b" || JavaTrim("　a　") != "　a　" {
		t.Fatal("String.trim 只去 ≤ U+0020")
	}
	if HasText("  \t") || HasText("　") || !HasText(" ") || !HasText(" x ") || HasText("") {
		t.Fatal("hasText：U+3000 是空白，U+00A0 不是")
	}
	if JavaLowerRoot("ABC Ä") != "abc ä" || JavaLowerRoot("ΣΑΣ x") != "σας x" || JavaLowerRoot("İ") != "i̇" {
		t.Fatalf("toLowerCase(ROOT): %q %q", JavaLowerRoot("ΣΑΣ x"), JavaLowerRoot("İ"))
	}
}

func TestSpringBinding(t *testing.T) {
	type c struct {
		in   []string
		want int64
		err  bool
	}
	for _, x := range []c{
		{nil, 7, false}, {[]string{""}, 7, false}, {[]string{"3"}, 3, false}, {[]string{" 1 2 "}, 12, false},
		{[]string{"0x1F"}, 31, false}, {[]string{"#10"}, 16, false}, {[]string{"-0x10"}, -16, false}, {[]string{"+5"}, 5, false},
		{[]string{"010"}, 10, false}, {[]string{"2", "9"}, 2, false}, {[]string{"", "9"}, 7, false},
		{[]string{" "}, 0, true}, {[]string{"abc"}, 0, true}, {[]string{"1.0"}, 0, true}, {[]string{"0x"}, 0, true},
		{[]string{"0x-1"}, 0, true}, {[]string{"+0x1"}, 0, true}, {[]string{"9223372036854775808"}, 0, true}, {[]string{"-"}, 0, true},
	} {
		got, err := SpringLong(x.in, 7)
		if (err != nil) != x.err || (!x.err && got != x.want) {
			t.Errorf("SpringLong(%q) = %d, %v", x.in, got, err)
		}
	}
	for _, x := range []struct {
		in      []string
		want    string
		present bool
	}{
		{nil, "", false}, {[]string{""}, "", true}, {[]string{"a, b ,c"}, "a|b|c", true}, {[]string{"a,"}, "a|", true},
		{[]string{"a,b", " c "}, "a,b| c ", true},
	} {
		got, present := SpringStringList(x.in)
		if strings.Join(got, "|") != x.want || present != x.present {
			t.Errorf("SpringStringList(%q) = %q %v", x.in, got, present)
		}
	}
	if s, ok := SpringString([]string{"a", "b"}); s != "a,b" || !ok {
		t.Error("SpringString 多值逗号拼接")
	}
}

func TestJavaRequestID(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-Id", "req-12345678")
	if JavaRequestID(r) != "req-12345678" {
		t.Fatal("合法 id 沿用")
	}
	r.Header.Set("X-Request-Id", "caf\xe9-1234") // ISO-8859-1 的 é 是字母
	if got := JavaRequestID(r); got != "café-1234" {
		t.Fatalf("Latin-1 解码: %q", got)
	}
	r.Header.Set("X-Request-Id", "a_b-12345")
	if got := JavaRequestID(r); len(got) != 32 || got == "a_b-12345" {
		t.Fatalf("下划线不合法应重新生成: %q", got)
	}
}

func TestWriteJavaEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-Id", "abcdefgh")
	WriteJavaFail(rec, r, &JavaFailure{Status: 500, Code: 5002, Reason: "DATABASE_ERROR", Msg: "兼容数据损坏"})
	b := rec.Body.String()
	if rec.Code != 500 || !strings.HasPrefix(b, `{"code":5002,"reason":"DATABASE_ERROR","msg":"兼容数据损坏","data":null,"requestId":"abcdefgh","timestamp":`) {
		t.Fatalf("%d %s", rec.Code, b)
	}
	if rec.Header().Get("X-Jsoncase") != "off" {
		t.Fatal("应关闭 jsoncase 改写")
	}
}
