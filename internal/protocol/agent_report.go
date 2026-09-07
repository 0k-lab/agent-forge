package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"agent-forge/internal/configjson"
)

const MaxAgentReportBytes = 25 << 10 // Includes worst-case JSON escaping of 4 KiB of report text.

type AgentReport struct {
	Summary string   `json:"summary"`
	Changes []string `json:"changes"`
}

func ValidateAgentReport(report *AgentReport) error {
	if report == nil {
		return nil
	}
	valid := func(value string, limit int) bool {
		return value != "" && len(value) <= limit && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, func(r rune) bool {
			return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
		})
	}
	if !valid(report.Summary, 1024) || len(report.Changes) < 1 || len(report.Changes) > 12 {
		return errors.New("invalid agent report")
	}
	for _, change := range report.Changes {
		if !valid(change, 256) {
			return errors.New("invalid agent report")
		}
	}
	return nil
}

func EncodeAgentReport(report *AgentReport) (string, error) {
	if err := ValidateAgentReport(report); err != nil {
		return "", err
	}
	if report == nil {
		return "", nil
	}
	body, err := json.Marshal(report)
	return string(body), err
}

func DecodeAgentReport(value string) (*AgentReport, error) {
	if value == "" {
		return nil, nil
	}
	var report AgentReport
	if len(value) > MaxAgentReportBytes || configjson.Decode([]byte(value), &report) != nil || ValidateAgentReport(&report) != nil {
		return nil, errors.New("invalid agent report")
	}
	return &report, nil
}
