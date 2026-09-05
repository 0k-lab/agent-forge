package gate

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-forge/internal/configjson"
	"agent-forge/internal/githubdelivery"
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
	if c.ownerDigest != sha256.Sum256([]byte("owner-secret")) || len(c.workerTokens) != 1 || c.workerTokens[0].registration.ID != "worker-1" {
		t.Fatal("environment secrets were not resolved")
	}
}

func TestParseConfigDeploymentProfilesContract(t *testing.T) {
	values := map[string]string{"FORGE_OWNER_TOKEN": "owner-secret", "FORGE_WORKER_TOKEN": "worker-secret"}
	getenv := func(name string) string { return values[name] }
	profile := `"deployment_profiles":[{"version":1,"id":"staging","target":"staging-app","prepare":{"argv":["/opt/forge/prepare","staging"],"timeout":"2m"},"activate":{"argv":["/opt/forge/activate","staging"],"timeout":"30s"},"healthcheck":{"argv":["/opt/forge/healthcheck","staging"],"timeout":"10s"},"cleanup_policy":"restore_previous"}],`
	body := strings.Replace(validGateConfig, `"workers":`, profile+`"workers":`, 1)
	body = strings.Replace(body, `"id":"agent-forge"`, `"id":"agent-forge","deployment_profile":"staging"`, 1)

	config, err := ParseConfig([]byte(body), getenv)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.DeploymentProfiles) != 1 || config.DeploymentProfiles[0].Prepare.Timeout != 2*time.Minute || config.Repositories[0].DeploymentProfile != "staging" {
		t.Fatalf("deployment config = %#v, repository = %#v", config.DeploymentProfiles, config.Repositories[0])
	}

	invalid := map[string]string{
		"duplicate profile ID":    strings.Replace(body, `"deployment_profiles":[`, `"deployment_profiles":[{"version":1,"id":"staging","target":"other","prepare":{"argv":["/bin/true"],"timeout":"1s"},"activate":{"argv":["/bin/true"],"timeout":"1s"},"healthcheck":{"argv":["/bin/true"],"timeout":"1s"},"cleanup_policy":"retain"},`, 1),
		"unknown profile version": strings.Replace(body, `"version":1,"id":"staging"`, `"version":2,"id":"staging"`, 1),
		"unknown profile field":   strings.Replace(body, `"target":"staging-app"`, `"target":"staging-app","surprise":true`, 1),
		"unsafe profile ID":       strings.Replace(body, `"id":"staging"`, `"id":"../staging"`, 1),
		"unsafe target identity":  strings.Replace(body, `"target":"staging-app"`, `"target":"../staging"`, 1),
		"empty executable":        strings.Replace(body, `"/opt/forge/prepare","staging"`, `"","staging"`, 1),
		"relative executable":     strings.Replace(body, `"/opt/forge/prepare"`, `"bin/prepare"`, 1),
		"missing argv":            strings.Replace(body, `"argv":["/opt/forge/prepare","staging"],`, ``, 1),
		"invalid duration":        strings.Replace(body, `"timeout":"2m"`, `"timeout":"0s"`, 1),
		"unknown profile":         strings.Replace(body, `"deployment_profile":"staging"`, `"deployment_profile":"missing"`, 1),
		"empty profile selection": strings.Replace(body, `"deployment_profile":"staging"`, `"deployment_profile":""`, 1),
		"unknown cleanup policy":  strings.Replace(body, `"cleanup_policy":"restore_previous"`, `"cleanup_policy":"restore-and-retain"`, 1),
	}
	for name, invalidBody := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(invalidBody), getenv); err == nil {
				t.Fatal("accepted invalid deployment config")
			}
		})
	}

	baseline, err := ParseConfig([]byte(validGateConfig), getenv)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(baseline)
	if err != nil || strings.Contains(string(encoded), "DeploymentProfiles") || strings.Contains(string(encoded), "deployment_profile") {
		t.Fatalf("baseline serialization changed: %s, %v", encoded, err)
	}
}

func TestLoadConfigRejectsOversizedFileWithoutLeakingValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-config-path")
	private := "private-config-content"
	if err := os.WriteFile(path, []byte(strings.Repeat(private, configjson.MaxBytes/len(private)+2)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("accepted oversized config")
	}
	for _, secret := range []string{path, private} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked config data: %q", err)
		}
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

func TestParseConfigResolvesOptionalDeliveryWithoutSerializingAppID(t *testing.T) {
	public := publicGateConfig(t)
	keyDir := t.TempDir()
	if err := os.Chmod(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(keyDir, "github-app.pem")
	if err := os.WriteFile(key, []byte("loaded only when delivery runs"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitDir := t.TempDir()
	if err := os.Chmod(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	git := filepath.Join(gitDir, "git")
	if err := os.WriteFile(git, []byte("#!/bin/sh\nexec "+public.GitExecutable+" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := githubdelivery.ValidateConfig(githubdelivery.Config{Version: 1, APIBase: "https://api.github.com", Owner: "0k-lab", Repository: "agent-forge", LocalRepository: public.PublicRepositoryRoot, GitExecutable: git, AppID: "987654321", PrivateKeyPath: key}); err != nil {
		t.Fatalf("delivery fixture: %v", err)
	}
	body := strings.Replace(validGateConfig, `"repositories": [`, `"public_repository_root":`+quoteJSON(public.PublicRepositoryRoot)+`,"git_executable":`+quoteJSON(git)+`,"delivery":{"api_base":"https://api.github.com","github_app_id_env":"FORGE_GITHUB_APP_ID","github_app_private_key_path":`+quoteJSON(key)+`,"max_attempts":3,"retry_base":"1s","poll_interval":"1s","no_runs_grace":"1s","timeout":"1s"},"repositories": [`, 1)
	body = strings.Replace(body, `"id":"agent-forge"`, `"id":"agent-forge","repository_url":"https://github.com/0k-lab/agent-forge.git"`, 1)
	values := map[string]string{"FORGE_OWNER_TOKEN": "owner", "FORGE_WORKER_TOKEN": "worker", "FORGE_GITHUB_APP_ID": "987654321"}
	config, err := ParseConfig([]byte(body), func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if config.Delivery == nil || config.Delivery.RetryBase != time.Second || config.Delivery.PollInterval != time.Second || config.Delivery.NoRunsGrace != time.Second || config.Delivery.Timeout != time.Second {
		t.Fatalf("delivery = %#v", config.Delivery)
	}
	encoded, err := json.Marshal(config)
	if err != nil || strings.Contains(string(encoded), "987654321") {
		t.Fatalf("serialized delivery credentials: %s, %v", encoded, err)
	}
}
