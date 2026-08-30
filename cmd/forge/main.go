package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"agent-forge/internal/buildinfo"
	"agent-forge/internal/configjson"
	"agent-forge/internal/linuxinstall"
	"agent-forge/internal/protocol"
)

const maxErrorBytes = 64 << 10

type ErrorCode string

const (
	CodeInvalidUsage  ErrorCode = "invalid_usage"
	CodeInvalidInput  ErrorCode = "invalid_input"
	CodeInvalidConfig ErrorCode = "invalid_config"
	CodeHTTPFailure   ErrorCode = "http_failure"
	CodeTimeout       ErrorCode = "timeout"
	CodeJobFailed     ErrorCode = "job_failed"
	CodeUnhealthy     ErrorCode = "unhealthy"
)

type CLIError struct {
	Code         ErrorCode
	InstallStage string
}

func (e *CLIError) Error() string { return string(e.Code) }

func fail(code ErrorCode) error { return &CLIError{Code: code} }

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr); err != nil {
		os.Exit(reportFailure(os.Stderr, err))
	}
}

func reportFailure(stderr io.Writer, err error) int {
	status := exitStatus(err)
	code := CodeHTTPFailure
	var cliErr *CLIError
	if errors.As(err, &cliErr) {
		code = cliErr.Code
	}
	if code == CodeUnhealthy {
		return status
	}
	failure := map[string]any{"component": "forge", "event": "client_failed", "failure_code": code}
	if cliErr != nil && cliErr.InstallStage != "" {
		failure["install_stage"] = cliErr.InstallStage
	}
	_ = json.NewEncoder(stderr).Encode(failure)
	return status
}

func exitStatus(err error) int {
	var cliErr *CLIError
	if errors.As(err, &cliErr) && cliErr.Code == CodeUnhealthy {
		return 2
	}
	return 1
}

type commandOptions struct {
	url      string
	tokenEnv string
	timeout  time.Duration
	poll     time.Duration
	file     string
	wait     bool
}

var installLinux = linuxinstall.Install
var doctorLinux = linuxinstall.Doctor
var effectiveUID = os.Geteuid

func run(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) error {
	if requested, err := buildinfo.WriteIfRequested(args, stdout, "forge"); requested {
		return err
	}
	if len(args) == 0 || getenv == nil {
		return fail(CodeInvalidUsage)
	}
	name := args[0]
	if name == "install" {
		return runInstall(args[1:])
	}
	if name == "doctor" {
		return runDoctor(args[1:], stdout)
	}
	if name != "submit" && name != "wait" && name != "status" && name != "events" && name != "result" {
		return fail(CodeInvalidUsage)
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	options := commandOptions{url: getenv("FORGE_GATE_URL"), tokenEnv: "FORGE_OWNER_TOKEN", timeout: 10 * time.Minute, poll: time.Second, file: "-"}
	fs.StringVar(&options.url, "url", options.url, "Gate origin")
	fs.StringVar(&options.tokenEnv, "token-env", options.tokenEnv, "owner token environment variable")
	if name == "submit" {
		fs.StringVar(&options.file, "file", options.file, "task JSON file or - for stdin")
		fs.BoolVar(&options.wait, "wait", false, "wait for a terminal result")
	}
	if name == "submit" || name == "wait" {
		fs.DurationVar(&options.timeout, "timeout", options.timeout, "bounded wait timeout")
		fs.DurationVar(&options.poll, "poll", options.poll, "poll interval")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return fail(CodeInvalidUsage)
	}
	if !regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`).MatchString(options.tokenEnv) {
		return fail(CodeInvalidConfig)
	}
	c, err := newClient(options.url, getenv(options.tokenEnv))
	if err != nil {
		return err
	}
	switch name {
	case "submit":
		if fs.NArg() != 0 {
			return fail(CodeInvalidUsage)
		}
		body, err := readTask(options.file, stdin)
		if err != nil {
			return err
		}
		response, err := c.do(http.MethodPost, "/v1/jobs", body)
		if err != nil {
			return err
		}
		var job struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if json.Unmarshal(response, &job) != nil || !validID(job.ID) {
			return fail(CodeHTTPFailure)
		}
		if options.wait {
			return waitForJob(c, job.ID, options.timeout, options.poll, stdout)
		}
		return printJSON(stdout, response)
	case "wait":
		if fs.NArg() != 1 || !validID(fs.Arg(0)) || !validWait(options.timeout, options.poll) {
			return fail(CodeInvalidUsage)
		}
		return waitForJob(c, fs.Arg(0), options.timeout, options.poll, stdout)
	default:
		if fs.NArg() != 1 || !validID(fs.Arg(0)) {
			return fail(CodeInvalidUsage)
		}
		suffix := map[string]string{"status": "/status", "events": "/events", "result": "/result"}[name]
		response, err := c.get("/v1/jobs/" + fs.Arg(0) + suffix)
		if err != nil {
			return err
		}
		return printJSON(stdout, response)
	}
}

func runDoctor(args []string, stdout io.Writer) error {
	if len(args) != 0 || effectiveUID() != 0 {
		return fail(CodeInvalidUsage)
	}
	report := doctorLinux(linuxinstall.DoctorOptions{Services: linuxinstall.HostServiceManager{}, Ownership: linuxinstall.HostOwnershipManager{}, Account: linuxinstall.HostAccountManager{}})
	for _, check := range report.Checks {
		status := "FAIL"
		if check.OK {
			status = "PASS"
		}
		fmt.Fprintf(stdout, "%s %s\n", status, check.ID)
	}
	if !report.Healthy() {
		fmt.Fprintln(stdout, "RESULT unhealthy")
		return fail(CodeUnhealthy)
	}
	fmt.Fprintln(stdout, "RESULT healthy")
	return nil
}

func runInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o linuxinstall.Options
	o.InstallerVersion, o.InstallerCommit = buildinfo.Version, buildinfo.Commit
	fs.StringVar(&o.Version, "version", "", "exact release version")
	fs.StringVar(&o.Commit, "commit", "", "full release commit")
	fs.StringVar(&o.AssetDir, "asset-dir", "", "absolute offline release asset directory")
	fs.StringVar(&o.SHA256SUMSSHA256, "sha256sums-sha256", "", "trusted SHA256SUMS digest")
	fs.BoolVar(&o.EnableNow, "enable-now", false, "enable and start services after installation")
	fs.BoolVar(&o.RunAsRoot, "run-as-root", false, "run Gate and Worker as root")
	fs.BoolVar(&o.Upgrade, "upgrade", false, "upgrade an existing installation to a newer exact release")
	o.Account = linuxinstall.HostAccountManager{}
	o.Services = linuxinstall.HostServiceManager{}
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || effectiveUID() != 0 || !regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(o.Version) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(o.Commit) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(o.SHA256SUMSSHA256) || !strings.HasPrefix(o.AssetDir, "/") {
		return fail(CodeInvalidUsage)
	}
	if err := installLinux(o); err != nil {
		stage, _ := linuxinstall.FailureStage(err)
		return &CLIError{Code: CodeInvalidInput, InstallStage: stage}
	}
	return nil
}

type submission struct {
	Input             string            `json:"input,omitempty"`
	RepositoryID      string            `json:"repository_id,omitempty"`
	Repository        string            `json:"repository,omitempty"`
	BaseSHA           string            `json:"base_sha,omitempty"`
	Instruction       string            `json:"instruction,omitempty"`
	Tests             [][]string        `json:"tests,omitempty"`
	CommitAuthorName  string            `json:"commit_author_name,omitempty"`
	CommitAuthorEmail string            `json:"commit_author_email,omitempty"`
	SourceRef         configjson.String `json:"source_ref,omitempty"`
}

func readTask(name string, stdin io.Reader) ([]byte, error) {
	var reader io.Reader = stdin
	var file *os.File
	var err error
	if name != "-" {
		file, err = os.Open(name)
		if err != nil {
			return nil, fail(CodeInvalidInput)
		}
		defer file.Close()
		reader = file
	}
	if reader == nil {
		return nil, fail(CodeInvalidInput)
	}
	body, err := io.ReadAll(io.LimitReader(reader, configjson.MaxBytes+1))
	if err != nil || len(body) > configjson.MaxBytes {
		return nil, fail(CodeInvalidInput)
	}
	var task submission
	if configjson.Decode(body, &task) != nil {
		return nil, fail(CodeInvalidInput)
	}
	return bytes.TrimSpace(body), nil
}

type client struct {
	base  *url.URL
	token string
	http  *http.Client
}

var httpTransport http.RoundTripper = http.DefaultTransport

func newClient(rawURL, token string) (*client, error) {
	base, err := url.Parse(rawURL)
	if err != nil || token == "" || base.User != nil || base.Host == "" || base.RawQuery != "" || base.Fragment != "" || base.RawPath != "" || base.Opaque != "" || base.Path != "" && base.Path != "/" || base.Scheme != "http" && base.Scheme != "https" {
		return nil, fail(CodeInvalidConfig)
	}
	base.Path = ""
	origin := base.Scheme + "://" + base.Host
	httpClient := &http.Client{Transport: httpTransport, Timeout: 30 * time.Second}
	httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme+"://"+req.URL.Host != origin {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("redirect limit")
		}
		return nil
	}
	return &client{base: base, token: token, http: httpClient}, nil
}

func (c *client) get(path string) ([]byte, error) { return c.do(http.MethodGet, path, nil) }

func (c *client) getContext(ctx context.Context, path string) ([]byte, error) {
	return c.doContext(ctx, http.MethodGet, path, nil)
}

func (c *client) do(method, path string, body []byte) ([]byte, error) {
	return c.doContext(context.Background(), method, path, body)
}

func (c *client) doContext(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, bytes.NewReader(body))
	if err != nil {
		return nil, fail(CodeHTTPFailure)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fail(CodeTimeout)
		}
		return nil, fail(CodeHTTPFailure)
	}
	defer response.Body.Close()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fail(CodeTimeout)
	}
	limit := int64(protocol.MaxWorkerMessageBytes)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		limit = maxErrorBytes
	}
	result, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(result)) > limit || response.StatusCode < 200 || response.StatusCode >= 300 || !json.Valid(result) {
		return nil, fail(CodeHTTPFailure)
	}
	return result, nil
}

func waitForJob(c *client, id string, timeout, poll time.Duration, output io.Writer) error {
	if !validWait(timeout, poll) {
		return fail(CodeInvalidUsage)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		body, err := c.getContext(ctx, "/v1/jobs/"+id+"/status")
		if err != nil {
			return err
		}
		var job struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(body, &job) != nil || job.Status == "" {
			return fail(CodeHTTPFailure)
		}
		if job.Status == "succeeded" || job.Status == "failed" {
			result, err := c.getContext(ctx, "/v1/jobs/"+id+"/result")
			if err != nil {
				return err
			}
			if err := printJSON(output, result); err != nil {
				return err
			}
			if job.Status == "failed" {
				return fail(CodeJobFailed)
			}
			return nil
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fail(CodeTimeout)
		case <-timer.C:
		}
	}
}

func validWait(timeout, poll time.Duration) bool {
	return timeout > 0 && timeout <= 24*time.Hour && poll >= time.Millisecond && poll <= time.Minute && poll <= timeout
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := range len(id) {
		if id[i] < '0' || id[i] > '9' && (id[i] < 'a' || id[i] > 'f') {
			return false
		}
	}
	return true
}

func printJSON(output io.Writer, body []byte) error {
	if output == nil {
		return fail(CodeInvalidConfig)
	}
	_, err := fmt.Fprintln(output, strings.TrimSpace(string(body)))
	if err != nil {
		return fail(CodeInvalidConfig)
	}
	return nil
}
