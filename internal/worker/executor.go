package worker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
)

type RunStatus struct {
	ExitCode   int       `json:"exit_code"`
	Error      string    `json:"error,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
}

func (a *Agent) launchPending(ctx context.Context) error {
	commands, err := a.store.PendingCommands(ctx, int(a.config.Capacity.MaxParallel))
	if err != nil {
		return err
	}
	for _, stored := range commands {
		if err := a.launch(ctx, stored.Command); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) launch(ctx context.Context, command domain.WorkerCommand) error {
	job := command.Job
	workDir := filepath.Join(a.config.WorkRoot, job.Spec.Project, job.ID, command.AttemptID)
	artifactDir := filepath.Join(a.config.ArtifactRoot, job.Spec.Project, job.ID, strconv.FormatUint(uint64(command.AttemptNumber), 10))
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return a.failBeforeLaunch(ctx, command, workDir, artifactDir, fmt.Errorf("create work directory: %w", err))
	}
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return a.failBeforeLaunch(ctx, command, workDir, artifactDir, fmt.Errorf("create artifact directory: %w", err))
	}
	statusPath := filepath.Join(artifactDir, "run-status.json")
	metricsPath := filepath.Join(artifactDir, "metrics.jsonl")
	logPath := filepath.Join(artifactDir, "run.log")
	if err := materializeSource(ctx, workDir, job.Spec.Source); err != nil {
		return a.failBeforeLaunch(ctx, command, workDir, artifactDir, err)
	}
	if len(job.Spec.SecretRefs) != 0 {
		return a.failBeforeLaunch(ctx, command, workDir, artifactDir, errors.New("secret references require a configured worker secret provider"))
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return a.failBeforeLaunch(ctx, command, workDir, artifactDir, fmt.Errorf("open run log: %w", err))
	}
	executable, err := os.Executable()
	if err != nil {
		logFile.Close()
		return a.failBeforeLaunch(ctx, command, workDir, artifactDir, err)
	}
	arguments := append([]string{"supervise", "--status", statusPath, "--"}, job.Spec.Command...)
	process := exec.Command(executable, arguments...)
	process.Dir = workDir
	process.Stdout = logFile
	process.Stderr = logFile
	process.Env = jobEnvironment(job, command, metricsPath, artifactDir)
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := process.Start(); err != nil {
		logFile.Close()
		return a.failBeforeLaunch(ctx, command, workDir, artifactDir, fmt.Errorf("start supervisor: %w", err))
	}
	logFile.Close()
	if err := a.store.MarkStarted(ctx, command.CommandID, process.Process.Pid, workDir, statusPath, metricsPath); err != nil {
		_ = syscall.Kill(-process.Process.Pid, syscall.SIGTERM)
		return err
	}
	go func() { _ = process.Wait() }()
	return nil
}

func (a *Agent) failBeforeLaunch(ctx context.Context, command domain.WorkerCommand, workDir, artifactDir string, runErr error) error {
	statusPath := filepath.Join(artifactDir, "run-status.json")
	metricsPath := filepath.Join(artifactDir, "metrics.jsonl")
	if err := a.store.MarkStarted(ctx, command.CommandID, 0, workDir, statusPath, metricsPath); err != nil {
		return err
	}
	return a.store.MarkFinished(ctx, command.CommandID, -1, runErr.Error(), workerURI(a.config.WorkerID, command, "run.log"), nil)
}

func materializeSource(ctx context.Context, workDir string, source domain.Source) error {
	if source.OCIImage != "" {
		return errors.New("OCI image execution is not implemented")
	}
	if source.GitURL == "" || source.Commit == "" {
		return errors.New("git URL and commit are required")
	}
	gitDir := filepath.Join(workDir, ".git")
	if _, err := os.Stat(gitDir); errors.Is(err, os.ErrNotExist) {
		if output, err := exec.CommandContext(ctx, "git", "init", workDir).CombinedOutput(); err != nil {
			return fmt.Errorf("initialize repository: %w: %s", err, strings.TrimSpace(string(output)))
		}
		if output, err := exec.CommandContext(ctx, "git", "-C", workDir, "remote", "add", "origin", source.GitURL).CombinedOutput(); err != nil {
			return fmt.Errorf("configure repository: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	if output, err := exec.CommandContext(ctx, "git", "-C", workDir, "fetch", "--depth=1", "origin", source.Commit).CombinedOutput(); err != nil {
		return fmt.Errorf("fetch source commit: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if output, err := exec.CommandContext(ctx, "git", "-C", workDir, "checkout", "--detach", "--force", "FETCH_HEAD").CombinedOutput(); err != nil {
		return fmt.Errorf("check out source commit: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func jobEnvironment(job domain.Job, command domain.WorkerCommand, metricsPath, artifactDir string) []string {
	environment := append([]string{}, os.Environ()...)
	for name, value := range job.Spec.Environment {
		environment = append(environment, name+"="+value)
	}
	environment = append(environment,
		"LCJS_JOB_ID="+job.ID,
		"LCJS_ATTEMPT_ID="+command.AttemptID,
		"LCJS_ATTEMPT_NUMBER="+strconv.FormatUint(uint64(command.AttemptNumber), 10),
		"LCJS_METRICS_FILE="+metricsPath,
		"LCJS_ARTIFACT_DIR="+artifactDir,
	)
	return environment
}

func (a *Agent) reconcileRunning(ctx context.Context) error {
	commands, err := a.store.RunningCommands(ctx)
	if err != nil {
		return err
	}
	for _, command := range commands {
		if err := a.ingestMetrics(ctx, command); err != nil {
			return err
		}
		status, found, err := readRunStatus(command.StatusPath)
		if err != nil {
			return err
		}
		if !found {
			if command.PID > 0 && processAlive(command.PID) {
				continue
			}
			status = RunStatus{ExitCode: -1, Error: "worker supervisor disappeared before recording an exit status", FinishedAt: time.Now().UTC()}
		}
		artifacts, err := collectArtifacts(a.config.WorkerID, a.config.ArtifactRoot, command)
		if err != nil {
			status.ExitCode = -1
			status.Error = errors.Join(errors.New(status.Error), err).Error()
		}
		if err := a.store.MarkFinished(ctx, command.Command.CommandID, status.ExitCode, status.Error, workerURI(a.config.WorkerID, command.Command, "run.log"), artifacts); err != nil {
			return err
		}
	}
	return nil
}

func readRunStatus(path string) (RunStatus, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return RunStatus{}, false, nil
	}
	if err != nil {
		return RunStatus{}, false, fmt.Errorf("open run status: %w", err)
	}
	defer file.Close()
	var status RunStatus
	if err := json.NewDecoder(file).Decode(&status); err != nil {
		return RunStatus{}, false, fmt.Errorf("decode run status: %w", err)
	}
	return status, true, nil
}

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func (a *Agent) ingestMetrics(ctx context.Context, command StoredCommand) error {
	file, err := os.Open(command.MetricsPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open metrics file: %w", err)
	}
	defer file.Close()
	if _, err := file.Seek(command.MetricOffset, io.SeekStart); err != nil {
		return err
	}
	reader := bufio.NewReader(file)
	offset := command.MetricOffset
	for {
		line, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read metric line: %w", err)
		}
		if len(line) > 1<<20 {
			return errors.New("metric line exceeds 1 MiB")
		}
		var sample domain.MetricSample
		if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &sample); err != nil {
			return fmt.Errorf("decode metric line at offset %d: %w", offset, err)
		}
		if sample.ObservedAt.IsZero() {
			sample.ObservedAt = time.Now().UTC()
		}
		newOffset := offset + int64(len(line))
		if err := a.store.EnqueueMetric(ctx, command.Command.CommandID, command.Command.AttemptID, sample, newOffset); err != nil {
			return err
		}
		offset = newOffset
	}
	return nil
}

func collectArtifacts(workerID, artifactRoot string, command StoredCommand) ([]domain.ArtifactAnnouncement, error) {
	rules := append([]domain.ArtifactRule{}, command.Command.Job.Spec.Artifacts...)
	if checkpoint := command.Command.Job.Spec.Checkpoint; checkpoint != nil {
		rules = append(rules, domain.ArtifactRule{Name: "checkpoint", Glob: checkpoint.Glob})
	}
	announcements := make([]domain.ArtifactAnnouncement, 0)
	for _, rule := range rules {
		if filepath.IsAbs(rule.Glob) || strings.Contains(filepath.ToSlash(rule.Glob), "../") {
			return nil, fmt.Errorf("artifact glob %q escapes the work directory", rule.Glob)
		}
		matches, err := filepath.Glob(filepath.Join(command.WorkDir, rule.Glob))
		if err != nil {
			return nil, fmt.Errorf("expand artifact %s: %w", rule.Name, err)
		}
		for _, sourcePath := range matches {
			info, err := os.Stat(sourcePath)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			relative, err := filepath.Rel(command.WorkDir, sourcePath)
			if err != nil || strings.HasPrefix(relative, "..") {
				return nil, fmt.Errorf("artifact %q is outside the work directory", sourcePath)
			}
			destination := filepath.Join(artifactRoot, command.Command.Job.Spec.Project, command.Command.Job.ID, strconv.FormatUint(uint64(command.Command.AttemptNumber), 10), rule.Name, relative)
			size, checksum, err := copyArtifact(sourcePath, destination)
			if err != nil {
				return nil, err
			}
			uri := workerURI(workerID, command.Command, filepath.ToSlash(filepath.Join(rule.Name, relative)))
			announcements = append(announcements, domain.ArtifactAnnouncement{Name: rule.Name, URI: uri, SizeBytes: size, SHA256: checksum})
		}
	}
	return announcements, nil
}

func copyArtifact(sourcePath, destination string) (int64, string, error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return 0, "", err
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return 0, "", err
	}
	defer source.Close()
	temporary := destination + ".partial"
	target, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(target, hash), source)
	syncErr := target.Sync()
	closeErr := target.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return 0, "", err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func workerURI(workerID string, command domain.WorkerCommand, name string) string {
	return "worker://" + workerID + "/" + command.Job.Spec.Project + "/" + command.Job.ID + "/" + strconv.FormatUint(uint64(command.AttemptNumber), 10) + "/" + strings.TrimLeft(filepath.ToSlash(name), "/")
}
