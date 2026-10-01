package snowflake

import (
	"strings"
	"testing"
)

// TestParseNodeSpec 三层解析(D-t151 决策 2):显式 env 数字 > 内建默认 >
// 字面 "auto" 哨兵;缺省 = 内建默认。坏值启动即败(数字越界在转换前
// 拒绝,镜像各组件 uint16 截断防护先例)。
func TestParseNodeSpec(t *testing.T) {
	def := Explicit(2)
	cases := []struct {
		name string
		raw  string
		want NodeSpec
		err  string
	}{
		{name: "empty falls back to builtin default", raw: "", want: def},
		{name: "auto sentinel", raw: "auto", want: Auto()},
		{name: "explicit zero", raw: "0", want: Explicit(0)},
		{name: "explicit low", raw: "1", want: Explicit(1)},
		{name: "explicit max", raw: "1023", want: Explicit(1023)},
		{name: "negative rejected", raw: "-1", err: `want "auto" or 0..1023`},
		{name: "above max rejected", raw: "1024", err: `want "auto" or 0..1023`},
		{name: "far above max rejected (uint16 fold guard)", raw: "65536", err: `want "auto" or 0..1023`},
		{name: "non-numeric rejected", raw: "abc", err: `want "auto" or 0..1023`},
		{name: "case-sensitive auto", raw: "AUTO", err: `want "auto" or 0..1023`},
		{name: "whitespace rejected", raw: " 1", err: `want "auto" or 0..1023`},
		{name: "trailing junk rejected", raw: "1x", err: `want "auto" or 0..1023`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseNodeSpec(tc.raw, def)
			if tc.err != "" {
				if err == nil {
					t.Fatalf("ParseNodeSpec(%q) = %+v, want error containing %q", tc.raw, got, tc.err)
				}
				if !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("ParseNodeSpec(%q) err = %q, want containing %q", tc.raw, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseNodeSpec(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ParseNodeSpec(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}
