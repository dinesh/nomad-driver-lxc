package lxc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/appscode/go/wait"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/lib/fifo"
	"github.com/hashicorp/nomad/client/stats"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/pkg/errors"
	lxc "gopkg.in/lxc/go-lxc.v2"
)

type ContainerInfo struct {
	container *lxc.Container
	initPid   int
	command   []string
}

type taskHandle struct {
	ContainerInfo

	logger hclog.Logger
	driver *Driver

	totalCpuStats  *stats.CpuStats
	userCpuStats   *stats.CpuStats
	systemCpuStats *stats.CpuStats

	// stateLock syncs access to all fields below
	stateLock sync.RWMutex
	doneCh    chan bool

	taskConfig  *drivers.TaskConfig
	procState   drivers.TaskState
	startedAt   time.Time
	completedAt time.Time
	exitResult  *drivers.ExitResult
}

var (
	LXCMeasuredCpuStats = []string{"System Mode", "User Mode", "Percent"}

	LXCMeasuredMemStats = []string{"RSS", "Cache", "Swap", "Max Usage", "Kernel Usage", "Kernel Max Usage"}

	AttachmentMode = true
)

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
			"pid": strconv.Itoa(h.initPid),
		},
	}
}

func (h *taskHandle) IsRunning() bool {
	h.stateLock.RLock()
	defer h.stateLock.RUnlock()
	return h.procState == drivers.TaskStateRunning
}

func (h *taskHandle) waitForCommand() (int, error) {
	if len(h.command) == 0 {
		h.logger.Debug("No command/args found, will wait for lxc-init process")
		h.waitTillStopped()
		return 0, nil
	}

	stopInitProcess := func() {
		h.logger.Info("stopping container")
		if serr := h.container.Stop(); serr != nil {
			h.logger.Error("stopping lxc-init", "err", serr)
		}
	}
	defer stopInitProcess()

	options := lxc.DefaultAttachOptions
	h.logger.Debug("setting attach options",
		"command", h.command,
	)

	stdout, err := fifo.OpenWriter(h.taskConfig.StdoutPath)
	if err != nil {
		return 0, errors.Wrap(err, "fifo.OpenWriter stdout")
	}
	stderr, err := fifo.OpenWriter(h.taskConfig.StderrPath)
	if err != nil {
		return 0, errors.Wrap(err, "fifo.OpenWriter stderr")
	}

	outr, outw, _ := os.Pipe()
	errr, errw, _ := os.Pipe()

	go asyncCopy(stdout, outr)
	go asyncCopy(stderr, errr)

	options.StdoutFd = outw.Fd()
	options.StderrFd = errw.Fd()
	options.Env = h.taskConfig.EnvList()

	h.logger.Info("Attaching to container", "command", h.command)

	now := time.Now()
	exitCode, err := h.container.RunCommandStatus(h.command, options)

	{
		outr.Close()
		errr.Close()
		stdout.Close()
		stderr.Close()
	}

	h.logger.Debug(fmt.Sprintf("Command execution with %d in %s",
		exitCode, time.Now().Sub(now).String()))

	return exitCode, err
}

func (h *taskHandle) waitForNetwork() {
	backoff := wait.Backoff{
		Steps:    10,
		Duration: 500 * time.Millisecond,
		Factor:   1,
	}
	now := time.Now()

	var ips []string
	if err := wait.ExponentialBackoff(backoff, func() (done bool, err error) {
		ips, err = h.container.IPAddresses()
		done = len(ips) > 0
		err = nil
		return
	}); err != nil {
		h.logger.Warn(fmt.Sprintf("timed-out %v", err))
		return
	}

	h.logger.Debug(fmt.Sprintf("Allocated IPAddress(s) %v in %s", ips, time.Now().Sub(now).String()))
}

func (h *taskHandle) waitForInit() error {
	var (
		timeout = time.After(10 * time.Second)
		tick    = time.Tick(500 * time.Millisecond)
	)

	if !h.container.Running() {
		h.logger.Warn("Container not running", "state", h.container.State())
		return nil
	}

	for {
		select {
		case <-h.driver.ctx.Done():
			h.logger.Info("Main Context cancelled, Existing ...")
			return nil
		case <-timeout:
			h.logger.Info("timed out, Exiting loop", "timeout", timeout)
			return fmt.Errorf("timed out for condition")
		case <-tick:
			if h.container.Running() {
				return nil
			}
		}
	}
}

func (h *taskHandle) runNext() {
	h.stateLock.Lock()
	if h.exitResult == nil {
		h.exitResult = &drivers.ExitResult{}
	}
	h.stateLock.Unlock()
	var err error
	var exitCode int

	if AttachmentMode {
		err = h.waitForInit()
		if err == nil {
			if exitCode, err = h.waitForCommand(); err != nil {
				h.logger.Error("Command failed", "err", err)
			}
		} else {
			h.logger.Error("LXC init failed", "err", err)
		}
	}

	// Shutdown stats collection
	close(h.doneCh)

	// Wait till container stops
	h.waitTillStopped()

	h.stateLock.Lock()

	if err != nil {
		h.exitResult.Err = err
		h.exitResult.ExitCode = 1
	} else {
		h.exitResult.Signal = 0
		h.exitResult.ExitCode = exitCode
	}
	h.completedAt = time.Now()

	h.procState = drivers.TaskStateExited
	h.stateLock.Unlock()
}

func (h *taskHandle) waitTillStopped() {
	if ok, err := waitTillStopped(h.container); !ok {
		h.logger.Error("failed to find container process", "error", err)
		return
	}
}

// shutdown shuts down the container, with `timeout` grace period
// before killing the container with SIGKILL.
func (h *taskHandle) shutdown(timeout time.Duration) error {
	err := h.container.Shutdown(timeout)
	if err == nil || strings.Contains(err.Error(), "not running") {
		return nil
	}

	err = h.container.Stop()
	if err == nil || strings.Contains(err.Error(), "not running") {
		return nil
	}
	return err
}
