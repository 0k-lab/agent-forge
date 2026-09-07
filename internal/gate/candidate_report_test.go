package gate

import (
	"agent-forge/internal/protocol"
	"agent-forge/internal/store"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGateCandidateReportPersistsOrRejectsBeforeMutation(t *testing.T) {
	for _, report := range []string{`{"summary":"Normalize input","changes":["Match equivalent names"]}`, `{"summary":"private","changes":[]}`, `mixed-failure-private`} {
		t.Run(report[:12], func(t *testing.T) {
			s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			job, err := s.CreateCodingJob(protocol.CodingTask{Repository: "/repo", BaseSHA: strings.Repeat("a", 40), Instruction: "edit"})
			if err != nil {
				t.Fatal(err)
			}
			options := DefaultOptions()
			options.LeasePollInterval = time.Millisecond
			h, err := NewHandlerWithOptions(s, map[string]string{"token": "worker-1"}, "owner", options)
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(h)
			defer ts.Close()
			c := dialWorker(t, ts.URL, "worker-1", "token")
			defer c.CloseNow()
			lease := readMessage(t, c)
			message := protocol.Message{Type: protocol.MessageResult, JobID: lease.JobID, AttemptID: lease.AttemptID, CandidateSHA: strings.Repeat("b", 40), Result: report}
			if report == "mixed-failure-private" {
				message.Error = protocol.FailureExecution
				message.Disposition = protocol.DispositionRetryable
			}
			writeMessage(t, c, message)
			ack := readMessage(t, c)
			got, _ := s.Job(job.ID)
			attempts, _ := s.Attempts(job.ID)
			if strings.Contains(report, "private") {
				if ack.Type != protocol.MessageError || ack.Error != "request failed" || got.Status != "leased" || got.Result != "" || attempts[0].Result != "" {
					t.Fatal("malformed report accepted or leaked", ack, got.Status)
				}
			} else if ack.Type != protocol.MessageAck || got.Result != report || attempts[0].Result != report {
				t.Fatalf("report discarded: ack=%s job=%q", ack.Type, got.Result)
			}
		})
	}
}

func TestControlProjectsOnlyTypedCandidateAgentReports(t *testing.T) {
	s, _, h := controlFixture(t)
	report := `{"summary":"Normalize input","changes":["Match equivalent names"]}`
	job, err := s.CreateCodingJob(protocol.CodingTask{BaseSHA: strings.Repeat("a", 40), Instruction: "edit"})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err = s.CompleteCandidateReportAt(job.ID, lease.AttemptID, strings.Repeat("b", 40), report, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	w := controlRequest(h, "GET", "/v1/control/jobs/"+job.ID, "owner", "")
	if w.Code != 200 || strings.Count(w.Body.String(), `"agent_report":`) < 2 || !strings.Contains(w.Body.String(), `"summary":"Normalize input"`) {
		t.Fatal("missing typed run/attempt report", w.Body.String())
	}
	for _, legacy := range []string{"private arbitrary output", `{"summary":"x","changes":[]}`, report, ""} {
		job, err := s.CreateJob("legacy")
		if err != nil {
			t.Fatal(err)
		}
		lease, ok, err := s.LeaseNext("worker")
		if err != nil || !ok {
			t.Fatal(err)
		}
		if _, err = s.CompleteAt(job.ID, lease.AttemptID, legacy, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		w := controlRequest(h, "GET", "/v1/control/jobs/"+job.ID, "owner", "")
		if w.Code != 200 || strings.Contains(w.Body.String(), `"agent_report"`) || strings.Contains(w.Body.String(), "private arbitrary") {
			t.Fatal("legacy result exposed as agent report")
		}
	}
}
