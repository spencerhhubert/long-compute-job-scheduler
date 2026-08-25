package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	workeragent "github.com/spencerhhubert/long-compute-job-scheduler/internal/worker"
)

func runSupervise(arguments []string) error {
	flags := flag.NewFlagSet("supervise", flag.ContinueOnError)
	statusPath := flags.String("status", "", "atomic exit-status file")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	command := flags.Args()
	if *statusPath == "" || len(command) == 0 {
		return errors.New("usage: lcjs supervise --status PATH -- COMMAND [ARG...]")
	}
	process := exec.Command(command[0], command[1:]...)
	process.Stdin = os.Stdin
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	process.Env = os.Environ()
	status := workeragent.RunStatus{ExitCode: -1, FinishedAt: time.Now().UTC()}
	if err := process.Start(); err != nil {
		status.Error = err.Error()
		return writeStatus(*statusPath, status)
	}
	// A canceled attempt is terminated by signaling the whole process group,
	// which includes this supervisor. Forwarding the signal to the job and
	// staying alive lets the exit status still be recorded; only SIGKILL loses
	// it.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for received := range signals {
			_ = process.Process.Signal(received)
		}
	}()
	err := process.Wait()
	signal.Stop(signals)
	status.FinishedAt = time.Now().UTC()
	if err == nil {
		status.ExitCode = 0
	} else {
		status.Error = err.Error()
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			status.ExitCode = exitError.ExitCode()
		}
	}
	if err := writeStatus(*statusPath, status); err != nil {
		return err
	}
	// The supervisor itself exits successfully once it has durably recorded the
	// child result. The worker reports the child's exit code to the control plane.
	return nil
}

func writeStatus(path string, status workeragent.RunStatus) error {
	temporary := path + ".tmp-" + strconv.Itoa(os.Getpid())
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create status file: %w", err)
	}
	encodeErr := json.NewEncoder(file).Encode(status)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(encodeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write status file: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("publish status file: %w", err)
	}
	return nil
}
