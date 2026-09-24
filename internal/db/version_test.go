package db

import (
	"testing"
	"testing/fstest"
)

// TestLatestVersion:版本号推导的表驱动锚(注入 FS,离线)。
func TestLatestVersion(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		want    uint
		wantErr bool
	}{
		{
			name: "max of up files, down ignored",
			files: map[string]string{
				"migrations/0001_init.up.sql":     "",
				"migrations/0001_init.down.sql":   "",
				"migrations/0002_auth.up.sql":     "",
				"migrations/0003_approval.up.sql": "",
			},
			want: 3,
		},
		{
			name:  "single migration",
			files: map[string]string{"migrations/0001_init.up.sql": ""},
			want:  1,
		},
		{
			name:    "no up migrations",
			files:   map[string]string{"migrations/README": ""},
			wantErr: true,
		},
		{
			name:    "malformed: no underscore",
			files:   map[string]string{"migrations/broken.up.sql": ""},
			wantErr: true,
		},
		{
			name:    "malformed: version not numeric",
			files:   map[string]string{"migrations/0abc_x.up.sql": ""},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := latestVersion(mapFS(tt.files))
			if tt.wantErr != (err != nil) {
				t.Fatalf("latestVersion() err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got != tt.want {
				t.Fatalf("latestVersion() = %d, want %d", got, tt.want)
			}
		})
	}
}

// mapFS 把文件名集合转成 fstest.MapFS(内容对 ReadDir 无意义,空 MapFile)。
func mapFS(files map[string]string) fstest.MapFS {
	m := make(fstest.MapFS, len(files))
	for name := range files {
		m[name] = &fstest.MapFile{}
	}
	return m
}

// TestLatestMigrationVersionMatchesShippedFiles:真实内嵌 FS 的锚。硬编码
// 当前值(3 = 0003_approval):按目录行数推导会与实现同构,锚就失去意义;
// 新增迁移时随 bump(先红后绿的契约自提醒)。
func TestLatestMigrationVersionMatchesShippedFiles(t *testing.T) {
	got, err := LatestMigrationVersion()
	if err != nil {
		t.Fatalf("LatestMigrationVersion(): %v", err)
	}
	if want := uint(3); got != want {
		t.Fatalf("LatestMigrationVersion() = %d, want %d (bump this anchor when adding migrations)", got, want)
	}
}
