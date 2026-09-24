package db

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// LatestMigrationVersion 返回内嵌迁移目录的最大版本号(文件名 <version>_ 前缀)。
// T7.3 安装自检用它比对 schema_migrations 落库版本:二进制内嵌与数据库各持
// 一半真相,本函数是内嵌一半的唯一读取口(migrationsFS 对包外私有)。
func LatestMigrationVersion() (uint, error) {
	return latestVersion(migrationsFS)
}

// latestVersion 在任意 FS 上求最大 *.up.sql 版本号(单测经 fstest.MapFS 注入)。
// 畸形文件名响亮报错——它同样会弄坏 golang-migrate 的 iofs 源驱动,静默跳过
// 只是把损坏延迟到迁移执行期。
func latestVersion(fsys fs.FS) (uint, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return 0, fmt.Errorf("db: read embedded migrations: %w", err)
	}
	var max uint
	seen := false
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		sep := strings.IndexByte(name, '_')
		if sep <= 0 {
			return 0, fmt.Errorf("db: migration filename %q: want <version>_<slug>.up.sql", name)
		}
		n, err := strconv.ParseUint(name[:sep], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("db: migration filename %q: version not a number: %w", name, err)
		}
		if !seen || uint(n) > max {
			max, seen = uint(n), true
		}
	}
	if !seen {
		return 0, fmt.Errorf("db: no *.up.sql migrations found in embedded FS")
	}
	return max, nil
}
