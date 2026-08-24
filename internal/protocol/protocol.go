package protocol

type Message struct {
	Type         string      `json:"type"`
	JobID        string      `json:"job_id,omitempty"`
	AttemptID    string      `json:"attempt_id,omitempty"`
	Input        string      `json:"input,omitempty"`
	Task         *CodingTask `json:"task,omitempty"`
	Result       string      `json:"result,omitempty"`
	CandidateSHA string      `json:"candidate_sha,omitempty"`
	Error        string      `json:"error,omitempty"`
}

type CodingTask struct {
	Repository  string     `json:"repository"`
	BaseSHA     string     `json:"base_sha"`
	Instruction string     `json:"instruction"`
	Tests       [][]string `json:"tests"`
}
