package lxc

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	execute "github.com/alexellis/go-execute/pkg/v1"
	"github.com/hashicorp/go-hclog"
	lxc "gopkg.in/lxc/go-lxc.v2"
)

func newReconciler(d *Driver) *containerReconciler {
	return &containerReconciler{
		driver: d,
		config: d.config.GC.DanglingContainers,
		logger: d.logger.With("component", "reconciler"),
	}
}

type containerReconciler struct {
	driver *Driver
	config ContainerGCConfig
	once   sync.Once
	logger hclog.Logger
}

func (r *containerReconciler) Start() {
	if !r.config.Enabled {
		r.logger.Debug("skipping dangling containers handling; is disabled")
		return
	}

	r.once.Do(func() {
		go r.removeDanglingContainersGoroutine()
	})
}

func (r *containerReconciler) isDriverHealthy() bool {
	return r.driver.fingerprint.IsSet()
}

func (r *containerReconciler) removeDanglingContainersGoroutine() {
	period := r.config.period

	lastIterSucceeded := true

	// ensure that we wait for at least a period or creation timeout
	// for first container GC iteration
	// The initial period is a grace period for restore allocation
	// before a driver may kill containers launched by an earlier nomad
	// process.
	initialDelay := period
	if r.config.CreationGrace > initialDelay {
		initialDelay = r.config.CreationGrace
	}

	timer := time.NewTimer(initialDelay)
	for {
		select {
		case <-timer.C:
			if r.isDriverHealthy() {
				err := r.removeDanglingContainersIteration()
				if err != nil && lastIterSucceeded {
					r.logger.Warn("failed to remove dangling containers", "error", err)
				}
				lastIterSucceeded = (err == nil)
			}

			timer.Reset(period)
		case <-r.driver.ctx.Done():
			return
		}
	}
}

func (r *containerReconciler) removeDanglingContainersIteration() error {
	cutoff := time.Now().Add(-r.config.CreationGrace)
	tracked := r.trackedContainers()
	untracked, err := r.untrackedContainers(tracked, cutoff)
	if err != nil {
		return fmt.Errorf("failed to find untracked containers: %v", err)
	}

	if len(untracked) == 0 {
		return nil
	}

	if r.config.DryRun {
		var cids = make([]string, len(untracked))
		for i, c := range untracked {
			cids[i] = c.Name()
		}
		r.logger.Info("detected untracked containers", "container_ids", cids)
		return nil
	}

	for _, c := range untracked {
		id := c.Name()
		err = c.Destroy()
		if err != nil {
			r.logger.Warn("failed to remove untracked container", "container_id", id, "error", err)
		} else {
			r.logger.Info("removed untracked container", "container_id", id)
		}
		c.Release()
	}

	return nil
}

func (r *containerReconciler) trackedContainers() map[string]bool {
	d := r.driver
	d.tasks.lock.RLock()
	defer d.tasks.lock.RUnlock()

	res := make(map[string]bool, len(d.tasks.store))
	for _, h := range d.tasks.store {
		res[h.container.Name()] = true
	}

	return res
}

// untrackedContainers returns the ids of containers that suspected
// to have been started by Nomad but aren't tracked by this driver
func (r *containerReconciler) untrackedContainers(tracked map[string]bool, cutoffTime time.Time) ([]*lxc.Container, error) {
	result := []*lxc.Container{}
	cc := lxc.DefinedContainers(r.driver.lxcPath())
	cutoff := cutoffTime.Unix()

	for _, c := range cc {
		if tracked[c.Name()] {
			c.Release()
			continue
		}

		createdAt, err := elaspedTime(c)
		if err != nil {
			r.logger.Debug(fmt.Sprintf("calculating elasped time for %s: %v", c.Name(), err))
			c.Release()
			continue
		}

		if createdAt > cutoff {
			r.logger.Debug(fmt.Sprintf("container %s etime is %s < cutoff", c.Name(), time.Duration(createdAt).String()))
			c.Release()
			continue
		}

		if !isNomadContainer(c) {
			r.logger.Debug(fmt.Sprintf(
				"container %s is not created by Nomad.", c.Name(),
			))
			c.Release()
			continue
		}

		result = append(result, c)
	}

	return result, nil
}

func isNomadContainer(c *lxc.Container) bool {
	// Couldn't find a better way to detect if a container is provisioned by nomad
	// resorting to matching a allocation regexp
	if !hasNomadName(c) {
		// if !hasMount(c, "/alloc") || !hasNomadName(c) {
		return false
	}

	return true
}

func hasEnvVar(c *lxc.Container, p string) bool {
	mounts := c.ConfigItem("lxc.environment")

	for _, m := range mounts {
		if strings.Contains(m, p) {
			return true
		}
	}

	return false
}

func hasMount(c *lxc.Container, p string) bool {
	mounts := c.ConfigItem("lxc.log.file")

	for _, m := range mounts {
		if strings.Contains(m, p) {
			return true
		}
	}

	return false
}

var nomadContainerNamePattern = regexp.MustCompile(`.*-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func hasNomadName(c *lxc.Container) bool {
	if nomadContainerNamePattern.MatchString(c.Name()) {
		return true
	}

	return false
}

func pidElapsedTime(pid int) (int64, error) {
	if pid == 0 {
		return 0, fmt.Errorf("Missing PID")
	}

	r, err := psCommand("etime", pid)
	if err != nil {
		return 0, err
	}

	if len(r) < 1 {
		return 0, fmt.Errorf("Invalid `ps -o etime -p %d`", pid)
	}

	elapsedSegments := strings.Split(strings.Replace(r[0][0], "-", ":", 1), ":")
	var elapsedDurations []time.Duration
	for i := len(elapsedSegments) - 1; i >= 0; i-- {
		p, err := strconv.ParseInt(elapsedSegments[i], 10, 0)
		if err != nil {
			return 0, err
		}
		elapsedDurations = append(elapsedDurations, time.Duration(p))
	}

	var elapsed = time.Duration(elapsedDurations[0]) * time.Second
	if len(elapsedDurations) > 1 {
		elapsed += time.Duration(elapsedDurations[1]) * time.Minute
	}
	if len(elapsedDurations) > 2 {
		elapsed += time.Duration(elapsedDurations[2]) * time.Hour
	}
	if len(elapsedDurations) > 3 {
		elapsed += time.Duration(elapsedDurations[3]) * time.Hour * 24
	}

	start := time.Now().Add(-elapsed)
	return start.Unix(), nil
}

func psCommand(col string, pid int) ([][]string, error) {
	bin, err := exec.LookPath("ps")
	if err != nil {
		return [][]string{}, err
	}

	var args []string

	if pid == 0 { // will get from all processes.
		args = []string{"-ax", "-o", col}
	} else {
		args = []string{"-x", "-o", col, "-p", strconv.Itoa(int(pid))}
	}

	// only works on linux
	cmd := execute.ExecTask{
		Command: bin,
		Args:    args,
	}
	res, err := cmd.Execute()
	if err != nil {
		return [][]string{}, err
	}

	lines := strings.Split(string(res.Stdout), "\n")

	var ret [][]string
	for _, l := range lines[1:] {
		var lr []string
		for _, r := range strings.Split(l, " ") {
			if r == "" {
				continue
			}
			lr = append(lr, strings.TrimSpace(r))
		}
		if len(lr) != 0 {
			ret = append(ret, lr)
		}
	}

	return ret, nil
}

func elaspedTime(c *lxc.Container) (int64, error) {
	stat, err := os.Stat(c.ConfigPath())
	if err != nil {
		return 0, err
	}

	return stat.ModTime().Unix(), nil
}
