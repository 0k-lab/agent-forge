package pluginprotocol

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agent-forge/internal/processtree"
)

const drainGrace = 250 * time.Millisecond

var ErrStart = errors.New("plugin start failed")

type Options struct {
	Timeout      time.Duration
	OutputBytes  int64
	Capabilities []Capability
	Environment  []string
}

type exchangeResponse struct {
	result Result
	err    error
}

func Run(parent context.Context, argv []string, request Request, options Options) (Result, error) {
	if parent.Err() != nil {
		return Result{}, ErrCancelled
	}
	if len(argv) == 0 || len(argv) > 64 || argv[0] == "" || options.Timeout <= 0 || options.OutputBytes <= 0 || !validRequestPayload(request) {
		return Result{}, ErrStart
	}
	for _, argument := range argv {
		if argument == "" || len(argument) > 4096 || !utf8.ValidString(argument) || strings.IndexByte(argument, 0) >= 0 {
			return Result{}, ErrStart
		}
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Result{}, ErrStart
	}
	request.ID = hex.EncodeToString(random[:])
	offered := append([]Capability{request.Operation}, options.Capabilities...)
	if !validCapabilities(offered) {
		return Result{}, ErrStart
	}
	ctx, cancel := context.WithTimeout(parent, options.Timeout)
	defer cancel()
	if ctx.Err() != nil {
		return Result{}, ErrCancelled
	}
	processCtx, stopProcess := context.WithCancel(context.Background())
	defer stopProcess()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = options.Environment
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, ErrStart
	}
	budget := &streamBudget{remaining: options.OutputBytes, exceeded: make(chan struct{})}
	stdoutReader, stdoutWriter := io.Pipe()
	cmd.Stdout = &budgetWriter{writer: stdoutWriter, budget: budget}
	cmd.Stderr = &budgetWriter{writer: io.Discard, budget: budget}
	reader := bufio.NewReaderSize(stdoutReader, MaxFrameBytes+1)
	processDone := make(chan error, 1)
	go func() {
		processDone <- processtree.RunInvocation(processCtx, cmd)
		_ = stdoutWriter.Close()
	}()
	exchangeDone := make(chan exchangeResponse, 1)
	go func() {
		result, err := exchangeContext(ctx, stdin, reader, request, offered, stopProcess)
		exchangeDone <- exchangeResponse{result, err}
	}()
	var result Result
	var exchangeErr error
	select {
	case response := <-exchangeDone:
		result, exchangeErr = response.result, response.err
	case <-budget.exceeded:
		if ctx.Err() != nil {
			abortRun(stopProcess, stdin, stdoutReader, processDone, exchangeDone)
			return Result{}, ErrCancelled
		}
		abortRun(stopProcess, stdin, stdoutReader, processDone, exchangeDone)
		return Result{}, errors.New("plugin protocol failed")
	case <-ctx.Done():
		timer := time.NewTimer(drainGrace)
		select {
		case response := <-exchangeDone:
			timer.Stop()
			result, exchangeErr = response.result, response.err
		case <-budget.exceeded:
			timer.Stop()
			abortRun(stopProcess, stdin, stdoutReader, processDone, exchangeDone)
			return Result{}, ErrCancelled
		case <-timer.C:
			abortRun(stopProcess, stdin, stdoutReader, processDone, exchangeDone)
			return Result{}, ErrCancelled
		}
		abortRun(stopProcess, stdin, stdoutReader, processDone, nil)
		return Result{}, ErrCancelled
	}
	_ = stdin.Close()
	if exchangeErr != nil && !errors.Is(exchangeErr, ErrOperation) {
		processErr := abortRun(stopProcess, stdin, stdoutReader, processDone, nil)
		if ctx.Err() != nil {
			return Result{}, ErrCancelled
		}
		if errors.Is(processErr, processtree.ErrStart) {
			return Result{}, ErrStart
		}
		if errors.Is(exchangeErr, errCancelSent) || errors.Is(exchangeErr, ErrCancelled) {
			return Result{}, ErrCancelled
		}
		return Result{}, errors.New("plugin protocol failed")
	}
	tailDone := make(chan bool, 1)
	go func() {
		_, err := reader.ReadByte()
		tailDone <- err == io.EOF
	}()
	timer := time.NewTimer(drainGrace)
	defer timer.Stop()
	var processErr error
	processFinished, cleanEOF := false, false
	for !processFinished || !cleanEOF {
		select {
		case processErr = <-processDone:
			processFinished = true
			processDone = nil
			if processErr != nil {
				abortRun(stopProcess, stdin, stdoutReader, nil, nil)
				if ctx.Err() != nil {
					return Result{}, ErrCancelled
				}
				return Result{}, errors.New("plugin protocol failed")
			}
		case clean := <-tailDone:
			if !clean {
				abortRun(stopProcess, stdin, stdoutReader, processDone, nil)
				if ctx.Err() != nil {
					return Result{}, ErrCancelled
				}
				return Result{}, errors.New("plugin protocol failed")
			}
			cleanEOF = true
			tailDone = nil
		case <-budget.exceeded:
			if ctx.Err() != nil {
				abortRun(stopProcess, stdin, stdoutReader, processDone, nil)
				return Result{}, ErrCancelled
			}
			abortRun(stopProcess, stdin, stdoutReader, processDone, nil)
			return Result{}, errors.New("plugin protocol failed")
		case <-ctx.Done():
			abortRun(stopProcess, stdin, stdoutReader, processDone, nil)
			return Result{}, ErrCancelled
		case <-timer.C:
			abortRun(stopProcess, stdin, stdoutReader, processDone, nil)
			if ctx.Err() != nil {
				return Result{}, ErrCancelled
			}
			return Result{}, errors.New("plugin protocol failed")
		}
	}
	if ctx.Err() != nil {
		return Result{}, ErrCancelled
	}
	if budget.exceededLimit() {
		return Result{}, errors.New("plugin protocol failed")
	}
	if errors.Is(exchangeErr, ErrOperation) {
		return Result{}, ErrOperation
	}
	return result, nil
}

func abortRun(stopProcess context.CancelFunc, stdin io.Closer, stdout io.Closer, processDone <-chan error, exchangeDone <-chan exchangeResponse) error {
	stopProcess()
	_ = stdin.Close()
	_ = stdout.Close()
	timer := time.NewTimer(3 * drainGrace)
	defer timer.Stop()
	var processErr error
	if processDone != nil {
		select {
		case processErr = <-processDone:
		case <-timer.C:
			return processErr
		}
	}
	if exchangeDone != nil {
		select {
		case <-exchangeDone:
		case <-timer.C:
		}
	}
	return processErr
}

type streamBudget struct {
	mu        sync.Mutex
	remaining int64
	over      bool
	exceeded  chan struct{}
}

func (b *streamBudget) use(n int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	allowed := min(int64(n), max(b.remaining, 0))
	if int64(n) > b.remaining {
		b.remaining = -1
	} else {
		b.remaining -= int64(n)
	}
	if b.remaining < 0 && !b.over {
		b.over = true
		close(b.exceeded)
	}
	return int(allowed)
}

func (b *streamBudget) exceededLimit() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.over
}

type budgetWriter struct {
	writer io.Writer
	budget *streamBudget
}

func (w *budgetWriter) Write(p []byte) (int, error) {
	allowed := w.budget.use(len(p))
	if allowed > 0 {
		if _, err := w.writer.Write(p[:allowed]); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}
