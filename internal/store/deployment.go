package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"agent-forge/internal/configjson"
)

const (
	ResolvedDeploymentProfileVersion  = 1
	MaxResolvedDeploymentProfileBytes = 16 << 10
)

type ResolvedDeploymentCommand struct {
	Argv         []string `json:"argv"`
	TimeoutNanos int64    `json:"timeout_nanos"`
}

type ResolvedDeploymentProfile struct {
	Version       int                       `json:"version"`
	ID            string                    `json:"id"`
	Target        string                    `json:"target"`
	Prepare       ResolvedDeploymentCommand `json:"prepare"`
	Activate      ResolvedDeploymentCommand `json:"activate"`
	Healthcheck   ResolvedDeploymentCommand `json:"healthcheck"`
	CleanupPolicy string                    `json:"cleanup_policy"`
}

func CanonicalDeploymentProfile(profile ResolvedDeploymentProfile) ([]byte, error) {
	if !validDeploymentProfile(profile) {
		return nil, errors.New("invalid resolved deployment profile")
	}
	body, err := json.Marshal(profile)
	if err != nil || len(body) > MaxResolvedDeploymentProfileBytes {
		return nil, errors.New("invalid resolved deployment profile")
	}
	return body, nil
}

func DecodeCanonicalDeploymentProfile(body []byte) (ResolvedDeploymentProfile, error) {
	if len(body) == 0 || len(body) > MaxResolvedDeploymentProfileBytes {
		return ResolvedDeploymentProfile{}, errors.New("corrupt resolved deployment profile")
	}
	var profile ResolvedDeploymentProfile
	if configjson.Decode(body, &profile) != nil || !validDeploymentProfile(profile) {
		return ResolvedDeploymentProfile{}, errors.New("corrupt resolved deployment profile")
	}
	canonical, err := CanonicalDeploymentProfile(profile)
	if err != nil || !bytes.Equal(canonical, body) {
		return ResolvedDeploymentProfile{}, errors.New("corrupt resolved deployment profile")
	}
	return profile, nil
}

func validDeploymentProfile(profile ResolvedDeploymentProfile) bool {
	if profile.Version != ResolvedDeploymentProfileVersion || !policyID.MatchString(profile.ID) || !policyID.MatchString(profile.Target) || profile.CleanupPolicy != "restore_previous" && profile.CleanupPolicy != "retain" && profile.CleanupPolicy != "cleanup" {
		return false
	}
	for _, command := range []ResolvedDeploymentCommand{profile.Prepare, profile.Activate, profile.Healthcheck} {
		if len(command.Argv) == 0 || len(command.Argv) > 64 || !filepath.IsAbs(command.Argv[0]) || command.TimeoutNanos < int64(time.Millisecond) || command.TimeoutNanos > int64(24*time.Hour) {
			return false
		}
		for _, arg := range command.Argv {
			if arg == "" || len(arg) > 4096 || !utf8.ValidString(arg) || strings.IndexByte(arg, 0) >= 0 {
				return false
			}
		}
	}
	return true
}
