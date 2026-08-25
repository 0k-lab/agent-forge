package pluginprotocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	Version               = "v1"
	MaxFrameBytes         = 1 << 20
	MaxProgressFrames     = 128
	MaxTextBytes          = 64 << 10
	MaxProgressTextBytes  = 1024
	MaxCommitSubjectBytes = 256
)

type Capability string

const (
	Text          Capability = "text"
	WorkspaceEdit Capability = "workspace_edit"
	Progress      Capability = "progress"
	Cancel        Capability = "cancel"
	CommitSubject Capability = "commit_subject"
)

type Limits struct {
	FrameBytes         int `json:"frame_bytes"`
	ProgressFrames     int `json:"progress_frames"`
	TextBytes          int `json:"text_bytes"`
	ProgressTextBytes  int `json:"progress_text_bytes"`
	CommitSubjectBytes int `json:"commit_subject_bytes"`
}

func V1Limits() Limits {
	return Limits{MaxFrameBytes, MaxProgressFrames, MaxTextBytes, MaxProgressTextBytes, MaxCommitSubjectBytes}
}

var ErrOperation = errors.New("plugin operation failed")
var ErrCancelled = errors.New("plugin operation cancelled")
var errCancelSent = errors.New("plugin cancel sent")

type Request struct {
	ID          string
	Operation   Capability
	Input       string
	Workspace   string
	Instruction string
	TimeoutMS   int64
}

type Result struct {
	Output        string
	CommitSubject *string
}

type initialize struct {
	Version      string       `json:"version"`
	ID           string       `json:"id"`
	Type         string       `json:"type"`
	Capabilities []Capability `json:"capabilities"`
	Limits       Limits       `json:"limits"`
}

type initialized struct {
	Version      string       `json:"version"`
	ID           string       `json:"id"`
	Type         string       `json:"type"`
	Capabilities []Capability `json:"capabilities"`
}

type textExecute struct {
	Version   string     `json:"version"`
	ID        string     `json:"id"`
	Type      string     `json:"type"`
	Operation Capability `json:"operation"`
	Input     string     `json:"input"`
}

type workspaceExecute struct {
	Version     string     `json:"version"`
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	Operation   Capability `json:"operation"`
	Workspace   string     `json:"workspace"`
	Instruction string     `json:"instruction"`
	TimeoutMS   int64      `json:"timeout_ms"`
}

type textResult struct {
	Version string `json:"version"`
	ID      string `json:"id"`
	Type    string `json:"type"`
	Output  string `json:"output"`
}

type workspaceResult struct {
	Version       string  `json:"version"`
	ID            string  `json:"id"`
	Type          string  `json:"type"`
	CommitSubject *string `json:"commit_subject,omitempty"`
}

type progressFrame struct {
	Version  string `json:"version"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Sequence int    `json:"sequence"`
	Stage    string `json:"stage"`
	Text     string `json:"text"`
}

type frameHeader struct {
	Version string `json:"version"`
	ID      string `json:"id"`
	Type    string `json:"type"`
}

type failure struct {
	Version  string `json:"version"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Category string `json:"category"`
}

type cancelFrame struct {
	Version string `json:"version"`
	ID      string `json:"id"`
	Type    string `json:"type"`
}

type Handler func(context.Context, Request) (Result, error)

func Exchange(dst io.Writer, src io.Reader, request Request, offered []Capability) (Result, error) {
	return exchangeContext(context.Background(), dst, src, request, offered, func() {})
}

func exchangeContext(ctx context.Context, dst io.Writer, src io.Reader, request Request, offered []Capability, abort func()) (Result, error) {
	if !validID(request.ID) || !validCapabilities(offered) || !contains(offered, request.Operation) || !validRequestPayload(request) {
		return Result{}, errors.New("invalid plugin request")
	}
	if err := writeFrame(dst, initialize{Version, request.ID, "initialize", offered, V1Limits()}); err != nil {
		return Result{}, err
	}
	reader := bufio.NewReaderSize(src, MaxFrameBytes+1)
	var ready initialized
	body, err := readFrameBodySelect(ctx, reader, abort, nil)
	if err != nil || decodeFrame(body, &ready) != nil || ready.Version != Version || ready.ID != request.ID || ready.Type != "initialized" || !selectedCapabilities(ready.Capabilities, offered, request.Operation) {
		return Result{}, errors.New("invalid initialized frame")
	}
	switch request.Operation {
	case Text:
		if err := writeFrame(dst, textExecute{Version, request.ID, "execute", Text, request.Input}); err != nil {
			return Result{}, err
		}
	case WorkspaceEdit:
		if err := writeFrame(dst, workspaceExecute{Version, request.ID, "execute", WorkspaceEdit, request.Workspace, request.Instruction, request.TimeoutMS}); err != nil {
			return Result{}, err
		}
	default:
		return Result{}, errors.New("invalid plugin request")
	}
	progressCount, sequence := 0, 0
	for {
		var onCancel func() bool
		if contains(ready.Capabilities, Cancel) {
			onCancel = func() bool { return writeFrame(dst, cancelFrame{Version, request.ID, "cancel"}) == nil }
		}
		body, err := readFrameBodySelect(ctx, reader, abort, onCancel)
		if err != nil {
			if errors.Is(err, errCancelSent) {
				return Result{}, errCancelSent
			}
			if errors.Is(err, ErrCancelled) {
				return Result{}, ErrCancelled
			}
			return Result{}, errors.New("invalid terminal frame")
		}
		var header frameHeader
		if err := json.Unmarshal(body, &header); err != nil || header.Version != Version || header.ID != request.ID {
			return Result{}, errors.New("invalid terminal frame")
		}
		switch header.Type {
		case "progress":
			var progress progressFrame
			if err := decodeFrame(body, &progress); err != nil || !contains(ready.Capabilities, Progress) || progress.Sequence != sequence+1 || !containsString([]string{"started", "working", "finalizing"}, progress.Stage) || len(progress.Text) > MaxProgressTextBytes || !utf8.ValidString(progress.Text) {
				return Result{}, errors.New("invalid progress frame")
			}
			progressCount++
			sequence = progress.Sequence
			if progressCount > MaxProgressFrames {
				return Result{}, errors.New("too many progress frames")
			}
		case "failure":
			var terminal failure
			if err := decodeFrame(body, &terminal); err != nil || !containsString([]string{"invalid_request", "incompatible", "execution_failed", "cancelled"}, terminal.Category) {
				return Result{}, errors.New("invalid failure frame")
			}
			return Result{}, ErrOperation
		case "result":
			if request.Operation == Text {
				var terminal textResult
				if err := decodeFrame(body, &terminal); err != nil || len(terminal.Output) > MaxTextBytes || !utf8.ValidString(terminal.Output) {
					return Result{}, errors.New("invalid result frame")
				}
				return Result{Output: terminal.Output}, nil
			}
			var terminal workspaceResult
			if err := decodeFrame(body, &terminal); err != nil || ValidateCommitSubject(terminal.CommitSubject, contains(ready.Capabilities, CommitSubject)) != nil {
				return Result{}, errors.New("invalid result frame")
			}
			return Result{CommitSubject: terminal.CommitSubject}, nil
		default:
			return Result{}, errors.New("invalid frame order")
		}
	}
}

func validRequestPayload(request Request) bool {
	switch request.Operation {
	case Text:
		return len(request.Input) <= MaxTextBytes && utf8.ValidString(request.Input)
	case WorkspaceEdit:
		return request.Workspace != "" && len(request.Workspace) <= MaxFrameBytes && request.Instruction != "" && len(request.Instruction) <= MaxTextBytes && request.TimeoutMS > 0 && utf8.ValidString(request.Workspace) && utf8.ValidString(request.Instruction)
	default:
		return false
	}
}

func readFrameBodySelect(ctx context.Context, reader *bufio.Reader, abort func(), onCancel func() bool) ([]byte, error) {
	type response struct {
		body []byte
		err  error
	}
	done := make(chan response, 1)
	go func() {
		body, err := readFrameBody(reader)
		done <- response{body, err}
	}()
	select {
	case result := <-done:
		if ctx.Err() != nil {
			abort()
			return nil, ErrCancelled
		}
		return result.body, result.err
	case <-ctx.Done():
		if onCancel != nil && onCancel() {
			timer := time.NewTimer(drainGrace)
			select {
			case <-done:
				if !timer.Stop() {
					<-timer.C
				}
				return nil, errCancelSent
			case <-timer.C:
			}
		}
		abort()
		<-done
		if onCancel != nil {
			return nil, errCancelSent
		}
		return nil, ErrCancelled
	}
}

func ReadFailure(src io.Reader) (string, error) {
	var frame failure
	if err := readFrame(src, &frame); err != nil {
		return "", err
	}
	if frame.Version != Version || !validID(frame.ID) || frame.Type != "failure" || !containsString([]string{"invalid_request", "incompatible", "execution_failed", "cancelled"}, frame.Category) {
		return "", errors.New("invalid failure frame")
	}
	return frame.Category, nil
}

func Serve(in io.Reader, out io.Writer, supported []Capability, handler Handler) error {
	if !validCapabilities(supported) || len(supported) == 0 || contains(supported, Cancel) || contains(supported, Progress) || handler == nil {
		return errors.New("invalid plugin configuration")
	}
	reader := bufio.NewReaderSize(in, MaxFrameBytes+1)
	var init initialize
	if err := readFrame(reader, &init); err != nil || init.Version != Version || init.Type != "initialize" || !validID(init.ID) || !validCapabilities(init.Capabilities) || init.Limits != V1Limits() {
		return errors.New("invalid initialize frame")
	}
	selected := make([]Capability, 0, len(supported))
	for _, capability := range supported {
		if contains(init.Capabilities, capability) {
			selected = append(selected, capability)
		}
	}
	if err := writeFrame(out, initialized{Version, init.ID, "initialized", selected}); err != nil {
		return err
	}
	body, err := readFrameBody(reader)
	if err != nil {
		return err
	}
	var header frameHeader
	if err := json.Unmarshal(body, &header); err != nil || header.Version != Version || header.ID != init.ID || header.Type != "execute" {
		return errors.New("invalid execute frame")
	}
	request := Request{ID: init.ID}
	var operation struct {
		Operation Capability `json:"operation"`
	}
	if err := json.Unmarshal(body, &operation); err != nil || !contains(selected, operation.Operation) {
		return errors.New("invalid execute operation")
	}
	switch operation.Operation {
	case Text:
		var execute textExecute
		if err := decodeFrame(body, &execute); err != nil || len(execute.Input) > MaxTextBytes || !utf8.ValidString(execute.Input) {
			return errors.New("invalid execute frame")
		}
		request.Operation, request.Input = Text, execute.Input
	case WorkspaceEdit:
		var execute workspaceExecute
		if err := decodeFrame(body, &execute); err != nil || execute.Workspace == "" || execute.Instruction == "" || execute.TimeoutMS <= 0 || len(execute.Instruction) > MaxTextBytes || !utf8.ValidString(execute.Workspace+execute.Instruction) {
			return errors.New("invalid execute frame")
		}
		request.Operation, request.Workspace, request.Instruction, request.TimeoutMS = WorkspaceEdit, execute.Workspace, execute.Instruction, execute.TimeoutMS
	default:
		return errors.New("invalid execute operation")
	}
	result, handlerErr := handler(context.Background(), request)
	if handlerErr != nil {
		return writeFrame(out, failure{Version, init.ID, "failure", "execution_failed"})
	}
	if request.Operation == Text {
		if len(result.Output) > MaxTextBytes || !utf8.ValidString(result.Output) {
			return writeFrame(out, failure{Version, init.ID, "failure", "execution_failed"})
		}
		return writeFrame(out, textResult{Version, init.ID, "result", result.Output})
	}
	if err := ValidateCommitSubject(result.CommitSubject, contains(selected, CommitSubject)); err != nil {
		return writeFrame(out, failure{Version, init.ID, "failure", "execution_failed"})
	}
	return writeFrame(out, workspaceResult{Version, init.ID, "result", result.CommitSubject})
}

func ValidateCommitSubject(subject *string, negotiated bool) error {
	if subject == nil {
		return nil
	}
	value := *subject
	if !negotiated || value == "" || len(value) > MaxCommitSubjectBytes || !utf8.ValidString(value) || strings.TrimFunc(value, unicode.IsSpace) != value {
		return errors.New("invalid commit subject")
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return errors.New("invalid commit subject")
		}
	}
	return nil
}

func writeFrame(w io.Writer, value any) error {
	body, err := json.Marshal(value)
	if err != nil || len(body) > MaxFrameBytes {
		return errors.New("invalid frame")
	}
	body = append(body, '\n')
	_, err = w.Write(body)
	return err
}

func readFrame(r io.Reader, dst any) error {
	reader, ok := r.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReaderSize(r, MaxFrameBytes+1)
	}
	body, err := readFrameBody(reader)
	if err != nil {
		return err
	}
	return decodeFrame(body, dst)
}

func readFrameBody(reader *bufio.Reader) ([]byte, error) {
	body, err := reader.ReadSlice('\n')
	if err != nil || len(body) == 1 || len(body)-1 > MaxFrameBytes || !utf8.Valid(body) || bytes.IndexByte(body[:len(body)-1], '\n') >= 0 {
		return nil, errors.New("invalid frame")
	}
	body = body[:len(body)-1]
	var compact bytes.Buffer
	if body[len(body)-1] == '\r' || json.Compact(&compact, body) != nil || !bytes.Equal(compact.Bytes(), body) || hasDuplicateJSONField(body) {
		return nil, errors.New("invalid frame")
	}
	return body, nil
}

func decodeFrame(body []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("invalid frame")
	}
	return nil
}

func hasDuplicateJSONField(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate field")
				}
				seen[name] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		}
		_, err = decoder.Token()
		return err
	}
	return walk() != nil
}

func validID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == id
}

func validCapabilities(capabilities []Capability) bool {
	seen := map[Capability]bool{}
	for _, capability := range capabilities {
		if !contains([]Capability{Text, WorkspaceEdit, Progress, Cancel, CommitSubject}, capability) || seen[capability] {
			return false
		}
		seen[capability] = true
	}
	return true
}

func selectedCapabilities(selected, offered []Capability, required Capability) bool {
	return validCapabilities(selected) && contains(selected, required) && every(selected, func(capability Capability) bool { return contains(offered, capability) })
}

func contains(values []Capability, value Capability) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func every(values []Capability, predicate func(Capability) bool) bool {
	for _, value := range values {
		if !predicate(value) {
			return false
		}
	}
	return true
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
