package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocationFingerprint(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		h1 := LocationFingerprint("ashita-ai/mimir", "internal/core/types.go", "FindingValidate", CategorySecurity)
		h2 := LocationFingerprint("ashita-ai/mimir", "internal/core/types.go", "FindingValidate", CategorySecurity)
		assert.Equal(t, h1, h2)
	})

	t.Run("hex encoded sha256", func(t *testing.T) {
		h := LocationFingerprint("repo", "file.go", "Fn", CategoryLogic)
		assert.Len(t, h, 64, "sha256 hex digest is 64 chars")
	})

	t.Run("different inputs produce different hashes", func(t *testing.T) {
		base := LocationFingerprint("repo", "file.go", "Fn", CategoryLogic)

		assert.NotEqual(t, base, LocationFingerprint("other", "file.go", "Fn", CategoryLogic), "different repo")
		assert.NotEqual(t, base, LocationFingerprint("repo", "other.go", "Fn", CategoryLogic), "different file")
		assert.NotEqual(t, base, LocationFingerprint("repo", "file.go", "Other", CategoryLogic), "different symbol")
		assert.NotEqual(t, base, LocationFingerprint("repo", "file.go", "Fn", CategorySecurity), "different category")
	})

	t.Run("null byte separator prevents collisions", func(t *testing.T) {
		h1 := LocationFingerprint("ab", "cd", "e", CategoryLogic)
		h2 := LocationFingerprint("a", "bcd", "e", CategoryLogic)
		assert.NotEqual(t, h1, h2)
	})

	t.Run("empty symbol is valid", func(t *testing.T) {
		h := LocationFingerprint("repo", "file.go", "", CategoryStyle)
		assert.Len(t, h, 64)
	})
}

func TestContentFingerprint(t *testing.T) {
	t.Run("empty input returns empty string", func(t *testing.T) {
		assert.Equal(t, "", ContentFingerprint(nil))
		assert.Equal(t, "", ContentFingerprint([]byte{}))
	})

	t.Run("deterministic", func(t *testing.T) {
		data := []byte("func Foo() { return 42 }")
		h1 := ContentFingerprint(data)
		h2 := ContentFingerprint(data)
		assert.Equal(t, h1, h2)
	})

	t.Run("hex encoded sha256", func(t *testing.T) {
		h := ContentFingerprint([]byte("some ast"))
		assert.Len(t, h, 64)
	})

	t.Run("different inputs produce different hashes", func(t *testing.T) {
		h1 := ContentFingerprint([]byte("func Foo()"))
		h2 := ContentFingerprint([]byte("func Bar()"))
		assert.NotEqual(t, h1, h2)
	})
}

func TestNewTokenBudget(t *testing.T) {
	const total = 16000

	t.Run("default logic split", func(t *testing.T) {
		b := NewTokenBudget(total, TaskTypeLogic, false)
		assert.Equal(t, 9600, b.DiffHunk)
		assert.Equal(t, 4000, b.CallGraph)
		assert.Equal(t, 2400, b.Tests)
		assert.Equal(t, total, b.Total)
	})

	t.Run("approximate index shifts toward diff", func(t *testing.T) {
		b := NewTokenBudget(total, TaskTypeLogic, true)
		assert.Equal(t, 11200, b.DiffHunk)
		assert.Equal(t, 2720, b.CallGraph)
		assert.Equal(t, 2080, b.Tests)
		assert.Equal(t, total, b.Total)
	})

	t.Run("security adds call graph budget", func(t *testing.T) {
		b := NewTokenBudget(total, TaskTypeSecurity, false)
		assert.Equal(t, 8000, b.DiffHunk)
		assert.Equal(t, 5600, b.CallGraph)
		assert.Equal(t, 2400, b.Tests)
		assert.Equal(t, total, b.Total)
	})

	t.Run("test_coverage adds test budget", func(t *testing.T) {
		b := NewTokenBudget(total, TaskTypeTestCoverage, false)
		assert.Equal(t, 9600, b.DiffHunk)
		assert.Equal(t, 800, b.CallGraph)
		assert.Equal(t, 5600, b.Tests)
		assert.Equal(t, total, b.Total)
	})

	t.Run("style is diff only", func(t *testing.T) {
		b := NewTokenBudget(total, TaskTypeStyle, false)
		assert.Equal(t, total, b.DiffHunk)
		assert.Equal(t, 0, b.CallGraph)
		assert.Equal(t, 0, b.Tests)
		assert.Equal(t, total, b.Total)
	})

	t.Run("zero budget produces zero caps", func(t *testing.T) {
		b := NewTokenBudget(0, TaskTypeLogic, false)
		assert.Equal(t, 0, b.Total)
	})
}

func TestValidateConfidenceTier(t *testing.T) {
	t.Run("high tier valid", func(t *testing.T) {
		require.NoError(t, ValidateConfidenceTier(ConfidenceHigh, 0.80))
		require.NoError(t, ValidateConfidenceTier(ConfidenceHigh, 0.95))
		require.NoError(t, ValidateConfidenceTier(ConfidenceHigh, 1.0))
	})

	t.Run("high tier invalid", func(t *testing.T) {
		require.Error(t, ValidateConfidenceTier(ConfidenceHigh, 0.79))
		require.Error(t, ValidateConfidenceTier(ConfidenceHigh, 0.0))
	})

	t.Run("medium tier valid", func(t *testing.T) {
		require.NoError(t, ValidateConfidenceTier(ConfidenceMedium, 0.50))
		require.NoError(t, ValidateConfidenceTier(ConfidenceMedium, 0.79))
	})

	t.Run("medium tier invalid", func(t *testing.T) {
		require.Error(t, ValidateConfidenceTier(ConfidenceMedium, 0.49))
		require.Error(t, ValidateConfidenceTier(ConfidenceMedium, 0.80))
	})

	t.Run("low tier valid", func(t *testing.T) {
		require.NoError(t, ValidateConfidenceTier(ConfidenceLow, 0.0))
		require.NoError(t, ValidateConfidenceTier(ConfidenceLow, 0.49))
	})

	t.Run("low tier invalid", func(t *testing.T) {
		require.Error(t, ValidateConfidenceTier(ConfidenceLow, 0.50))
	})

	t.Run("unknown tier", func(t *testing.T) {
		require.Error(t, ValidateConfidenceTier("bogus", 0.5))
	})
}
