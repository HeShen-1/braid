package api

import (
	"strings"
	"testing"
)

// testSecret 满足 auth.MinSecretLen 的低熵占位词组(纯词组无 hex/熵段,
// auth fixtures_test 同款形态——带数字串的值会触发 gitleaks
// generic-api-key 熵规则;测试密钥不进任何真实面)。
const testSecret = "t54-api-test-jwt-signing-secret-words-only-placeholder"

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr string
	}{
		{
			name:    "secret missing fails at startup",
			env:     map[string]string{},
			wantErr: "jwt secret shorter than",
		},
		{
			name: "secret too short fails",
			env:  map[string]string{"BRAID_AUTH_JWT_SECRET": "short"},
			// 长度 < MinSecretLen 的判定在 auth.Validate 与本包一致
			wantErr: "jwt secret shorter than",
		},
		{
			name: "defaults with secret",
			env:  map[string]string{"BRAID_AUTH_JWT_SECRET": testSecret},
			want: Config{Addr: DefaultAddr, JWTSecret: testSecret, SnowflakeNode: 1},
		},
		{
			name: "full env",
			env: map[string]string{
				"BRAID_AUTH_JWT_SECRET":    testSecret,
				"BRAID_API_ADDR":           "10.0.0.1:9000",
				"BRAID_API_SNOWFLAKE_NODE": "7",
				"BRAID_TOOL_ALLOWLIST":     "http , file_read",
			},
			want: Config{
				Addr:          "10.0.0.1:9000",
				JWTSecret:     testSecret,
				ToolAllowlist: []string{"http", "file_read"},
				SnowflakeNode: 7,
			},
		},
		{
			name: "allowlist empty entry rejected",
			env: map[string]string{
				"BRAID_AUTH_JWT_SECRET": testSecret,
				"BRAID_TOOL_ALLOWLIST":  "http,,file_read",
			},
			wantErr: "empty entry",
		},
		{
			name: "allowlist duplicate rejected",
			env: map[string]string{
				"BRAID_AUTH_JWT_SECRET": testSecret,
				"BRAID_TOOL_ALLOWLIST":  "http,http",
			},
			wantErr: `duplicate "http"`,
		},
		{
			name: "snowflake node overflow rejected",
			env: map[string]string{
				"BRAID_AUTH_JWT_SECRET":    testSecret,
				"BRAID_API_SNOWFLAKE_NODE": "1024",
			},
			wantErr: "want 0..1023",
		},
		{
			name: "snowflake node negative rejected",
			env: map[string]string{
				"BRAID_AUTH_JWT_SECRET":    testSecret,
				"BRAID_API_SNOWFLAKE_NODE": "-1",
			},
			wantErr: "want 0..1023",
		},
		{
			// uint16 截断带:65536 mod 2^16 = 0,若在转换后才校验会静默
			// 变成合法节点号 0(审查发现 #2 的回归锚)。
			name: "snowflake node uint16 truncation band rejected",
			env: map[string]string{
				"BRAID_AUTH_JWT_SECRET":    testSecret,
				"BRAID_API_SNOWFLAKE_NODE": "65536",
			},
			wantErr: "want 0..1023",
		},
		{
			name: "bad addr rejected",
			env: map[string]string{
				"BRAID_AUTH_JWT_SECRET": testSecret,
				"BRAID_API_ADDR":        "no-port-here",
			},
			wantErr: "addr",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ConfigFromEnv(func(k string) string { return tt.env[k] })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ConfigFromEnv() err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ConfigFromEnv() unexpected err: %v", err)
			}
			if got.Addr != tt.want.Addr || got.JWTSecret != tt.want.JWTSecret ||
				got.SnowflakeNode != tt.want.SnowflakeNode {
				t.Fatalf("ConfigFromEnv() = %+v, want %+v", got, tt.want)
			}
			if len(got.ToolAllowlist) != len(tt.want.ToolAllowlist) {
				t.Fatalf("ToolAllowlist = %v, want %v", got.ToolAllowlist, tt.want.ToolAllowlist)
			}
			for i := range got.ToolAllowlist {
				if got.ToolAllowlist[i] != tt.want.ToolAllowlist[i] {
					t.Fatalf("ToolAllowlist = %v, want %v", got.ToolAllowlist, tt.want.ToolAllowlist)
				}
			}
		})
	}
}

// TestConfigValidateSecretBoundary 钉死密钥下界与 auth.MinSecretLen 一致:
// 32 字节恰好通过,31 字节拒绝。
func TestConfigValidateSecretBoundary(t *testing.T) {
	c := DefaultConfig()
	c.JWTSecret = strings.Repeat("x", 31)
	if err := c.Validate(); err == nil {
		t.Fatal("31-byte secret accepted, want rejection")
	}
	c.JWTSecret = strings.Repeat("x", 32)
	if err := c.Validate(); err != nil {
		t.Fatalf("32-byte secret rejected: %v", err)
	}
}
