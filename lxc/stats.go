package lxc

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/pkg/errors"
)

const (
	// statsCollectorBackoffBaseline is the baseline time for exponential
	// backoff while calling the docker stats api.
	statsCollectorBackoffBaseline = 5 * time.Second

	// statsCollectorBackoffLimit is the limit of the exponential backoff for
	// calling the docker stats api.
	statsCollectorBackoffLimit = 2 * time.Minute
)

func (h *taskHandle) stats(ctx context.Context, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	ch := make(chan *drivers.TaskResourceUsage)
	go h.handleStats(ctx, ch, interval)
	return ch, nil
}

func (h *taskHandle) handleStats(ctx context.Context, ch chan *drivers.TaskResourceUsage, interval time.Duration) {
	defer close(ch)
	timer := time.NewTimer(0)
	h.logger.Info("starting stats emitter", "interval", interval.String())

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			timer.Reset(interval)
		}

		taskResUsage, err := h.getTaskResourceUsage()
		if err != nil {
			h.logger.Error(err.Error())
			time.Sleep(interval)
			return
		}

		select {
		case <-ctx.Done():
			return
		case ch <- taskResUsage:
		}
	}
}

func (h *taskHandle) handleBackoffStats(ctx context.Context, ch chan *drivers.TaskResourceUsage, interval time.Duration) {
	defer close(ch)
	timer := time.NewTimer(0)

	var backoff time.Duration
	var retry int
	for {
		if backoff > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-h.doneCh:
			return
		case <-timer.C:
			timer.Reset(interval)
		}

		taskResUsage, err := h.getTaskResourceUsage()
		if err != nil {
			h.logger.Error("collecting usage", "error", err, "backoff", backoff.String())
			// Calculate the new backoff
			backoff = (1 << (2 * uint64(retry))) * statsCollectorBackoffBaseline
			if backoff > statsCollectorBackoffLimit {
				backoff = statsCollectorBackoffLimit
			}
			// Increment retry counter
			retry++
			continue
		}

		select {
		case <-ctx.Done():
			return
		case ch <- taskResUsage:
		}

		backoff = 0
		retry = 0
	}
}

func (h *taskHandle) getTaskResourceUsage() (*drivers.TaskResourceUsage, error) {
	cpuStats, err := h.container.CPUStats()
	if err != nil {
		return nil, errors.Wrap(err, "CPU stats")
	}
	total, err := h.container.CPUTime()
	if err != nil {
		return nil, errors.Wrap(err, "CPU time")
	}

	t := time.Now()

	// Get the cpu stats
	system := cpuStats["system"]
	user := cpuStats["user"]
	cs := &drivers.CpuStats{
		SystemMode: h.systemCpuStats.Percent(float64(system)),
		UserMode:   h.systemCpuStats.Percent(float64(user)),
		Percent:    h.totalCpuStats.Percent(float64(total)),
		TotalTicks: float64(user + system),
		Measured:   LXCMeasuredCpuStats,
	}

	// Get the Memory Stats
	memData := map[string]uint64{
		"rss":   0,
		"cache": 0,
		"swap":  0,
	}
	rawMemStats := h.container.CgroupItem("memory.stat")
	for _, rawMemStat := range rawMemStats {
		key, val, err := keysToVal(rawMemStat)
		if err != nil {
			h.logger.Error("failed to get stat", "line", rawMemStat, "error", err)
			continue
		}
		if _, ok := memData[key]; ok {
			memData[key] = val

		}
	}
	ms := &drivers.MemoryStats{
		RSS:      memData["rss"],
		Cache:    memData["cache"],
		Swap:     memData["swap"],
		Measured: LXCMeasuredMemStats,
	}

	mu := h.container.CgroupItem("memory.max_usage_in_bytes")
	for _, rawMemMaxUsage := range mu {
		val, err := strconv.ParseUint(rawMemMaxUsage, 10, 64)
		if err != nil {
			h.logger.Error("failed to get max memory usage", "error", err)
			continue
		}
		ms.MaxUsage = val
	}
	ku := h.container.CgroupItem("memory.kmem.usage_in_bytes")
	for _, rawKernelUsage := range ku {
		val, err := strconv.ParseUint(rawKernelUsage, 10, 64)
		if err != nil {
			h.logger.Error("failed to get kernel memory usage", "error", err)
			continue
		}
		ms.KernelUsage = val
	}

	mku := h.container.CgroupItem("memory.kmem.max_usage_in_bytes")
	for _, rawMaxKernelUsage := range mku {
		val, err := strconv.ParseUint(rawMaxKernelUsage, 10, 64)
		if err != nil {
			h.logger.Error("failed tog get max kernel memory usage", "error", err)
			continue
		}
		ms.KernelMaxUsage = val
	}

	taskResUsage := &drivers.TaskResourceUsage{
		ResourceUsage: &drivers.ResourceUsage{
			CpuStats:    cs,
			MemoryStats: ms,
		},
		Timestamp: t.UTC().UnixNano(),
	}

	return taskResUsage, nil
}

func keysToVal(line string) (string, uint64, error) {
	tokens := strings.Split(line, " ")
	if len(tokens) != 2 {
		return "", 0, fmt.Errorf("line isn't a k/v pair")
	}
	key := tokens[0]
	val, err := strconv.ParseUint(tokens[1], 10, 64)
	return key, val, err
}

func asyncCopy(out io.Writer, in io.Reader) {
	_, err := io.Copy(out, in)
	if err != nil {
		fmt.Printf("ERROR: io.Copy %s\n", err.Error())
	}
}
