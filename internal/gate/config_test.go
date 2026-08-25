package gate

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

const validGateConfig = `{
  "version": 1,
  "listen": "127.0.0.1:8080",
  "database": "forge.db",
  "owner_token_env": "FORGE_OWNER_TOKEN",
  "recovery_interval": "1s",
  "lease_poll_interval": "100ms",
  "default_pool": "general",
  "lifecycle": {"lease_ttl":"30s","retry_base":"1s","max_attempts":3},
  "default_execution": {"plugin_id":"reference","environment":["PATH"],"plugin_timeout":"15m","check_timeout":"10m","git_timeout":"1m","cleanup_timeout":"10s","plugin_output_bytes":1048576,"check_output_bytes":2048,"git_output_bytes":1048576},
  "workers": [{"id":"worker-1","pool":"general","token_env":"FORGE_WORKER_TOKEN","concurrency":2}],
  "repositories": [{"id":"agent-forge","default_branch":"main","worker_pool":"general","execution":{"plugin_id":"codex","environment":["PATH","CODEX_HOME"],"plugin_timeout":"15m","check_timeout":"10m","git_timeout":"1m","cleanup_timeout":"10s","plugin_output_bytes":1048576,"check_output_bytes":2048,"git_output_bytes":1048576}}]
}`

func TestParseConfigStrictAndResolvesSecrets(t *testing.T) {
	values := map[string]string{"FORGE_OWNER_TOKEN": "owner-secret", "FORGE_WORKER_TOKEN": "worker-secret"}
	c, err := ParseConfig([]byte(validGateConfig), func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 1 || c.RecoveryInterval != time.Second || len(c.Workers) != 1 || len(c.Repositories) != 1 {
		t.Fatalf("config = %#v", c)
	}
	if c.ownerToken != "owner-secret" || len(c.workerTokens) != 1 || c.workerTokens[0].registration.ID != "worker-1" {
		t.Fatal("environment secrets were not resolved")
	}
}

func TestConfiguredWorkerTokensAreNotMapKeys(t *testing.T) {
	values := map[string]string{"FORGE_OWNER_TOKEN": "owner-secret", "FORGE_WORKER_TOKEN": "worker-secret"}
	c, err := ParseConfig([]byte(validGateConfig), func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if reflect.ValueOf(c).FieldByName("workerTokens").Kind() == reflect.Map {
		t.Fatal("worker bearer tokens are map keys")
	}
}

func TestParseConfigRejectsInvalidDocumentsWithoutValues(t *testing.T) {
	tests := map[string]string{
		"unknown field":    strings.Replace(validGateConfig, `"version": 1`, `"version": 1,"surprise":"private-value"`, 1),
		"trailing data":    validGateConfig + `{}`,
		"duplicate key":    strings.Replace(validGateConfig, `"version": 1`, `"version": 1,"version":1`, 1),
		"duplicate worker": strings.Replace(validGateConfig, `"workers": [`, `"workers": [{"id":"worker-1","pool":"general","token_env":"OTHER_TOKEN","concurrency":1},`, 1),
		"unknown pool":     strings.Replace(validGateConfig, `"worker_pool":"general"`, `"worker_pool":"missing-private-pool"`, 1),
		"bad interval":     strings.Replace(validGateConfig, `"recovery_interval": "1s"`, `"recovery_interval":"31s"`, 1),
	}
	getenv := func(string) string { return "distinct-secret" }
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(body), getenv)
			if err == nil {
				t.Fatal("accepted invalid config")
			}
			for _, private := range []string{"private-value", "missing-private-pool", "distinct-secret", "OTHER_TOKEN"} {
				if strings.Contains(err.Error(), private) {
					t.Fatalf("error leaked value: %q", err)
				}
			}
		})
	}
}

func TestParseConfigRejectsSecretCollisionsAndMissingSecrets(t *testing.T) {
	for name, getenv := range map[string]func(string) string{
		"missing":   func(string) string { return "" },
		"collision": func(string) string { return "same-secret" },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(validGateConfig), getenv); err == nil {
				t.Fatal("accepted invalid secret configuration")
			}
		})
	}
}
