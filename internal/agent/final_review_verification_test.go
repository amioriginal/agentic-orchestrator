// Copyright 2026 DoorDash, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/doordash-oss/agentic-orchestrator/internal/feature"
)

func TestFinalReviewContractVerificationReusesOnlyExactCandidateAndContract(t *testing.T) {
	state, contractPath, repo := newFinalReviewVerificationState(t, "#!/bin/sh\necho ok\n")

	firstDir := filepath.Join(state.artifactDir, "iteration-01")
	evidence, status, feedback, err := state.runFinalReviewContractVerification(1, firstDir)
	if err != nil || status != ReviewApproved || feedback != "" {
		t.Fatalf("first verification = status=%s feedback=%q err=%v", status, feedback, err)
	}
	if len(evidence.ReportPaths) != 1 {
		t.Fatalf("first report paths = %+v, want one", evidence.ReportPaths)
	}
	runsAfterFirst := countFinalReviewEvidenceRuns(t, state.artifactDir)
	if runsAfterFirst == 0 {
		t.Fatal("first final review must execute the harness-owned contract")
	}

	secondDir := filepath.Join(state.artifactDir, "iteration-02")
	_, status, feedback, err = state.runFinalReviewContractVerification(2, secondDir)
	if err != nil || status != ReviewApproved || feedback != "" {
		t.Fatalf("second verification = status=%s feedback=%q err=%v", status, feedback, err)
	}
	if runsAfterSecond := countFinalReviewEvidenceRuns(t, state.artifactDir); runsAfterSecond != runsAfterFirst {
		t.Fatalf("same candidate reran contract: runs=%d want=%d", runsAfterSecond, runsAfterFirst)
	}
	contract, err := ReadTestingContract(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	contract.Items[0].Run.Shell = "./verify.sh && printf contract-changed"
	if err := WriteTestingContract(contractPath, *contract); err != nil {
		t.Fatal(err)
	}
	thirdDir := filepath.Join(state.artifactDir, "iteration-03")
	_, status, feedback, err = state.runFinalReviewContractVerification(3, thirdDir)
	if err != nil || status != ReviewApproved || feedback != "" {
		t.Fatalf("changed contract verification = status=%s feedback=%q err=%v", status, feedback, err)
	}
	runsAfterContractChange := countFinalReviewEvidenceRuns(t, state.artifactDir)
	if runsAfterContractChange <= runsAfterFirst {
		t.Fatalf("same-revision changed contract reused stale evidence: runs=%d first=%d", runsAfterContractChange, runsAfterFirst)
	}

	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	fourthDir := filepath.Join(state.artifactDir, "iteration-04")
	_, status, feedback, err = state.runFinalReviewContractVerification(4, fourthDir)
	if err != nil || status != ReviewApproved || feedback != "" {
		t.Fatalf("changed verification = status=%s feedback=%q err=%v", status, feedback, err)
	}
	if runsAfterCandidateChange := countFinalReviewEvidenceRuns(t, state.artifactDir); runsAfterCandidateChange <= runsAfterContractChange {
		t.Fatalf("changed candidate reused stale contract evidence: runs=%d prior=%d", runsAfterCandidateChange, runsAfterContractChange)
	}
}

func countFinalReviewEvidenceRuns(t *testing.T, root string) int {
	t.Helper()
	runs := 0
	_ = filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() && strings.HasPrefix(entry.Name(), "run-") {
			runs++
		}
		return nil
	})
	return runs
}

func TestFinalReviewContractVerificationBlocksRegressionBeforeLLMReview(t *testing.T) {
	state, _, repo := newFinalReviewVerificationState(t, "#!/bin/sh\necho ok\n")
	if err := os.WriteFile(filepath.Join(repo, "verify.sh"), []byte("#!/bin/sh\necho broken >&2\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, status, feedback, err := state.runFinalReviewContractVerification(1, filepath.Join(state.artifactDir, "iteration-01"))
	if err != nil {
		t.Fatalf("verification error = %v", err)
	}
	if status != ReviewChangesRequested || !strings.Contains(feedback, "regression") {
		t.Fatalf("status=%s feedback=%q, want mechanical regression block", status, feedback)
	}
}

func TestFinalReviewContractVerificationRestoresCommandMutation(t *testing.T) {
	state, _, repo := newFinalReviewVerificationState(t, "#!/bin/sh\nprintf '# mutation\\n' >> verify.sh\n")
	original, err := os.ReadFile(filepath.Join(repo, "verify.sh"))
	if err != nil {
		t.Fatal(err)
	}

	_, status, _, err := state.runFinalReviewContractVerification(1, filepath.Join(state.artifactDir, "iteration-01"))
	if err == nil || status != ReviewFailed {
		t.Fatalf("mutation verification = status=%s err=%v, want restored protocol failure", status, err)
	}
	after, readErr := os.ReadFile(filepath.Join(repo, "verify.sh"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(original) {
		t.Fatalf("harness mutation was not restored:\n%s", after)
	}
}

func TestFinalReviewContractVerificationStopsOnBlockedCapability(t *testing.T) {
	state, contractPath, _ := newFinalReviewVerificationState(t, "#!/bin/sh\necho ok\n")
	contract, err := ReadTestingContract(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	contract.Items[0].Capabilities = []TestingContractCapability{{Name: "required session", Probe: "exit 1", OnMissing: "need_user_input"}}
	contract.Items[0].Policy.AllowBlocked = true
	contract.Items[0].Policy.AllowWaiver = true
	if err := WriteTestingContract(contractPath, *contract); err != nil {
		t.Fatal(err)
	}

	_, status, _, err := state.runFinalReviewContractVerification(1, filepath.Join(state.artifactDir, "iteration-01"))
	var terminal *finalReviewVerificationTerminal
	if status != ReviewFailed || !errors.As(err, &terminal) || terminal.status != "need_user_input" {
		t.Fatalf("blocked verification = status=%s terminal=%+v err=%v", status, terminal, err)
	}
	if terminal.inputPath == "" {
		t.Fatal("blocked verification did not publish a need-user-input gate")
	}
	if _, statErr := os.Stat(terminal.inputPath); statErr != nil {
		t.Fatalf("need-user-input gate missing: %v", statErr)
	}
}

func TestRunFeatureFinalReviewLoopPersistsBlockedVerificationGate(t *testing.T) {
	state, contractPath, _ := newFinalReviewVerificationState(t, "#!/bin/sh\necho ok\n")
	contract, err := ReadTestingContract(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	contract.Items[0].Capabilities = []TestingContractCapability{{Name: "required session", Probe: "exit 1", OnMissing: "need_user_input"}}
	contract.Items[0].Policy.AllowBlocked = true
	contract.Items[0].Policy.AllowWaiver = true
	if err := WriteTestingContract(contractPath, *contract); err != nil {
		t.Fatal(err)
	}
	f := state.cfg.Feature
	f.Status = feature.StatusFinalReviewing
	f.CurrentPhase = feature.PhaseReview
	f.CurrentRoadmapPhase = 1
	f.SchemaVersion = feature.SchemaVersionCurrent
	f.RepoStates = map[string]*feature.RepoState{"repo": {Touched: true}}
	store := feature.NewStore(state.cfg.StateDir)
	if err := store.Save(f); err != nil {
		t.Fatal(err)
	}
	result, err := RunFeatureFinalReviewLoop(OrchestratorConfig{
		Feature: f, FeatureStore: store, StateDir: state.cfg.StateDir,
		CommandRunner: state.cfg.CommandRunner, Model: "agent", ReviewModel: "reviewer",
		MaxIterations: 2, MaxConsecFails: 2,
	}, nil)
	if err != nil || result.FinalStatus != "need_user_input" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	loaded, err := store.Load(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PendingNeedUserInputPath == "" || loaded.PendingNeedUserInputPath != result.NeedUserInputPath {
		t.Fatalf("persisted gate=%q result=%q", loaded.PendingNeedUserInputPath, result.NeedUserInputPath)
	}
}

func TestRunFeatureFinalReviewLoopPersistsContractRepairFeedback(t *testing.T) {
	state, contractPath, _ := newFinalReviewVerificationState(t, "#!/bin/sh\necho ok\n")
	contract, err := ReadTestingContract(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	contract.Items[0].Command = "if then"
	contract.Items[0].Run.Shell = "if then"
	if err := WriteTestingContract(contractPath, *contract); err != nil {
		t.Fatal(err)
	}
	f := state.cfg.Feature
	f.Status = feature.StatusFinalReviewing
	f.CurrentPhase = feature.PhaseReview
	f.CurrentRoadmapPhase = 1
	f.SchemaVersion = feature.SchemaVersionCurrent
	f.RepoStates = map[string]*feature.RepoState{"repo": {Touched: true}}
	store := feature.NewStore(state.cfg.StateDir)
	if err := store.Save(f); err != nil {
		t.Fatal(err)
	}
	result, err := RunFeatureFinalReviewLoop(OrchestratorConfig{
		Feature: f, FeatureStore: store, StateDir: state.cfg.StateDir,
		CommandRunner: state.cfg.CommandRunner, Model: "agent", ReviewModel: "reviewer",
		MaxIterations: 2, MaxConsecFails: 2,
	}, nil)
	if err != nil || result.FinalStatus != "plan_revision_required" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	data, err := os.ReadFile(result.PlanRevisionFeedbackPath)
	if err != nil {
		t.Fatalf("read persisted plan repair feedback: %v", err)
	}
	if string(data) != result.PlanRevisionFeedback || !strings.Contains(string(data), "invalid verification shell syntax") {
		t.Fatalf("persisted feedback=%q result=%q", data, result.PlanRevisionFeedback)
	}
}

func TestFinalReviewContractVerificationStopsOnContractError(t *testing.T) {
	state, contractPath, _ := newFinalReviewVerificationState(t, "#!/bin/sh\necho ok\n")
	contract, err := ReadTestingContract(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	contract.Items[0].Command = "if then"
	contract.Items[0].Run.Shell = "if then"
	if err := WriteTestingContract(contractPath, *contract); err != nil {
		t.Fatal(err)
	}

	_, status, _, err := state.runFinalReviewContractVerification(1, filepath.Join(state.artifactDir, "iteration-01"))
	var terminal *finalReviewVerificationTerminal
	if status != ReviewFailed || !errors.As(err, &terminal) || terminal.status != "plan_revision_required" {
		t.Fatalf("contract error = status=%s terminal=%+v err=%v", status, terminal, err)
	}
	if !strings.Contains(terminal.feedback, "invalid verification shell syntax") {
		t.Fatalf("contract feedback = %q, want invalid command", terminal.feedback)
	}
}

func TestFinalReviewHarnessContractKeepsOnlyExecutableHarnessRows(t *testing.T) {
	contract := CompileTestingContract(strings.Join([]string{
		"#### Automated Verification:",
		"- [ ] Check: `printf ok`",
		"### Visual Evidence",
		"- [ ] Capture the rendered result.",
	}, "\n"), "/tmp/phase-plan.md", "collapsed")
	derived := finalReviewHarnessContract(&contract)
	if derived == nil || len(derived.Items) != 1 {
		t.Fatalf("derived contract items = %+v, want one automated row", derived)
	}
	if derived.Items[0].Owner != TestingContractOwnerHarness || derived.Items[0].Run == nil {
		t.Fatalf("derived item = %+v, want executable harness row", derived.Items[0])
	}
}

func newFinalReviewVerificationState(t *testing.T, script string) (*featureFinalReviewLoopState, string, string) {
	t.Helper()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	stateRoot := filepath.Join(tmp, "state")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewExecCommandRunner()
	for _, command := range []string{
		"git init -q",
		"git config user.email test@example.com",
		"git config user.name Test",
	} {
		runVerificationTestCommand(t, runner, repo, command)
	}
	if err := os.WriteFile(filepath.Join(repo, "verify.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runVerificationTestCommand(t, runner, repo, "git add verify.sh && git commit -qm base")

	f := &feature.Feature{
		ID: "final-review-harness", Name: "Final review harness", ActiveRun: 1,
		Repos: []feature.FeatureRepo{{Name: "repo", Path: repo, WorktreePath: repo}},
	}
	runDir := ActiveRunDir(stateRoot, f)
	contractPath := PhaseTestingContractPath(stateRoot, f, 1)
	if err := os.MkdirAll(filepath.Dir(contractPath), 0o755); err != nil {
		t.Fatal(err)
	}
	contract := compileImplementationTestingContract(ImplementConfig{Feature: f, RepoName: "repo"},
		"#### Automated Verification:\n- [ ] Verify behavior: `./verify.sh`\n")
	if err := EnsureTestingContractBaseCommits(&contract, f.Repos, func(path string) (string, error) {
		return resolveTestingContractWorktreeHEADWithRunner(runner, path)
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteTestingContract(contractPath, contract); err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(runDir, feature.PhaseReview.DirName())
	return &featureFinalReviewLoopState{
		cfg:         OrchestratorConfig{Feature: f, StateDir: stateRoot, CommandRunner: runner},
		workspace:   WorkspaceSetup{Cwd: runDir, RepoPaths: map[string]string{"repo": repo}},
		artifactDir: artifactDir,
	}, contractPath, repo
}
