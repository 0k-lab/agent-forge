package protocol

type Message struct {
	Type      string `json:"type"`
	JobID     string `json:"job_id,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
	Input     string `json:"input,omitempty"`
	Result    string `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
}
