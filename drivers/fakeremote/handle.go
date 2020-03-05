package fakeremote

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/lib/fifo"
	"github.com/hashicorp/nomad/plugins/drivers"
)

type taskHandle struct {
	pid    int
	logger hclog.Logger

	// stateLock syncs access to all fields below
	stateLock sync.RWMutex

	taskConfig  *drivers.TaskConfig
	procState   drivers.TaskState
	startedAt   time.Time
	completedAt time.Time
	exitResult  *drivers.ExitResult
	doneCh      chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
}

func newTaskHandle(logger hclog.Logger, ts TaskState, taskConfig *drivers.TaskConfig) *taskHandle {
	ctx, cancel := context.WithCancel(context.Background())
	logger = logger.Named("handle").With("pid", ts.PID)

	h := &taskHandle{
		pid: ts.PID,
		//FIXME(schmichael) this originally used a TaskConfig persisted
		//in the TaskState which broke logging as it pointed to the old
		//logmon path. can we remove the TaskConfig from the driver
		//state or does that break agent restart?!
		taskConfig: taskConfig,
		procState:  drivers.TaskStateRunning,
		startedAt:  ts.StartedAt,
		exitResult: &drivers.ExitResult{},
		logger:     logger,
		doneCh:     make(chan struct{}),
		ctx:        ctx,
		cancel:     cancel,
	}

	return h
}

func (h *taskHandle) TaskStatus() *drivers.TaskStatus {
	h.stateLock.RLock()
	defer h.stateLock.RUnlock()

	return &drivers.TaskStatus{
		ID:          h.taskConfig.ID,
		Name:        h.taskConfig.Name,
		State:       h.procState,
		StartedAt:   h.startedAt,
		CompletedAt: h.completedAt,
		ExitResult:  h.exitResult,
		DriverAttributes: map[string]string{
			"pid": strconv.Itoa(h.pid),
		},
	}
}

func (h *taskHandle) IsRunning() bool {
	h.stateLock.RLock()
	defer h.stateLock.RUnlock()
	return h.procState == drivers.TaskStateRunning
}

func (h *taskHandle) run() {
	defer close(h.doneCh)
	h.stateLock.Lock()
	if h.exitResult == nil {
		h.exitResult = &drivers.ExitResult{}
	}
	h.stateLock.Unlock()

	h.logger.Info("-----> OpenWriter()", "stdout_path", h.taskConfig.StdoutPath)
	f, err := fifo.OpenWriter(h.taskConfig.StdoutPath)
	if err != nil {
		h.logger.Info("-----> OpenWriter() ERROR 1", "error", err, "stdout_path", h.taskConfig.StdoutPath)
		h.stateLock.Lock()
		defer h.stateLock.Unlock()
		h.completedAt = time.Now()
		h.exitResult.ExitCode = 1
		h.exitResult.Err = fmt.Errorf("failed to create stdout: %v", err)
		return
	}
	defer f.Close()

	// Block until stopped.
	for h.ctx.Err() == nil {

		// External providers do not support blocking queries or pub/sub style
		// feedback on resources. This block mimics what is likely to be
		// required in these situations were we poll for the health of the
		// remote resource and log relevant information.
		select {
		case <-time.After(5 * time.Second):

			_, err := os.FindProcess(h.pid)
			if err != nil {
				h.logger.Info("-----> failed to find process", "error", err, "pid", h.pid)
				h.stateLock.Lock()
				h.completedAt = time.Now()
				h.exitResult.ExitCode = 2
				h.exitResult.Err = fmt.Errorf("failed to find process: %v", err)
				h.stateLock.Unlock()
				return
			}

			now := time.Now().Format(time.RFC3339)
			if _, err := fmt.Fprintf(f, "[%s] - client is remotely monitoring pid:%v\n", now, h.pid); err != nil {
				h.logger.Info("-----> OpenWriter() ERROR 2", "error", err, "stdout_path", h.taskConfig.StdoutPath)
				h.stateLock.Lock()
				h.completedAt = time.Now()
				h.exitResult.ExitCode = 2
				h.exitResult.Err = fmt.Errorf("failed to write to stdout: %v", err)
			}

		case <-h.ctx.Done():
		}
	}

	h.logger.Info("-----> handle.run DONE", "ctx_error", h.ctx.Err(), "stdout_path", h.taskConfig.StdoutPath)

	h.stateLock.Lock()
	defer h.stateLock.Unlock()

	h.procState = drivers.TaskStateExited
	h.exitResult.ExitCode = 0
	h.exitResult.Signal = 0
	h.completedAt = time.Now()
}

func (h *taskHandle) stop() {
	h.logger.Info("handle.stop()")
	h.cancel()
}