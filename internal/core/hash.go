package core

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
)

// LocationFingerprint produces a deterministic hash for dedup across runs.
// No LLM output in the hash — fully deterministic.
func LocationFingerprint(repoFullName, filePath, symbol string, category FindingCategory) string {
	h := sha256.New()
	io.WriteString(h, repoFullName)
	io.WriteString(h, "\x00")
	io.WriteString(h, filePath)
	io.WriteString(h, "\x00")
	io.WriteString(h, symbol)
	io.WriteString(h, "\x00")
	io.WriteString(h, string(category))
	return hex.EncodeToString(h.Sum(nil))
}

// ContentFingerprint produces a hash of the AST subtree for the flagged code region.
// Returns empty string if the AST is unavailable.
func ContentFingerprint(astSubtree []byte) string {
	if len(astSubtree) == 0 {
		return ""
	}
	h := sha256.Sum256(astSubtree)
	return hex.EncodeToString(h[:])
}

// NewTokenBudget computes per-section token caps for a given total budget,
// task type, and index precision. The returned caps follow the allocation
// strategy in slice-budgeting.md:
//
//   - Default split: 60% diff, 25% call graph, 15% tests
//   - Approximate index: 70% diff, 17% call graph, 13% tests
//   - Per-task-type overrides adjust the default before applying
//
// These are caps, not allocations. The runtime slice builder fills in
// priority order (diff -> call graph -> tests) and rolls unused budget
// from later slots into diff.
func NewTokenBudget(totalTokens int, taskType TaskType, approximate bool) TokenBudget {
	var diffBP, callGraphBP, testsBP int

	if approximate {
		diffBP, callGraphBP, testsBP = 7000, 1700, 1300
	} else {
		diffBP, callGraphBP, testsBP = 6000, 2500, 1500
	}

	switch taskType {
	case TaskTypeSecurity:
		diffBP -= 1000
		callGraphBP += 1000
	case TaskTypeTestCoverage:
		callGraphBP -= 2000
		testsBP += 2000
	case TaskTypeStyle:
		diffBP = 10000
		callGraphBP = 0
		testsBP = 0
	case TaskTypeLogic:
		// Default split, no adjustment.
	}

	return TokenBudget{
		DiffHunk:  totalTokens * diffBP / 10000,
		CallGraph: totalTokens * callGraphBP / 10000,
		Tests:     totalTokens * testsBP / 10000,
		Total:     totalTokens,
	}
}
