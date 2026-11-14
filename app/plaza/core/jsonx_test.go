package core

import (
	"strings"
	"testing"
)

func TestSanitizePayloadKeepsOrderAndBigInts(t *testing.T) {
	data := `{"z":1,"prompt":"SECRET","a":{"prompt":"nested-kept","n":12345678901234567890},"authorName":"x","agentSkillId":"1","agent_skill_id":2,"v1PlazaTemplateId":3,"guidance":"g","big":9007199254740993}`
	got := string(sanitizePayload([]byte(data)))
	want := `{"z":1,"a":{"prompt":"nested-kept","n":12345678901234567890},"guidance":"g","big":9007199254740993}`
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	// 非对象 / 空 → {}
	for _, bad := range []string{``, `[1]`, `"x"`, `{"a":`} {
		if got := string(sanitizePayload([]byte(bad))); got != `{}` {
			t.Errorf("%q -> %s", bad, got)
		}
	}
}

func TestComposeDataKeepsV1IDAndAgentContract(t *testing.T) {
	existing := []byte(`{"v1PlazaTemplateId":777,"agentSkillId":"old","prompt":"old"}`)
	// 载荷里夹带的元数据键被忽略；v1PlazaTemplateId 沿用库里的；技能绑定写成字符串；prompt / guidance 随载荷保留
	got, err := composeData([]byte(`{"prompt":"new","guidance":"g","agentSkillId":"evil","v1PlazaTemplateId":1,"result":["u"]}`), "1876543210987654321", existing)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"prompt":"new","guidance":"g","result":["u"],"v1PlazaTemplateId":777,"agentSkillId":"1876543210987654321"}`
	if string(got) != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	// 新增：没有 existing、没有技能
	got, _ = composeData(nil, "", nil)
	if string(got) != `{}` {
		t.Fatal(string(got))
	}
	for _, bad := range []string{`[1]`, `"x"`, `{"a":}`, `{"a":1} trailing`} {
		if _, err := composeData([]byte(bad), "", nil); codeOf(err) == 0 {
			t.Errorf("%q 应被拒", bad)
		}
	}
}

func TestPayloadForDisplayKeepsAuthorAndPrompt(t *testing.T) {
	got := string(payloadForDisplay([]byte(`{"prompt":"p","authorName":"a","agentSkillId":"1","v1PlazaTemplateId":2,"result":[]}`)))
	if got != `{"prompt":"p","authorName":"a","result":[]}` {
		t.Fatal(got)
	}
}

func TestSkillIDForms(t *testing.T) {
	cases := map[string]string{
		`{"agentSkillId":"123"}`:                 "123",
		`{"agentSkillId":187654321098765432100}`: "187654321098765432100",
		`{"agent_skill_id":"9"}`:                 "9",
		`{"agentSkillId":null}`:                  "",
		`{}`:                                     "",
	}
	for in, want := range cases {
		if got := skillID([]byte(in)); got != want {
			t.Errorf("%s -> %q want %q", in, got, want)
		}
	}
}

func TestTagsRoundTrip(t *testing.T) {
	if got := readTags([]byte(`{"tags":["a"," b ",null,"",1]}`)); strings.Join(got, ",") != "a,b,1" {
		t.Fatalf("%v", got)
	}
	for _, bad := range []string{``, `[]`, `{"tags":"x"}`, `{}`} {
		if got := readTags([]byte(bad)); got == nil || len(got) != 0 {
			t.Errorf("%q -> %v", bad, got)
		}
	}
	if got := string(writeTags([]string{" a ", "a", "", "b"})); got != `{"tags":["a","b"]}` {
		t.Fatal(got)
	}
}

func TestFirstResult(t *testing.T) {
	if got := firstResult([]byte(`{"result":["r"],"image_urls":["i"]}`)); got != "r" {
		t.Fatal(got)
	}
	if got := firstResult([]byte(`{"result":[],"image_urls":["i"]}`)); got != "i" {
		t.Fatal(got)
	}
	if got := firstResult([]byte(`{}`)); got != "" {
		t.Fatal(got)
	}
}
