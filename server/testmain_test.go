package server

import (
	"database/sql"
	"os"
	"testing"

	"github.com/chu0119/pojun/db"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMain acquires a PostgreSQL advisory lock (7337741002) for the entire
// server test suite so cross-package DELETE cleanup races with db/agent
// packages are avoided when running `go test ./...`.
func TestMain(m *testing.M) {
	// 测试进程不启动漏洞情报同步 / nuclei 模板树：否则每个 NewManager 都会在
	// 临时目录里克隆一份 1.4 万文件的模板树，清理不及就堆积成 GB 级垃圾。
	vulnSyncDisabled = true
	// 测试进程不自动探测视觉能力：保存配置本该后台发真实图片请求，测试里既慢
	// 又依赖外网。需要探测语义的用例直接调纯函数（visionProbeNeeded 等）。
	_ = os.Setenv("POJUN_VISION_AUTOPROBE", "0")
	dsn, _, err := db.DSN()
	if err != nil {
		os.Exit(m.Run())
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil || conn.Ping() != nil {
		os.Exit(m.Run())
	}
	defer conn.Close()
	if _, err := conn.Exec(`SELECT pg_advisory_lock(7337741002)`); err != nil {
		os.Exit(m.Run())
	}
	defer conn.Exec(`SELECT pg_advisory_unlock(7337741002)`) //nolint:errcheck
	os.Exit(m.Run())
}
