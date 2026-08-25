package processtree

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type processIdentity struct {
	pid, parent int
	start       uint64
}

func terminateTree(root int) {
	_ = syscall.Kill(-root, syscall.SIGSTOP)
	opened := make(map[processIdentity]int)
	for range 4 {
		for _, process := range descendants(root) {
			if _, ok := opened[process]; ok {
				continue
			}
			fd, err := openProcess(process)
			if err != nil {
				continue
			}
			opened[process] = fd
			_ = unix.PidfdSendSignal(fd, unix.SIGSTOP, nil, 0)
		}
	}
	for _, fd := range opened {
		_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		_ = unix.Close(fd)
	}
	_ = syscall.Kill(-root, syscall.SIGKILL)
	_ = syscall.Kill(root, syscall.SIGKILL)
}

func openProcess(process processIdentity) (int, error) {
	fd, err := unix.PidfdOpen(process.pid, 0)
	if err != nil {
		return -1, err
	}
	current, err := readProcess(process.pid)
	if err != nil || current != process {
		_ = unix.Close(fd)
		return -1, os.ErrInvalid
	}
	return fd, nil
}

func descendants(root int) []processIdentity {
	parents := make(map[int][]processIdentity)
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		process, err := readProcess(pid)
		if err != nil {
			continue
		}
		parents[process.parent] = append(parents[process.parent], process)
	}
	var found []processIdentity
	queue := []int{root}
	seen := map[int]bool{root: true}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, process := range parents[parent] {
			if seen[process.pid] {
				continue
			}
			seen[process.pid] = true
			found = append(found, process)
			queue = append(queue, process.pid)
		}
	}
	return found
}

func readProcess(pid int) (processIdentity, error) {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return processIdentity{}, err
	}
	process, err := parseProcessStat(stat)
	if err != nil || process.pid != pid {
		return processIdentity{}, os.ErrInvalid
	}
	return process, nil
}

func parseProcessStat(stat []byte) (processIdentity, error) {
	space, close := bytes.IndexByte(stat, ' '), bytes.LastIndexByte(stat, ')')
	if space < 1 || close < space {
		return processIdentity{}, os.ErrInvalid
	}
	pid, err := strconv.Atoi(string(stat[:space]))
	if err != nil {
		return processIdentity{}, err
	}
	fields := strings.Fields(string(stat[close+1:]))
	if len(fields) < 20 {
		return processIdentity{}, os.ErrInvalid
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return processIdentity{}, err
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	return processIdentity{pid: pid, parent: parent, start: start}, err
}
