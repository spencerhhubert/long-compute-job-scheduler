package worker

import (
	"bytes"
	"context"
	"math"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
)

func TestReadFileTailBoundsRecentLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	content := strings.Repeat("old\n", 100) + "final-result\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	tail, err := readFileTail(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) > 32 || !strings.HasSuffix(tail, "final-result\n") || strings.HasPrefix(tail, "old\nold\nold\n") {
		t.Fatalf("tail = %q", tail)
	}
}

func testCommand(project string) domain.WorkerCommand {
	return domain.WorkerCommand{
		CommandID: "cmd_test", AttemptID: "att_test", AttemptNumber: 1, Kind: "run_job",
		Job: domain.Job{
			ID: "job_test",
			Spec: domain.JobSpec{
				Project: project, Name: "train",
				Command:     []string{"python", "train.py"},
				Environment: map[string]string{"PYTHONUNBUFFERED": "1"},
				Retry:       domain.RetryPolicy{MaxAttempts: 1, LostWorker: domain.LostWorkerManual},
			},
		},
	}
}

func TestBuildProcessRunsDirectlyInProjectDirectory(t *testing.T) {
	command := testCommand("example-research")
	projectDir := t.TempDir()
	process := buildProcess("/usr/local/bin/lcjs", command.Job, command, projectDir, "/tmp/status.json", "/tmp/metrics.jsonl", "/tmp/artifacts")

	if process.Dir != projectDir {
		t.Fatalf("process dir = %q, want project directory %q", process.Dir, projectDir)
	}
	wantArgs := []string{"/usr/local/bin/lcjs", "supervise", "--status", "/tmp/status.json", "--", "python", "train.py"}
	if !slices.Equal(process.Args, wantArgs) {
		t.Fatalf("process args = %v, want %v", process.Args, wantArgs)
	}
	for _, want := range []string{
		"PYTHONUNBUFFERED=1",
		"LCJS_JOB_ID=job_test",
		"LCJS_ATTEMPT_ID=att_test",
		"LCJS_ATTEMPT_NUMBER=1",
		"LCJS_METRICS_FILE=/tmp/metrics.jsonl",
		"LCJS_ARTIFACT_DIR=/tmp/artifacts",
	} {
		if !slices.Contains(process.Env, want) {
			t.Errorf("environment does not contain %q", want)
		}
	}
	// The worker process environment is inherited, not replaced.
	if len(process.Env) < len(os.Environ()) {
		t.Fatalf("environment has %d entries, want at least the inherited %d", len(process.Env), len(os.Environ()))
	}
}

func TestCollectArtifactsInlinesOnlySmallTextContent(t *testing.T) {
	workDir := t.TempDir()
	command := testCommand("example-research")
	command.Job.Spec.Artifacts = []domain.ArtifactRule{
		{Name: "result", Glob: "champion.json"},
		{Name: "weights", Glob: "model.bin"},
		{Name: "trace", Glob: "trace.log"},
	}
	const text = "{\"score\": 0.91}\n"
	large := bytes.Repeat([]byte("line\n"), maxInlineArtifactBytes/5+1)
	for name, content := range map[string][]byte{
		"champion.json": []byte(text),
		"model.bin":     {0xff, 0xfe, 0x00, 0x01},
		"trace.log":     large,
	} {
		if err := os.WriteFile(filepath.Join(workDir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	announcements, err := collectArtifacts("wrk_test", t.TempDir(), StoredCommand{Command: command, WorkDir: workDir})
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]domain.ArtifactAnnouncement, len(announcements))
	for _, announcement := range announcements {
		byName[announcement.Name] = announcement
	}
	if got := byName["result"]; got.Content != text {
		t.Fatalf("small text artifact content = %q, want %q", got.Content, text)
	}
	if got := byName["weights"]; got.Content != "" || got.SizeBytes != 4 {
		t.Fatalf("binary artifact = %+v, want metadata without content", got)
	}
	if got := byName["trace"]; got.Content != "" || got.SizeBytes != int64(len(large)) {
		t.Fatalf("large artifact = %+v, want metadata without content", got)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

func runGit(t *testing.T, dir string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, arguments...)...)
	command.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

// newTestRepo creates a repository on branch main with one committed file and
// returns its directory and head commit.
func newTestRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "checkout", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte("print('one')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "train.py")
	runGit(t, dir, "commit", "-q", "-m", "first")
	return dir, runGit(t, dir, "rev-parse", "HEAD")
}

func TestObserveGitStateRecordsRepositoryBranchAndDirtyFlag(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	if state := observeGitState(ctx, t.TempDir()); state != nil {
		t.Fatalf("state for a non-repository = %+v, want nil", state)
	}

	dir, head := newTestRepo(t)
	state := observeGitState(ctx, dir)
	if state == nil || state.Commit != head || state.Branch != "main" || state.Dirty {
		t.Fatalf("clean state = %+v, want commit %s on main, not dirty", state, head)
	}

	// Untracked files do not make the tree dirty.
	if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if state := observeGitState(ctx, dir); state == nil || state.Dirty {
		t.Fatalf("state with untracked file = %+v, want clean", state)
	}

	// Modifying a tracked file does.
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte("print('changed')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if state := observeGitState(ctx, dir); state == nil || !state.Dirty {
		t.Fatalf("state with modified tracked file = %+v, want dirty", state)
	}
}

func TestPrepareSourceWithoutPinRecordsStateAsIs(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	dir, head := newTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte("print('wip')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := prepareSource(ctx, dir, domain.Source{})
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Commit != head || !state.Dirty {
		t.Fatalf("state = %+v, want dirty tree at %s recorded without error", state, head)
	}

	// A non-repository directory is also fine without a pin.
	state, err = prepareSource(ctx, t.TempDir(), domain.Source{})
	if err != nil || state != nil {
		t.Fatalf("non-repository = %+v, %v, want nil state and no error", state, err)
	}
}

func TestPrepareSourceChecksOutPinnedCommitInCleanTree(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	dir, first := newTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte("print('two')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "commit", "-q", "-a", "-m", "second")

	state, err := prepareSource(ctx, dir, domain.Source{Commit: first})
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Commit != first || state.Branch != "" || state.Dirty {
		t.Fatalf("state = %+v, want detached clean checkout of %s", state, first)
	}
	if head := runGit(t, dir, "rev-parse", "HEAD"); head != first {
		t.Fatalf("HEAD = %s, want pinned %s", head, first)
	}
}

func TestPrepareSourceRefusesPinnedCommitInDirtyOrMissingRepository(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	dir, first := newTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte("print('wip')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := prepareSource(ctx, dir, domain.Source{Commit: first})
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("error = %v, want uncommitted-changes refusal", err)
	}
	if state == nil || !state.Dirty {
		t.Fatalf("state = %+v, want the observed dirty state", state)
	}

	if _, err := prepareSource(ctx, t.TempDir(), domain.Source{Commit: first}); err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("error = %v, want non-repository refusal", err)
	}

	unknown := strings.Repeat("d", 40)
	clean, _ := newTestRepo(t)
	if _, err := prepareSource(ctx, clean, domain.Source{Commit: unknown}); err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("error = %v, want missing-commit refusal", err)
	}
}

func TestLaunchFailsAttemptWhenProjectIsNotMapped(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	store, err := OpenStore(ctx, filepath.Join(base, "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	agent := &Agent{
		config: Config{
			WorkerID:     "wrk_test",
			ArtifactRoot: filepath.Join(base, "artifacts"),
			Projects:     map[string]string{"other-project": base},
		},
		store: store,
	}
	command := testCommand("example-research")
	if err := store.AcceptCommands(ctx, []domain.WorkerCommand{command}); err != nil {
		t.Fatal(err)
	}
	if err := agent.launchPending(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var finished *domain.WorkerEvent
	for index, event := range events {
		if event.Kind == domain.WorkerEventAttemptFinished {
			finished = &events[index]
		}
	}
	if finished == nil || finished.ExitCode == nil || *finished.ExitCode != -1 || !strings.Contains(finished.Error, "no project directory") {
		t.Fatalf("finished event = %+v, want failure naming the missing project directory", finished)
	}
}

func TestAnnounceProgressOnlyRepublishesWhatChanged(t *testing.T) {
	// The point of announcing mid-run is that a long run's output can be read
	// while it is going; the point of the change check is that a checkpoint
	// rewritten every few seconds must not flood the event stream.
	agent := &Agent{
		config:            Config{WorkerID: "wrk_test"},
		lastArtifactSweep: map[string]time.Time{},
		lastArtifactHash:  map[string]map[string]string{},
	}
	id := "cmd_test"
	agent.lastArtifactHash[id] = map[string]string{"champion": "abc"}

	seen := agent.lastArtifactHash[id]
	unchanged := domain.ArtifactAnnouncement{Name: "champion", SHA256: "abc"}
	if seen[unchanged.Name] != unchanged.SHA256 {
		t.Fatal("fixture is wrong")
	}
	moved := domain.ArtifactAnnouncement{Name: "champion", SHA256: "def"}
	if seen[moved.Name] == moved.SHA256 {
		t.Fatal("a changed artifact must not look unchanged")
	}
	agent.lastArtifactSweep[id] = time.Now()
	if time.Since(agent.lastArtifactSweep[id]) >= artifactInterval {
		t.Fatal("a sweep that just happened must suppress the next one")
	}
}

func TestOneBadMetricLineDoesNotStopTheWorker(t *testing.T) {
	// A job wrote NaN, the reader rejected the line, and every cycle from
	// then on failed on the same offset: no attempt was reaped and nothing
	// was scheduled while the queue looked healthy. The line must be skipped.
	for _, line := range []string{
		`{"name":"x","value":NaN}`,
		`{"name":"x","value":`,
		`not json at all`,
	} {
		var sample domain.MetricSample
		if err := json.Unmarshal([]byte(line), &sample); err == nil {
			t.Fatalf("expected %q to be unreadable", line)
		}
	}
	var finite domain.MetricSample
	if err := json.Unmarshal([]byte(`{"name":"x","value":1.5}`), &finite); err != nil {
		t.Fatalf("a good line must still parse: %v", err)
	}
	if math.IsNaN(finite.Value) || math.IsInf(finite.Value, 0) {
		t.Fatal("a finite sample must not be treated as non-finite")
	}
}
