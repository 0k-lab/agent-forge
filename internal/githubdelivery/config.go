package githubdelivery

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"unicode/utf8"

	"agent-forge/internal/configjson"
)

var (
	errConfig = errors.New("invalid config")
	envName   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	ownerName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	repoName  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
)

type Config struct {
	Version         int
	APIBase         string
	Owner           string
	Repository      string
	LocalRepository string
	GitExecutable   string
	AppID           string `json:"-"`
	PrivateKeyPath  string
}

type rawConfig struct {
	Version        int    `json:"version"`
	APIBase        string `json:"api_base"`
	Owner          string `json:"owner"`
	Repository     string `json:"repository"`
	LocalRepo      string `json:"local_repository"`
	GitExecutable  string `json:"git_executable"`
	AppIDEnv       string `json:"github_app_id_env"`
	PrivateKeyPath string `json:"github_app_private_key_path"`
}

func LoadConfig(path string) (Config, error) {
	data, err := readProtectedFile(path, 0o600, configjson.MaxBytes)
	if err != nil {
		return Config{}, errConfig
	}
	return ParseConfig(data, os.Getenv)
}

func ParseConfig(data []byte, getenv func(string) string) (Config, error) {
	if !utf8.Valid(data) || getenv == nil {
		return Config{}, errConfig
	}
	var raw rawConfig
	if configjson.Decode(data, &raw) != nil || raw.Version != 1 || raw.APIBase != "https://api.github.com" || !ownerName.MatchString(raw.Owner) || !repoName.MatchString(raw.Repository) || !envName.MatchString(raw.AppIDEnv) {
		return Config{}, errConfig
	}
	appID := getenv(raw.AppIDEnv)
	if _, err := strconv.ParseUint(appID, 10, 64); err != nil || appID == "0" {
		return Config{}, errConfig
	}
	repo, err := ownedDirectory(raw.LocalRepo)
	if err != nil {
		return Config{}, errConfig
	}
	key, err := protectedFile(raw.PrivateKeyPath)
	if err != nil {
		return Config{}, errConfig
	}
	git, err := protectedExecutable(raw.GitExecutable)
	if err != nil {
		return Config{}, errConfig
	}
	return Config{Version: 1, APIBase: raw.APIBase, Owner: raw.Owner, Repository: raw.Repository, LocalRepository: repo, GitExecutable: git, AppID: appID, PrivateKeyPath: key}, nil
}

func ownedDirectory(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errConfig
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return "", errConfig
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 || !owned(info) {
		return "", errConfig
	}
	return path, nil
}

func protectedFile(path string) (string, error) {
	file, err := openProtected(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !owned(info) || info.Mode().Perm() != 0o600 {
		return "", errConfig
	}
	return path, nil
}

func protectedExecutable(path string) (string, error) {
	file, err := openProtectedExecutable(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !executableOwned(info) || info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o111 == 0 {
		return "", errConfig
	}
	return path, nil
}
