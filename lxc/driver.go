package lxc

import (
	"context"
	"fmt"
	"strings"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/stats"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	"github.com/tevino/abool"
	lxc "gopkg.in/lxc/go-lxc.v2"
)

const (
	// pluginName is the name of the plugin
	pluginName = "lxc"

	// fingerprintPeriod is the interval at which the driver will send fingerprint responses
	fingerprintPeriod = 30 * time.Second

	// taskHandleVersion is the version of task handle which this driver sets
	// and understands how to decode driver state
	taskHandleVersion = 1

	// ipAddressInterval is the interval to wait for a container to get IP4 Address
	ipAddressInterval = 10 * time.Second

	danglingContainersCreationGraceMinimum = 1 * time.Minute

	providerEnvVar = "PROVIDER=lxc-driver"
)

var (
	// pluginInfo is the response returned for the PluginInfo RPC
	pluginInfo = &base.PluginInfoResponse{
		Type:              base.PluginTypeDriver,
		PluginApiVersions: []string{drivers.ApiVersion010},
		PluginVersion:     "0.1.1-dev",
		Name:              pluginName,
	}

	// capabilities is returned by the Capabilities RPC and indicates what
	// optional features this driver supports
	capabilities = &drivers.Capabilities{
		SendSignals: true,
		Exec:        false,
		FSIsolation: drivers.FSIsolationImage,
	}
)

// Driver is a driver for running LXC containers
type Driver struct {
	// eventer is used to handle multiplexing of TaskEvents calls such that an
	// event can be broadcast to all callers
	eventer *eventer.Eventer

	// config is the driver configuration set by the SetConfig RPC
	config *Config

	// nomadConfig is the client config from nomad
	nomadConfig *base.ClientDriverConfig

	// tasks is the in memory datastore mapping taskIDs to rawExecDriverHandles
	tasks *taskStore

	// ctx is the context for the driver. It is passed to other subsystems to
	// coordinate shutdown
	ctx context.Context

	// signalShutdown is called when the driver is shutting down and cancels the
	// ctx passed to any subsystems
	signalShutdown context.CancelFunc

	// logger will log to the Nomad agent
	logger hclog.Logger

	reconciler *containerReconciler

	// A tri-state boolean to know if the fingerprinting has happened and
	// whether it has been successful
	fingerprint *abool.AtomicBool
}

// ContainerGCConfig controls the behavior of the GC reconciler to detects
// dangling nomad containers that aren't tracked due to lxc/nomad bugs
type ContainerGCConfig struct {
	// Enabled controls whether container reconciler is enabled
	Enabled bool `codec:"enabled"`

	// DryRun indicates that reconciler should log unexpectedly running containers
	// if found without actually killing them
	DryRun bool `codec:"dry_run"`

	// PeriodStr controls the frequency of scanning containers
	PeriodStr string        `codec:"period"`
	period    time.Duration `codec:"-"`

	// CreationGraceStr is the duration allowed for a newly created container
	// to live without being registered as a running task in nomad.
	// A container is treated as leaked if it lived more than grace duration
	// and haven't been registered in tasks.
	CreationGraceStr string        `codec:"creation_grace"`
	CreationGrace    time.Duration `codec:"-"`
}

// GCConfig is the driver GarbageCollection configuration
type GCConfig struct {
	Container          bool              `codec:"container"`
	DanglingContainers ContainerGCConfig `codec:"dangling_containers"`
}

// Config is the driver configuration set by the SetConfig RPC call
type Config struct {
	// Enabled is set to true to enable the lxc driver
	Enabled bool `codec:"enabled"`

	AllowVolumes bool `codec:"volumes_enabled"`

	LXCPath string `codec:"lxc_path"`

	// default networking mode if not specified in task config
	NetworkMode string `codec:"network_mode"`

	GC GCConfig `codec:"gc"`
}

// TaskConfig is the driver configuration of a task within a job
type TaskConfig struct {
	Template             string   `codec:"template"`
	Distro               string   `codec:"distro"`
	Release              string   `codec:"release"`
	Arch                 string   `codec:"arch"`
	ImageVariant         string   `codec:"image_variant"`
	ImageServer          string   `codec:"image_server"`
	GPGKeyID             string   `codec:"gpg_key_id"`
	GPGKeyServer         string   `codec:"gpg_key_server"`
	DisableGPGValidation bool     `codec:"disable_gpg"`
	FlushCache           bool     `codec:"flush_cache"`
	ForceCache           bool     `codec:"force_cache"`
	TemplateArgs         []string `codec:"template_args"`
	LogLevel             string   `codec:"log_level"`
	Verbosity            string   `codec:"verbosity"`
	Volumes              []string `codec:"volumes"`
	NetworkMode          string   `codec:"network_mode"`
	Parameters           []string `codec:"parameters"`
	Command              string   `codec:"command"`
	Args                 []string `codec:"args"`
}

// TaskState is the state which is encoded in the handle returned in
// StartTask. This information is needed to rebuild the task state and handler
// during recovery.
type TaskState struct {
	TaskConfig    *drivers.TaskConfig
	ContainerName string
	StartedAt     time.Time
}

// NewLXCDriver returns a new DriverPlugin implementation
func NewLXCDriver(logger hclog.Logger) drivers.DriverPlugin {
	ctx, cancel := context.WithCancel(context.Background())
	logger = logger.Named(pluginName)
	return &Driver{
		eventer:        eventer.NewEventer(ctx, logger),
		config:         &Config{},
		tasks:          newTaskStore(),
		ctx:            ctx,
		fingerprint:    abool.New(),
		signalShutdown: cancel,
		logger:         logger,
	}
}

func (d *Driver) PluginInfo() (*base.PluginInfoResponse, error) {
	return pluginInfo, nil
}

func (d *Driver) ConfigSchema() (*hclspec.Spec, error) {
	return configSpec, nil
}

func (d *Driver) SetConfig(cfg *base.Config) error {
	var config Config
	if len(cfg.PluginConfig) != 0 {
		if err := base.MsgPackDecode(cfg.PluginConfig, &config); err != nil {
			return err
		}
	}

	if len(config.GC.DanglingContainers.PeriodStr) > 0 {
		dur, err := time.ParseDuration(config.GC.DanglingContainers.PeriodStr)
		if err != nil {
			return fmt.Errorf("failed to parse 'period' duration: %v", err)
		}
		config.GC.DanglingContainers.period = dur
	}

	if len(config.GC.DanglingContainers.CreationGraceStr) > 0 {
		dur, err := time.ParseDuration(config.GC.DanglingContainers.CreationGraceStr)
		if err != nil {
			return fmt.Errorf("failed to parse 'creation_grace' duration: %v", err)
		}
		if dur < danglingContainersCreationGraceMinimum {
			return fmt.Errorf("creation_grace is less than minimum, %v", danglingContainersCreationGraceMinimum)
		}
		config.GC.DanglingContainers.CreationGrace = dur
	}

	d.config = &config
	if cfg.AgentConfig != nil {
		d.nomadConfig = cfg.AgentConfig.Driver
	}

	d.reconciler = newReconciler(d)

	return nil
}

func (d *Driver) Shutdown(ctx context.Context) error {
	d.signalShutdown()
	return nil
}

func (d *Driver) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

func (d *Driver) Capabilities() (*drivers.Capabilities, error) {
	return capabilities, nil
}

func (d *Driver) RecoverTask(handle *drivers.TaskHandle) error {
	if handle == nil {
		return fmt.Errorf("error: handle cannot be nil")
	}

	// COMPAT(0.10): pre 0.9 upgrade path check
	if handle.Version == 0 {
		return d.recoverPre09Task(handle)
	}

	if _, ok := d.tasks.Get(handle.Config.ID); ok {
		return nil
	}

	var taskState TaskState
	if err := handle.GetDriverState(&taskState); err != nil {
		return fmt.Errorf("failed to decode task state from handle: %v", err)
	}

	c, err := lxc.NewContainer(taskState.ContainerName, d.lxcPath())
	if err != nil {
		return fmt.Errorf("failed to create co	ntainer ref: %v", err)
	}

	initPid := c.InitPid()
	logger := d.logger.With("container", c.Name())

	h := &taskHandle{
		ContainerInfo: ContainerInfo{
			container: c,
			initPid:   initPid,
		},
		driver:     d,
		taskConfig: taskState.TaskConfig,
		procState:  drivers.TaskStateRunning,
		startedAt:  taskState.StartedAt,
		exitResult: &drivers.ExitResult{},
		logger:     logger,
		doneCh:     make(chan bool),

		totalCpuStats:  stats.NewCpuStats(),
		userCpuStats:   stats.NewCpuStats(),
		systemCpuStats: stats.NewCpuStats(),
	}

	d.tasks.Set(taskState.TaskConfig.ID, h)

	h.logger.Info("Recovered task", "taskID", taskState.TaskConfig.ID)
	go h.runNext()
	return nil
}

func (d *Driver) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	if _, ok := d.tasks.Get(cfg.ID); ok {
		return nil, nil, fmt.Errorf("task with ID %q already started", cfg.ID)
	}

	var driverConfig TaskConfig
	if err := cfg.DecodeDriverConfig(&driverConfig); err != nil {
		return nil, nil, fmt.Errorf("failed to decode driver config: %v", err)
	}

	if driverConfig.Command == "" && len(driverConfig.Args) > 0 {
		return nil, nil, fmt.Errorf(
			"`config.command` must be set with args(%d provided)",
			len(driverConfig.Args),
		)
	}

	d.logger.Info("starting lxc task", "driver_cfg", hclog.Fmt("%+v", driverConfig))
	handle := drivers.NewTaskHandle(taskHandleVersion)
	handle.Config = cfg

	c, err := d.initializeContainer(cfg, driverConfig)
	if err != nil {
		return nil, nil, err
	}

	for _, line := range driverConfig.Parameters {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) < 2 {
			d.logger.Warn("Incorrect parameter. (missing `=` char)", line)
			continue
		}
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if err := c.SetConfigItem(key, value); err != nil {
			d.logger.Debug(fmt.Sprintf("setting config item %s=%s", key, value))
			return nil, nil, fmt.Errorf("unable to apply config item(%s=%s): %v", key, value, err)
		}
	}

	opt := toLXCCreateOptions(driverConfig)
	if err := c.Create(opt); err != nil {
		return nil, nil, fmt.Errorf("unable to create container: %v", err)
	}

	cleanup := func() {
		if err := c.Destroy(); err != nil {
			d.logger.Error("failed to clean up from an error in Start", "error", err)
		}
	}

	if err := d.configureContainerNetwork(c, driverConfig); err != nil {
		cleanup()
		return nil, nil, err
	}

	if err := d.mountVolumes(c, cfg, driverConfig); err != nil {
		cleanup()
		return nil, nil, err
	}

	var command []string
	if driverConfig.Command != "" {
		command = append(command, driverConfig.Command)
		command = append(command, driverConfig.Args...)
	}

	c.SetConfigItem("lxc.environment", providerEnvVar)

	if AttachmentMode {
		if err := c.Start(); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("unable to start container: %v", err)
		}
	} else {
		if err := c.StartExecute(command); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("unable to start container: %v", err)
		}
	}

	if err := d.setResourceLimits(c, cfg); err != nil {
		cleanup()
		return nil, nil, err
	}

	var (
		pid         = c.InitPid()
		containerID = c.Name()
	)

	d.logger = d.logger.With("container", containerID)
	h := &taskHandle{
		ContainerInfo: ContainerInfo{
			initPid:   pid,
			container: c,
			command:   command,
		},
		driver:     d,
		taskConfig: cfg,
		procState:  drivers.TaskStateRunning,
		startedAt:  time.Now().Round(time.Millisecond),
		logger:     d.logger,
		doneCh:     make(chan bool),

		totalCpuStats:  stats.NewCpuStats(),
		userCpuStats:   stats.NewCpuStats(),
		systemCpuStats: stats.NewCpuStats(),
	}

	driverState := TaskState{
		ContainerName: containerID,
		TaskConfig:    cfg,
		StartedAt:     h.startedAt,
	}

	h.waitForNetwork()

	if err := handle.SetDriverState(&driverState); err != nil {
		d.logger.Error("failed to start task, error setting driver state", "err", err)
		cleanup()
		return nil, nil, fmt.Errorf("failed to set driver state: %v", err)
	}

	d.tasks.Set(cfg.ID, h)

	go h.runNext()

	d.logger.Info("Completely started task", "taskID", cfg.ID)
	return handle, nil, nil
}

func (d *Driver) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	ch := make(chan *drivers.ExitResult)
	go d.handleWait(ctx, handle, ch)

	return ch, nil
}

func (d *Driver) handleWait(ctx context.Context, handle *taskHandle, ch chan *drivers.ExitResult) {
	defer close(ch)

	//
	// Wait for process completion by polling status from handler.
	// We cannot use the following alternatives:
	//   * Process.Wait() requires LXC container processes to be children
	//     of self process; but LXC runs container in separate PID hierarchy
	//     owned by PID 1.
	//   * lxc.Container.Wait() holds a write lock on container and prevents
	//     any other calls, including stats.
	//
	// Going with simplest approach of polling for handler to mark exit.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			s := handle.TaskStatus()
			if s.State == drivers.TaskStateExited {
				ch <- handle.exitResult
			}
		}
	}
}

func (d *Driver) StopTask(taskID string, timeout time.Duration, signal string) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}
	handle.logger.Info("Got StopTask hook", "signal", signal, "timeout", timeout)
	if err := handle.shutdown(timeout); err != nil {
		return fmt.Errorf("executor Shutdown failed: %v", err)
	}

	return nil
}

func (d *Driver) DestroyTask(taskID string, force bool) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	if handle.IsRunning() && !force {
		return fmt.Errorf("cannot destroy running task")
	}

	if handle.IsRunning() {
		// grace period is chosen arbitrary here
		if err := handle.shutdown(1 * time.Minute); err != nil {
			handle.logger.Error("failed to destroy executor", "err", err)
		}
	}
	if d.config.GC.Container {
		handle.logger.Debug("Destroying container", "container", handle.container.Name())
		// delete the container itself
		if err := handle.container.Destroy(); err != nil {
			handle.logger.Error("failed to destroy lxc container", "err", err)
		}
	}
	// finally cleanup task map
	d.tasks.Delete(taskID)
	return nil
}

func (d *Driver) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return handle.TaskStatus(), nil
}

func (d *Driver) TaskStats(ctx context.Context, taskID string, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return handle.stats(ctx, interval)
}

func (d *Driver) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	return d.eventer.TaskEvents(ctx)
}

func (d *Driver) SignalTask(taskID string, signal string) error {
	return fmt.Errorf("LXC driver does not support signal")
}

func (d *Driver) ExecTask(taskID string, cmd []string, timeout time.Duration) (*drivers.ExecTaskResult, error) {
	return nil, fmt.Errorf("LXC driver does not support exec")
}
