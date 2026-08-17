package testsupport

import (
	"os"
)

const integrationRequiredEnv = "GO_SINGLE_INTEGRATION_REQUIRED"

type testHandle interface {
	Helper()
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}

// RequireDependency keeps dependency-backed tests optional locally but makes
// initialization failures fatal in CI, where the services are mandatory.
func RequireDependency(t testHandle, dependency string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if os.Getenv(integrationRequiredEnv) == "1" {
		t.Fatalf("%s 集成测试依赖初始化失败: %v", dependency, err)
		return
	}
	t.Skipf("%s 不可用，跳过集成测试（先 docker compose up -d）: %v", dependency, err)
}
