package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"agent-forge/internal/configjson"
	"agent-forge/internal/protocol"
)

const (
	ResolvedPolicyVersion  = protocol.ResolvedPolicyVersion
	RetryAlgorithmV1       = protocol.RetryAlgorithmV1
	MaxResolvedPolicyBytes = 16 << 10
)

var policyID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func validSessionGeneration(generation string) bool { return lowerHex(generation, 32) }

type ExecutionPolicy = protocol.ExecutionPolicy
type ResolvedPolicy struct {
	DeliveryPolicy protocol.DeliveryPolicy `json:"delivery_policy,omitempty"`
	Version        int                     `json:"version"`
	WorkerPool     string                  `json:"worker_pool"`
	LeaseTTLNanos  int64                   `json:"lease_ttl_nanos"`
	RetryBaseNanos int64                   `json:"retry_base_nanos"`
	MaxAttempts    int                     `json:"max_attempts"`
	RetryAlgorithm string                  `json:"retry_algorithm"`
	RetryMaxNanos  int64                   `json:"retry_max_nanos"`
	Execution      ExecutionPolicy         `json:"execution"`
}

// WorkerPolicy projects only the negotiated v1 execution/lease policy.
func (p ResolvedPolicy) WorkerPolicy() protocol.ResolvedPolicy {
	return protocol.ResolvedPolicy{Version: p.Version, WorkerPool: p.WorkerPool,
		LeaseTTLNanos: p.LeaseTTLNanos, RetryBaseNanos: p.RetryBaseNanos,
		MaxAttempts: p.MaxAttempts, RetryAlgorithm: p.RetryAlgorithm,
		RetryMaxNanos: p.RetryMaxNanos, Execution: p.Execution}
}

func (p ResolvedPolicy) Validate() error {
	if err := p.DeliveryPolicy.Validate(); err != nil {
		return err
	}
	return p.WorkerPolicy().Validate()
}

func CanonicalPolicy(policy ResolvedPolicy) ([]byte, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if policy.Execution.Environment == nil {
		policy.Execution.Environment = []string{}
	}
	body, err := json.Marshal(policy)
	if err != nil || len(body) > MaxResolvedPolicyBytes {
		return nil, errors.New("invalid resolved policy")
	}
	return body, nil
}

func DecodeCanonicalPolicy(body []byte) (ResolvedPolicy, error) {
	if len(body) == 0 || len(body) > MaxResolvedPolicyBytes {
		return ResolvedPolicy{}, errors.New("corrupt resolved policy")
	}
	var policy ResolvedPolicy
	if err := configjson.Decode(body, &policy); err != nil || policy.Validate() != nil {
		return ResolvedPolicy{}, errors.New("corrupt resolved policy")
	}
	canonical, err := CanonicalPolicy(policy)
	if err != nil || !bytes.Equal(canonical, body) {
		return ResolvedPolicy{}, errors.New("corrupt resolved policy")
	}
	return policy, nil
}

func retryDelay(policy ResolvedPolicy, ordinal int) time.Duration {
	delay, capNanos := policy.RetryBaseNanos, policy.RetryMaxNanos
	for i := 1; i < ordinal && delay < capNanos; i++ {
		if delay > capNanos/2 {
			delay = capNanos
		} else {
			delay *= 2
		}
	}
	if delay > capNanos {
		delay = capNanos
	}
	return time.Duration(delay)
}
