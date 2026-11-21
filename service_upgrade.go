package poudrine

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// The daemon still supervises the same PID across exec. Transfer the idle
// worker's queue and deadlines through a private, unlinked file descriptor.
type serviceUpgrade struct {
	exe    string
	worker *Builder
}

func (*serviceUpgrade) Error() string { return "service binary ready for replacement" }

const checkpointFDEnv = "POUDRINE_RESTART_FD"

type serviceCheckpoint struct {
	Config, State, PoudriereConfig, BaseFS, Jail, Ports, PackagesFile string
	Worker                                                            *workerCheckpoint
}

func checkpointIdentity(o options) serviceCheckpoint {
	return serviceCheckpoint{Config: o.configPath, State: o.state, PoudriereConfig: o.builder.ConfigDir,
		BaseFS: o.builder.BaseFS, Jail: o.builder.Jail, Ports: o.builder.Ports, PackagesFile: o.builder.PackagesFile}
}

func checkpointFile(o options, cp *workerCheckpoint) (*os.File, error) {
	// The restart socket already has a protected directory, even if the
	// worker is still installing prerequisites. A regular file permits queues
	// larger than a pipe or the exec argument/environment size limits.
	dir := o.builder.ConfigDir
	if socket := os.Getenv("POUDRINE_RESTART_SOCKET"); socket != "" {
		dir = filepath.Dir(socket)
	}
	f, err := os.CreateTemp(dir, ".poudrine-restart-*")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, err
	}
	state := checkpointIdentity(o)
	state.Worker = cp
	if err := json.NewEncoder(f).Encode(state); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func readServiceCheckpoint(o options) (*workerCheckpoint, error) {
	value, ok := os.LookupEnv(checkpointFDEnv)
	if !ok {
		return nil, nil
	}
	os.Unsetenv(checkpointFDEnv)
	fd, err := strconv.Atoi(value)
	if err != nil || fd < 3 {
		return nil, errors.New("invalid inherited service checkpoint descriptor")
	}
	syscall.CloseOnExec(fd)
	f := os.NewFile(uintptr(fd), "service-checkpoint")
	if f == nil {
		return nil, errors.New("missing service checkpoint")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() || info.Sys().(*syscall.Stat_t).Nlink != 0 {
		return nil, errors.New("unexpected inherited service checkpoint")
	}
	var state serviceCheckpoint
	if err := json.NewDecoder(f).Decode(&state); err != nil {
		return nil, fmt.Errorf("read service checkpoint: %w", err)
	}
	cp := state.Worker
	state.Worker = nil
	if state != checkpointIdentity(o) {
		log.Print("worker paths or jail changed; initializing the new worker schedule")
		return nil, nil
	}
	return cp, nil
}

func (u *serviceUpgrade) exec(o options, cp *workerCheckpoint) error {
	f, err := checkpointFile(o, cp)
	if err != nil {
		return err
	}
	defer f.Close()
	fd := f.Fd()
	if _, err := unix.FcntlInt(fd, unix.F_SETFD, 0); err != nil {
		return err
	}
	env := append(os.Environ(), checkpointFDEnv+"="+strconv.Itoa(int(fd)))
	log.Printf("starting updated service from %s", u.exe)
	return syscall.Exec(u.exe, []string{u.exe, "run", "-config", o.configPath}, env)
}
