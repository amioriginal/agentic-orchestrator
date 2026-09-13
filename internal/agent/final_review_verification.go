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
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/doordash-oss/agentic-orchestrator/internal/feature"
)

type finalReviewVerificationCacheEntry struct {
	report  VerificationReport
	outcome VerificationExecutionOutcome
}

type finalReviewVerificationTerminal struct {
	status    string
	feedback  string
	inputPath string
}

func (e *finalReviewVerificationTerminal) Error() string {
	if e == nil {
		return "final review verification stopped"
	}
	return strings.TrimSpace(e.feedback)
}

// runFinalReviewContractVerification executes the plan-owned automated
// contracts with Agentico's deterministic harness before LLM review. Exact
// candidate+contract repeats within this process reuse the harness result;
// process restarts deliberately re-execute until a runtime fingerprint is
// available for safe durable reuse.
func (s *featureFinalReviewLoopState) runFinalReviewContractVerification(iteration int, iterDir string) (priorImplementationEvidenceContext, ReviewStatus, string, error) {
	var evidence priorImplementationEvidenceContext
	contractPaths, err := finalReviewTestingContractPaths(s.cfg.StateDir, s.cfg.Feature)
	if err != nil || len(contractPaths) == 0 {
		return evidence, ReviewApproved, "", err
	}
	if s.cfg.CommandRunner == nil {
		// Contracts containing only agent-owned manual/visual evidence remain
		// review inputs and need no command runner.
		for _, path := range contractPaths {
			contract, readErr := ReadTestingContract(path)
			if readErr != nil {
				return evidence, ReviewFailed, "", fmt.Errorf("read final review testing contract %s: %w", path, readErr)
			}
			if finalReviewContractHasHarnessItems(contract) {
				return evidence, ReviewFailed, "", fmt.Errorf("final review contract verification: command runner is required")
			}
		}
		return evidence, ReviewApproved, "", nil
	}
	type finalReviewContract struct {
		path     string
		contract *TestingContract
	}
	eligible := make([]finalReviewContract, 0, len(contractPaths))
	for _, path := range contractPaths {
		contract, readErr := ReadTestingContract(path)
		if readErr != nil {
			return evidence, ReviewFailed, "", fmt.Errorf("read final review testing contract %s: %w", path, readErr)
		}
		if finalReviewContractHasHarnessItems(contract) {
			eligible = append(eligible, finalReviewContract{path: path, contract: contract})
		}
	}
	if len(eligible) == 0 {
		return evidence, ReviewApproved, "", nil
	}

	candidate := verificationCandidateFingerprint(context.Background(), s.cfg.CommandRunner, s.workspace.Cwd, s.cfg.Feature.Repos)
	if candidate == "" {
		return evidence, ReviewFailed, "", fmt.Errorf("final review contract verification: current candidate fingerprint is unavailable")
	}
	if s.verificationCache == nil {
		s.verificationCache = make(map[string]finalReviewVerificationCacheEntry)
	}

	for _, entry := range eligible {
		contractPath := entry.path
		contract := entry.contract
		phaseName := filepath.Base(filepath.Dir(contractPath))
		harnessDir := filepath.Join(iterDir, "harness-verification", phaseName)
		if mkErr := os.MkdirAll(harnessDir, 0o755); mkErr != nil {
			return evidence, ReviewFailed, "", fmt.Errorf("create final review harness directory: %w", mkErr)
		}
		reportPath := filepath.Join(harnessDir, "verification-report.yaml")
		executionContract := finalReviewHarnessContract(contract)
		contractBytes, marshalErr := json.Marshal(executionContract)
		if marshalErr != nil {
			return evidence, ReviewFailed, "", fmt.Errorf("fingerprint final review testing contract %s: %w", contractPath, marshalErr)
		}
		contractFingerprint := fmt.Sprintf("%x", sha256.Sum256(contractBytes))
		executionContractPath := filepath.Join(harnessDir, "testing-contract.yaml")
		if writeErr := WriteTestingContract(executionContractPath, *executionContract); writeErr != nil {
			return evidence, ReviewFailed, "", fmt.Errorf("write final review harness contract: %w", writeErr)
		}
		cacheKey := fmt.Sprintf("%s\x00%d\x00%s\x00%s", contractPath, contract.Revision, contractFingerprint, candidate)

		var outcome *VerificationExecutionOutcome
		if cached, ok := s.verificationCache[cacheKey]; ok {
			reportCopy := cached.report
			reportCopy.ContractPath = executionContractPath
			outcomeCopy := cached.outcome
			outcomeCopy.Report = &reportCopy
			outcome = &outcomeCopy
		} else {
			report := BuildContractVerificationReportStub(executionContract, executionContractPath)
			if baselineErr := RecordReadOnlyRepoBaseline(context.Background(), s.cfg.CommandRunner, s.cfg.Feature, harnessDir); baselineErr != nil {
				return evidence, ReviewFailed, "", fmt.Errorf("record final review harness repo baseline: %w", baselineErr)
			}
			executed, executeErr := ExecuteTestingContract(context.Background(), s.cfg.CommandRunner, executionContract, &report,
				executionContractPath, harnessDir, s.workspace.Cwd, s.cfg.Feature.Repos)
			mutations, mutationErr := EnforceReadOnlyRepoMutations(context.Background(), s.cfg.CommandRunner, s.cfg.Feature, feature.PhaseFinalReview, harnessDir)
			if mutationErr != nil {
				return evidence, ReviewFailed, "", fmt.Errorf("restore final review harness repo baseline: %w", mutationErr)
			}
			if len(mutations) > 0 {
				return evidence, ReviewFailed, "", newProtocolViolationError(RoleImplementationReviewQA, harnessDir, mutations)
			}
			if executeErr != nil {
				return evidence, ReviewFailed, "", fmt.Errorf("execute final review testing contract %s: %w", contractPath, executeErr)
			}
			outcome = executed
			if after := verificationCandidateFingerprint(context.Background(), s.cfg.CommandRunner, s.workspace.Cwd, s.cfg.Feature.Repos); after != candidate {
				return evidence, ReviewFailed, "", fmt.Errorf("final review testing contract changed candidate state: before=%s after=%s", candidate, after)
			}
			if outcome.Report == nil || outcome.Report.CandidateFingerprint != candidate {
				return evidence, ReviewFailed, "", fmt.Errorf("final review harness report is not bound to the tested candidate")
			}
			s.verificationCache[cacheKey] = finalReviewVerificationCacheEntry{report: *outcome.Report, outcome: *outcome}
		}

		if len(outcome.ContractErrors) > 0 {
			return evidence, ReviewFailed, "", &finalReviewVerificationTerminal{
				status:   "plan_revision_required",
				feedback: VerificationContractPlanRevisionFeedback(outcome.ContractErrors),
			}
		}
		if len(outcome.BlockedItems) > 0 {
			gatePath := NeedUserInputPath(harnessDir)
			record := SynthesizeVerificationNeedUserInputGateWithContext(
				executionContractPath, executionContract, outcome.Report, outcome.BlockedItems, iteration,
			)
			if writeErr := WriteNeedUserInputRecord(gatePath, record); writeErr != nil {
				return evidence, ReviewFailed, "", fmt.Errorf("write final review verification input gate: %w", writeErr)
			}
			return evidence, ReviewFailed, "", &finalReviewVerificationTerminal{
				status:    "need_user_input",
				feedback:  record.Summary,
				inputPath: gatePath,
			}
		}

		gate := ValidateVerificationReportWithContext(outcome.Report, nil, true, VerificationReportValidationContext{
			IterationDir:                 harnessDir,
			Contract:                     executionContract,
			ExpectedCandidateFingerprint: candidate,
		})
		if gate.Rejected {
			return evidence, ReviewChangesRequested, FormatGateFeedback(gate), nil
		}
		if len(outcome.RegressionItems) > 0 {
			feedback := finalReviewHarnessFailureFeedback(outcome)
			return evidence, ReviewChangesRequested, feedback, nil
		}
		if writeErr := WriteVerificationReport(reportPath, *outcome.Report); writeErr != nil {
			return evidence, ReviewFailed, "", fmt.Errorf("write final review verification report: %w", writeErr)
		}
		evidence.ReportPaths = append(evidence.ReportPaths, reportPath)
		evidence.EvidenceRootDirs = append(evidence.EvidenceRootDirs, harnessDir)
		evidence.EvidenceArtifactPaths = appendEvidenceArtifactPaths(evidence.EvidenceArtifactPaths, reportPath)
	}
	return evidence, ReviewApproved, "", nil
}

func finalReviewTestingContractPaths(stateRoot string, f *feature.Feature) ([]string, error) {
	if f == nil {
		return nil, nil
	}
	runDir := ActiveRunDir(stateRoot, f)
	entries, err := os.ReadDir(runDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "phase-") {
			continue
		}
		path := filepath.Join(runDir, entry.Name(), "testing-contract.yaml")
		if _, statErr := os.Stat(path); statErr == nil {
			paths = append(paths, path)
		} else if !os.IsNotExist(statErr) {
			return nil, statErr
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func finalReviewContractHasHarnessItems(contract *TestingContract) bool {
	if contract == nil {
		return false
	}
	for _, item := range contract.Items {
		if !IsTestingContractItemWaived(item) && item.Owner == TestingContractOwnerHarness && item.Run != nil && strings.TrimSpace(item.Run.Shell) != "" {
			return true
		}
	}
	return false
}

// finalReviewHarnessContract derives the executable subset from the original
// phase contract. Agent-owned visual/manual rows remain covered by their
// implementation evidence and the LLM evidence axis; the deterministic final
// gate owns only commands it can actually execute.
func finalReviewHarnessContract(contract *TestingContract) *TestingContract {
	if contract == nil {
		return nil
	}
	derived := *contract
	derived.Items = nil
	for _, item := range contract.Items {
		if IsTestingContractItemWaived(item) || item.Owner != TestingContractOwnerHarness || item.Run == nil || strings.TrimSpace(item.Run.Shell) == "" {
			continue
		}
		derived.Items = append(derived.Items, item)
	}
	return &derived
}

func finalReviewHarnessFailureFeedback(outcome *VerificationExecutionOutcome) string {
	var findings []string
	if outcome != nil {
		for _, item := range outcome.RegressionItems {
			findings = append(findings, fmt.Sprintf("- **High**: harness regression in `%s`", item))
		}
		for _, item := range outcome.BlockedItems {
			findings = append(findings, fmt.Sprintf("- **High**: harness verification blocked for `%s`", item))
		}
	}
	if len(findings) == 0 {
		findings = append(findings, "- **High**: harness verification did not produce an acceptable result")
	}
	return FormatStructuredReviewFeedback("Harness Final Verification", strings.Join(findings, "\n"), "- Re-run only after the named condition changes.", ReviewChangesRequested)
}
