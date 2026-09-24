package audit

// SanitizePayload 单测(T6.1 PII 脱敏钩子,零 DB):T1.1 口径继承——
// user_data 标注子树的叶子值掩蔽、键形保留、未标注 payload 字节不变。
// 变异自查(实现错 → 哪条红):
//   - 掩蔽漏叶子(数组/嵌套对象)→ 对应用例红;
//   - 未标注路径被重写(remarshal 副作用)→ bytes 不变断言红;
//   - 键匹配放宽(user_dataX 误中)→ 精确匹配用例红;
//   - 标志位与掩蔽动作脱钩(恒 true/恒 false)→ 两侧用例红。

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSanitizePayload(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		want       string
		wantMasked bool
	}{
		{
			name: "no marker unchanged",
			in:   `{"from":"PENDING","to":"EXECUTING","version":7}`,
			want: `{"from":"PENDING","to":"EXECUTING","version":7}`,
		},
		{
			name:       "top level user_data leaves masked",
			in:         `{"user_data":{"email":"a@b.c","seq":3},"node":"n1"}`,
			want:       `{"node":"n1","user_data":{"email":"***","seq":"***"}}`,
			wantMasked: true,
		},
		{
			name:       "nested marker at depth",
			in:         `{"ctx":{"llm":{"user_data":{"q":"secret"}}},"ok":true}`,
			want:       `{"ctx":{"llm":{"user_data":{"q":"***"}}},"ok":true}`,
			wantMasked: true,
		},
		{
			name:       "marker subtree with nested objects and arrays",
			in:         `{"user_data":{"profile":{"email":"a@b.c"},"tags":["x","y"],"count":2}}`,
			want:       `{"user_data":{"count":"***","profile":{"email":"***"},"tags":["***","***"]}}`,
			wantMasked: true,
		},
		{
			name:       "marker scalar value masked directly",
			in:         `{"user_data":"raw text"}`,
			want:       `{"user_data":"***"}`,
			wantMasked: true,
		},
		{
			name:       "null leaf under marker stays null",
			in:         `{"user_data":{"a":null,"b":"v"}}`,
			want:       `{"user_data":{"a":null,"b":"***"}}`,
			wantMasked: true,
		},
		{
			name: "empty marker object nothing to mask",
			in:   `{"user_data":{},"x":1}`,
			want: `{"user_data":{},"x":1}`,
		},
		{
			name: "exact key match only",
			in:   `{"user_dataX":{"a":"v"},"xuser_data":{"b":"v"},"user_database":"v"}`,
			want: `{"user_dataX":{"a":"v"},"xuser_data":{"b":"v"},"user_database":"v"}`,
		},
		{
			name:       "multiple markers all masked",
			in:         `{"user_data":{"a":"1"},"sub":{"user_data":{"b":"2"}}}`,
			want:       `{"sub":{"user_data":{"b":"***"}},"user_data":{"a":"***"}}`,
			wantMasked: true,
		},
		{
			name: "non object payload no marker",
			in:   `[1,2,3]`,
			want: `[1,2,3]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, masked, err := SanitizePayload(json.RawMessage(tc.in))
			if err != nil {
				t.Fatalf("SanitizePayload(%s) err = %v", tc.in, err)
			}
			if masked != tc.wantMasked {
				t.Fatalf("masked = %v, want %v", masked, tc.wantMasked)
			}
			var wantV, gotV any
			if err := json.Unmarshal([]byte(tc.want), &wantV); err != nil {
				t.Fatalf("want fixture: %v", err)
			}
			if err := json.Unmarshal(got, &gotV); err != nil {
				t.Fatalf("output not valid JSON: %s (%v)", got, err)
			}
			if fmtAny(t, gotV) != fmtAny(t, wantV) {
				t.Fatalf("output = %s, want %s", got, tc.want)
			}
		})
	}
}

func fmtAny(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	return string(b)
}

// TestSanitizePayloadUnmaskedBytesStable:未掩蔽路径必须原字节返回(非
// remarshal)——payload 进暂存表/审计行的字节稳定性是 P1 哈希链规范化
// 前提,也是「脱敏钩子零成本路径」的可观测锚。空格/键序差异在此红。
func TestSanitizePayloadUnmaskedBytesStable(t *testing.T) {
	in := json.RawMessage(`{ "b" : 1 , "a" : "x" }`)
	got, masked, err := SanitizePayload(in)
	if err != nil {
		t.Fatalf("SanitizePayload: %v", err)
	}
	if masked {
		t.Fatal("masked = true, want false")
	}
	if string(got) != string(in) {
		t.Fatalf("unmasked payload rewritten:\n got %s\nwant %s", got, in)
	}
}

// TestSanitizePayloadInvalidJSON:防御臂——validate() 先行保证合法,但钩子
// 独立调用面(未来直传字节调用方)拿非法输入必须响亮,不得静默放行。
func TestSanitizePayloadInvalidJSON(t *testing.T) {
	_, _, err := SanitizePayload(json.RawMessage(`{"user_data":`))
	if err == nil {
		t.Fatal("SanitizePayload(malformed) = nil err, want error")
	}
	if !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("err = %v, want ErrInvalidEvent wrap", err)
	}
}

// TestSanitizePayloadPreservesBigNumbers:掩蔽路径 remarshal 不失真——
// user_data 之外的 >2^53 整数(雪花 id 量级)逐字保持(对抗审计 MEDIUM:
// float64 往返静默失真;UseNumber 修)。
func TestSanitizePayloadPreservesBigNumbers(t *testing.T) {
	in := json.RawMessage(`{"execution_id":96611540228120576,"user_data":{"q":"secret"},"seq":1}`)
	got, masked, err := SanitizePayload(in)
	if err != nil {
		t.Fatalf("SanitizePayload: %v", err)
	}
	if !masked {
		t.Fatal("masked = false, want true")
	}
	s := string(got)
	if !strings.Contains(s, `"execution_id":96611540228120576`) {
		t.Fatalf("big int distorted: %s", s)
	}
	if !strings.Contains(s, `"seq":1`) {
		t.Fatalf("small int distorted: %s", s)
	}
}
