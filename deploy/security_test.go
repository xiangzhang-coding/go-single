package deploy_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const canaryJWT = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiI0MiJ9.sensitive-signature"

func TestNginxWebSocketAccessLogOmitsQueryAndHeaders(t *testing.T) {
	content, err := os.ReadFile("nginx/nginx.conf")
	require.NoError(t, err)
	config := string(content)
	require.Contains(t, config, "log_format safe")
	require.Contains(t, config, "access_log /var/log/nginx/access.log safe;")
	require.Contains(t, config, `if ($arg_token != "") { return 400; }`)
	require.NotContains(t, config, "$proxy_add_x_forwarded_for")

	for line := range strings.Lines(config) {
		if !strings.Contains(line, "log_format safe") {
			continue
		}
		require.NotContains(t, line, "$request ")
		require.NotContains(t, line, "$request_uri")
		require.NotContains(t, line, "$args")
		require.NotContains(t, line, "$http_sec_websocket_protocol")
		require.NotContains(t, line, "$http_referer")
		require.NotContains(t, line, "$http_user_agent")
	}
}

func TestPromtailRedactsJWTsFromEveryScrapeJob(t *testing.T) {
	type replaceStage struct {
		Expression string `yaml:"expression"`
		Replace    string `yaml:"replace"`
	}
	type config struct {
		ScrapeConfigs []struct {
			JobName        string `yaml:"job_name"`
			PipelineStages []struct {
				Replace *replaceStage `yaml:"replace"`
			} `yaml:"pipeline_stages"`
		} `yaml:"scrape_configs"`
	}

	content, err := os.ReadFile("monitoring/promtail/config.yml")
	require.NoError(t, err)
	var cfg config
	require.NoError(t, yaml.Unmarshal(content, &cfg))
	require.NotEmpty(t, cfg.ScrapeConfigs)

	for _, scrape := range cfg.ScrapeConfigs {
		redacted := "GET /ws?token=" + canaryJWT
		found := false
		for _, stage := range scrape.PipelineStages {
			if stage.Replace == nil {
				continue
			}
			found = true
			expression, compileErr := regexp.Compile(stage.Replace.Expression)
			require.NoError(t, compileErr, scrape.JobName)
			redacted = expression.ReplaceAllString(redacted, stage.Replace.Replace)
		}
		require.True(t, found, "%s 缺少脱敏阶段", scrape.JobName)
		require.NotContains(t, redacted, canaryJWT, scrape.JobName)
	}
}

func TestMonitoringPortsBindToLoopbackByDefault(t *testing.T) {
	type composeConfig struct {
		Services map[string]struct {
			Ports []string `yaml:"ports"`
		} `yaml:"services"`
	}

	content, err := os.ReadFile("docker-compose.yml")
	require.NoError(t, err)
	var cfg composeConfig
	require.NoError(t, yaml.Unmarshal(content, &cfg))
	for _, service := range []string{"prometheus", "grafana", "loki"} {
		ports := cfg.Services[service].Ports
		require.NotEmpty(t, ports, service)
		for _, port := range ports {
			require.True(t, strings.HasPrefix(port, "127.0.0.1:"), "%s 端口默认必须仅绑定回环地址: %s", service, port)
		}
	}
}

func TestNginxUsesDedicatedTrustedProxyAddress(t *testing.T) {
	compose, err := os.ReadFile("docker-compose.yml")
	require.NoError(t, err)
	require.Contains(t, string(compose), "ipv4_address: 172.30.0.10")

	appConfig, err := os.ReadFile("../configs/config.yaml")
	require.NoError(t, err)
	require.Contains(t, string(appConfig), "- 172.30.0.10")
	require.NotContains(t, string(appConfig), "172.16.0.0/12")
}

func TestNginxBodyBudgetsMatchBackendContracts(t *testing.T) {
	content, err := os.ReadFile("nginx/nginx.conf")
	require.NoError(t, err)
	config := string(content)

	require.Contains(t, config, "client_max_body_size 64k;")
	require.Contains(t, config, "location = /api/files")
	require.Equal(t, 1, strings.Count(config, "client_max_body_size 21m;"))
	require.Regexp(t, regexp.MustCompile(`(?s)location = /api/files\s*\{.*?client_max_body_size 21m;`), config)
}

func TestPagesDeploymentRunsAfterSuccessfulCI(t *testing.T) {
	pages, err := os.ReadFile("../.github/workflows/pages-deploy.yml")
	require.NoError(t, err)
	pagesWorkflow := string(pages)
	require.Contains(t, pagesWorkflow, "workflow_call:")
	require.NotContains(t, pagesWorkflow, "  push:")

	ci, err := os.ReadFile("../.github/workflows/ci.yml")
	require.NoError(t, err)
	ciWorkflow := string(ci)
	require.Regexp(t, regexp.MustCompile(`(?s)deploy-pages:.*?needs: lint-and-test.*?uses: \./\.github/workflows/pages-deploy\.yml`), ciWorkflow)
}

func TestCloudflareAPIBaseIncludesBackendAPIPrefix(t *testing.T) {
	docs, err := os.ReadFile("../docs/DEPLOYMENT.md")
	require.NoError(t, err)
	require.Contains(t, string(docs), "https://api.example.com/api")

	workflow, err := os.ReadFile("../.github/workflows/pages-deploy.yml")
	require.NoError(t, err)
	require.Contains(t, string(workflow), "bun run scripts/validate-pages-env.ts")
}
