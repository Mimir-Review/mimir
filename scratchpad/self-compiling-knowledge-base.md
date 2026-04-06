# Self-Compiling Knowledge Base

> **Status:** Discussion draft
> **Date:** 2026-04-06
> **Scope:** M2/M3 (builds on M1 data capture)

---

## The Problem

Mimir's pipeline is memoryless. Every review starts from scratch: parse the diff, build a symbol table, generate tasks, run inference, throw everything away except the findings. The semantic index is rebuilt and discarded each run. The eval layer captures reactions but nothing consumes them. After reviewing 500 PRs for a repo, Mimir knows exactly as much about the codebase as it did on PR #1.

This is the open-loop architecture described in the M1 specs. It was the right choice for M1 — get the pipeline working before making it adaptive. But it means:

- **Risk scoring is generic.** The planner uses static path-pattern heuristics (`auth` -> 0.4, `handler` -> 0.25). It doesn't know that `internal/billing/reconcile.go` has produced 8 confirmed findings in the last month while `internal/auth/handler.go` has been stable for a year.
- **Context assembly is uninformed.** The slice builder allocates tokens by fixed ratios (60/25/15). It doesn't know that for this specific repo, findings backed by test context are confirmed at 2x the rate of findings without.
- **The model has no institutional knowledge.** Each review task gets a diff, callers, and tests — but no sense of the repo's patterns, conventions, or history. A human reviewer who's been on the team for six months carries that context. Mimir never accumulates it.
- **Confidence thresholds are static.** The 0.80/0.50 tier boundaries were design intuition. They don't adapt when a particular model consistently over- or under-estimates confidence for a given repo.

## The Idea

Replace the open-loop architecture with a **self-compiling knowledge base** per repo. After each review, Mimir compiles what it learned into a persistent, structured knowledge base. On the next review, that knowledge is loaded as context alongside the fresh semantic index. The knowledge compounds with every review cycle.

The concept comes from Karpathy's LLM knowledge base pattern: raw data is ingested, an LLM compiles it into a structured markdown wiki, and the wiki is then used as context for future queries. The compilation step is the key — it forces synthesis, not just accumulation. The LLM doesn't see 500 raw findings; it sees a distilled understanding.

Applied to Mimir: the "raw data" is pipeline run output (findings, reactions, tasks, semantic index). The "wiki" is a per-repo knowledge base. The "queries" are review tasks. The "compilation" is a background job that runs after each review.

---

## What the Knowledge Base Contains

Six entry types, each serving a distinct role in the pipeline. All stored as structured text (markdown bodies with typed metadata), compiled by an LLM from raw pipeline data.

### 1. Repo Profile

A compressed overview (~2-3K tokens) loaded on every review. This is the L1 tier — always present, always cheap.

Contents:
- Language(s), framework(s), build system
- High-level package structure and dependency patterns
- Team conventions derived from code and review history (e.g., "uses table-driven tests," "wraps errors at boundaries," "prefers short functions")
- Summary statistics: total PRs reviewed, overall precision, top finding categories
- Known hot spots (top 5 files by confirmed finding density)

This is what a human reviewer would internalize after a month on the team. It gives the model the same ambient understanding.

### 2. Area Profiles

Per-package or per-directory entries (~500-1K tokens each), loaded per-task when the task touches that area. This is L2 — loaded on demand, scoped to relevance.

Contents:
- Package purpose and public API surface
- Historical finding density and types
- Team-specific patterns in this area (e.g., "this package uses a custom error type, not stdlib errors")
- Known coupling: which other packages change when this one changes
- Recent activity: last N PRs that touched this area, and what was found

### 3. Hot Spot Registry

A structured list of files/functions with disproportionate finding density or change frequency. Used by the planner to adjust risk scores.

Fields per entry:
- File path + symbol (if function-level)
- Finding density: confirmed findings per PR touching this file
- FP rate: thumbs-down rate for findings on this file
- Last reviewed: pipeline run ID and date
- Trend: improving, stable, or degrading (based on trailing 5 PR window)

### 4. Review Calibration

Per-(model, task_type) precision and recall estimates for this specific repo. Used by the policy layer for confidence calibration.

Fields:
- Model ID, task type
- Sample size (reacted findings)
- Empirical precision: thumbs_up / (thumbs_up + thumbs_down)
- Confidence distribution: where do confirmed vs. rejected findings cluster?
- Recommended threshold adjustment (computed, not hand-tuned)

### 5. Convention Guide

Extracted coding patterns and style preferences for this repo. Injected into review prompts so the model doesn't flag intentional patterns as issues.

Examples:
- "This repo uses `fmt.Errorf` wrapping with `%w` at package boundaries, not sentinel errors"
- "Test files use a `testdata/` directory for golden files, not inline expected strings"
- "Handler functions return early on validation failure; deep nesting is intentional in `processOrder`"

Sources: code patterns (tree-sitter analysis across multiple runs), confirmed findings (what the team upvotes = what they value), dismissed findings (what the team rejects = what they consider noise).

### 6. Recurring Patterns

Issues that reappear across PRs. Unlike dedup (which suppresses within a PR), this tracks patterns across PRs and across authors.

Examples:
- "Missing context propagation in new handler functions (3 confirmed findings in last 10 PRs, always by different authors)"
- "SQL injection risk when building dynamic queries in the reporting package (flagged 4 times, 2 confirmed, 2 FP)"

This is high-value context for the model: "this codebase has a recurring problem with X" is a strong prior.

---

## How Compilation Works

### Trigger

A new River job type: `CompileKnowledgeJob`. Enqueued automatically after a pipeline run completes (status = `completed`, not `failed`). Runs at lower priority than review jobs — knowledge compilation should never delay a review.

### Input

The compilation job reads:
1. The just-completed pipeline run: all findings, task results, slice metadata
2. All reaction events for this repo within the trailing window (default 90 days)
3. The current knowledge base entries for this repo (if any)
4. The semantic index summary from the just-completed run (not the full index — just the package structure and symbol counts)

### Process

```
                                    +----------------------+
                                    |   Current Knowledge  |
                                    |    Base (if exists)   |
                                    +----------+-----------+
                                               |
+-------------+     +--------------+           |     +------------------+
|  Pipeline   |---->|  Compile     |-----------+---->|  Updated         |
|  Run Output |     |  Knowledge   |                 |  Knowledge Base  |
+-------------+     |  Job (LLM)  |                 +------------------+
                    +------+-------+
                           |
                    +------+-------+
                    |  Reaction    |
                    |  History     |
                    +--------------+
```

The compilation is an LLM call. The model receives:
- The current knowledge base (or a bootstrap prompt if this is the first run)
- A structured summary of the new pipeline run
- Recent reaction data
- Instructions: "Update the knowledge base with what you learned from this run. Add new entries, revise existing ones, mark stale ones for removal."

The model returns updated entries in a structured format. The job parses the output and writes it to the database.

### Incremental, Not Full Rewrite

Each compilation is a delta operation. The model sees the existing KB and produces changes — not a fresh rewrite. This is critical for:
- **Token efficiency:** The model only processes the delta, not the entire history.
- **Stability:** Existing knowledge isn't lost when a single unusual run introduces noise.
- **Auditability:** Each compilation is tied to a pipeline run, so you can trace why an entry changed.

### Bootstrap

On the first review of a repo, there's no knowledge base. The compilation job runs with a bootstrap prompt that produces the initial repo profile from the first pipeline run's data. Early entries are flagged as `low_confidence` (small sample size) and naturally upgrade as more data accumulates.

---

## How the Pipeline Consumes Knowledge

### Planner: Risk Scoring

Current: `computeRiskScore` uses static path patterns and symbol characteristics.

With KB: The planner queries the hot spot registry before scoring. If the knowledge base has data for this file/symbol, the historical finding density becomes an input to the risk score.

```go
func computeRiskScore(sym core.Symbol, filePath string, hasTests bool, changedLineCount int, kb *KnowledgeBase) core.RiskScore {
    score := 0.0

    // Existing heuristics (unchanged)
    score += pathRisk(filePath)
    // ... existing logic ...

    // Knowledge-based adjustment
    if kb != nil {
        if hotSpot := kb.HotSpot(filePath, sym.Name); hotSpot != nil {
            // Historical finding density directly influences risk
            score += hotSpot.FindingDensity * 0.3 // capped contribution
        }
    }

    return core.RiskScore(min(score, 1.0))
}
```

Graceful degradation: `kb == nil` means no knowledge base exists for this repo. All existing behavior is unchanged.

### Runtime: Slice Building

Current: Slices contain diff hunk + call graph + test context.

With KB: The slice gets a fourth section — **knowledge context** — loaded from the area profile for the file being reviewed.

```
[System prompt]
[Task framing]
[Knowledge context]          <-- NEW: area profile + relevant conventions
[Approximate context note]
[Slice: diff hunk]
[Slice: call graph]
[Slice: test context]
[Truncation notice]
[Output instructions]
```

The knowledge context section is budget-capped like the others. Default: 10% of total budget. The cap-not-allocation model means unused knowledge budget rolls to the diff, just like call graph and test context.

| Section | Normal | With KB |
|---------|--------|---------|
| Diff hunk | 60% | 55% |
| Knowledge context | 0% | 10% |
| Call graph | 25% | 22% |
| Test context | 15% | 13% |

The knowledge context includes:
- The area profile for the file's package (if it exists)
- Relevant convention entries (filtered by language and package)
- Relevant recurring patterns (if the file is in a known problem area)

### Policy: Confidence Calibration

Current: Static 0.80/0.50 tier boundaries. Fixed 0.85x penalty for approximate index.

With KB: The review calibration entry provides empirical precision for each (model, task_type) pair in this repo. The policy layer applies a calibration adjustment before triage.

```go
func calibrate(f *core.Finding, kb *KnowledgeBase) {
    if kb == nil {
        return
    }
    cal := kb.ReviewCalibration(f.ModelID, f.TaskType)
    if cal == nil || cal.SampleSize < 20 {
        return // insufficient data
    }

    // Adjust confidence based on historical precision.
    // If the model historically overestimates (precision < expected),
    // pull confidence down. If it underestimates, pull it up.
    factor := cal.EmpiricalPrecision / 0.80 // 0.80 = expected precision
    factor = clamp(factor, 0.5, 1.5)        // guard rail

    f.ConfidenceScore = roundConfidence(f.ConfidenceScore * factor)
    f.ConfidenceTier = tierFromScore(f.ConfidenceScore)
    f.CalibrationFactor = &factor
    f.CalibrationSampleSize = &cal.SampleSize
}
```

### Prompts: Repo-Specific Instructions

Current: The review prompt is generic. Every repo gets the same system prompt + task framing.

With KB: The repo profile and convention guide are injected into the system prompt. The model knows:
- What the repo does and how it's structured
- What conventions the team follows (so it doesn't flag intentional patterns)
- What recurring issues to watch for (strong priors for detection)

This is the highest-leverage integration point. A model that knows "this team wraps errors at boundaries using `fmt.Errorf` with `%w`" will not waste a finding on `return err` in an internal helper. A model that knows "this codebase has a recurring SQL injection pattern in the reporting package" will be more attentive to query building in that area.

---

## Storage Design

### New Table: `repo_knowledge`

```sql
CREATE TABLE repo_knowledge (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id         UUID NOT NULL REFERENCES repositories(id) ON DELETE RESTRICT,
    entry_type      TEXT NOT NULL,
    -- entry_type values: 'repo_profile', 'area_profile', 'hot_spot',
    --   'calibration', 'convention', 'recurring_pattern'
    path            TEXT NOT NULL,
    -- virtual path for organization (e.g., 'internal/auth', 'global')
    content         TEXT NOT NULL,  -- markdown body, LLM-readable
    metadata        JSONB NOT NULL DEFAULT '{}',
    compiled_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    pipeline_run_id UUID REFERENCES pipeline_runs(id) ON DELETE RESTRICT,
    superseded_at   TIMESTAMPTZ,   -- soft supersede: non-null = replaced
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_repo_knowledge_active
    ON repo_knowledge(repo_id, entry_type)
    WHERE superseded_at IS NULL;

CREATE INDEX idx_repo_knowledge_path
    ON repo_knowledge(repo_id, path)
    WHERE superseded_at IS NULL;
```

Key properties:
- **Soft supersede, not update-in-place.** When compilation produces an updated entry, the old one gets `superseded_at` set and a new row is inserted. The full history of how the KB evolved is preserved.
- **`ON DELETE RESTRICT`** on `repo_id` and `pipeline_run_id`. Consistent with the existing durability invariants.
- **`pipeline_run_id`** links every KB entry to the run that triggered its compilation. Full provenance.

### Why Markdown, Not JSONB

The knowledge base content is markdown text, not structured JSONB. Reasons:

1. **LLMs read and write markdown natively.** The compilation step outputs markdown. The pipeline loads markdown into prompts. Markdown is the LLM's native format — any serialization boundary (JSON <-> natural language) is a lossy translation.
2. **Human-readable.** Engineers can read the knowledge base directly. No tooling required. `SELECT content FROM repo_knowledge WHERE repo_id = $1 AND entry_type = 'repo_profile' AND superseded_at IS NULL` returns something a human can understand.
3. **The structured parts live in `metadata` JSONB.** Hot spot scores, calibration numbers, sample sizes — anything the pipeline queries programmatically — lives in the metadata column. The markdown body is for the LLM and for humans.

---

## The Linting Loop

Karpathy's "health checks" map to a periodic maintenance job that validates and improves the knowledge base.

### `KnowledgeLintJob` (Periodic River Job)

Runs on a schedule (default: weekly, or after every N pipeline runs, whichever comes first). Not tied to any specific PR.

**Checks:**

1. **Staleness.** Query the repo's current file list (from the most recent pipeline run's semantic index summary). Flag KB entries whose `path` references files/packages that no longer exist.

2. **Contradictions.** Load all active entries for a repo and ask the LLM: "Do any of these entries contradict each other?" This catches drift — e.g., an area profile that says "this package uses sentinel errors" when a more recent convention entry says "wraps with `%w`."

3. **Coverage gaps.** Compare the hot spot registry against the actual package list. Identify active areas of the codebase with no area profile. Generate stubs for the compilation job to fill on the next run.

4. **Anchor validation.** Cross-reference the hot spot registry's finding density scores against current reaction data. If a "hot spot" hasn't produced a confirmed finding in 60 days, demote it.

5. **Connection discovery.** Ask the LLM to identify patterns across entries — recurring issues that span packages, coupling relationships that aren't captured in the area profiles, convention violations that multiple area profiles document independently.

**Output:** Updated/new/superseded entries, written through the same compilation path. Each change is tied to a `pipeline_run_id` (the most recent one at lint time) for provenance.

---

## Three-Tier Loading

The knowledge base can grow large for active repos. The pipeline doesn't load everything — it uses a tiered loading strategy matched to the token budget.

| Tier | What | When Loaded | Budget |
|------|------|-------------|--------|
| L1: Repo Profile | Compressed overview, top conventions, top hot spots | Every review, every task | ~2-3K tokens (fixed, outside per-task budget) |
| L2: Area Profile | Per-package detail, local conventions, local patterns | Per task, when task touches that package | From the 10% knowledge budget in the slice |
| L3: Full History | Raw finding history, reaction timelines, compilation history | On demand, for eval/debugging only | Not loaded during reviews |

The system prompt gets L1 (repo profile). The per-task slice gets L2 (area profile). L3 is never in the review path — it's for the compilation job, the linting job, and human queries.

This mirrors the article's finding: "three-tier loading (abstracts -> overviews -> full content on demand) achieved 83% fewer tokens AND better results." The repo profile is the abstract. Area profiles are overviews. Full history is the content you load only when you need it.

---

## The Compounding Loop

Here's the full cycle:

```
+-------------------------------------------------------------+
|                                                             |
|   +----------+    +----------+    +----------+              |
|   |  Index   |--->|  Planner |--->|  Runtime |              |
|   |          |    | (KB:risk)|    | (KB:ctx) |              |
|   +----------+    +----------+    +----+-----+              |
|        ^                               |                    |
|        |                               v                    |
|   +----+-----+    +----------+    +----------+              |
|   |  Lint    |<---|  Compile |<---|  Policy  |              |
|   |  Job     |    |  Job     |    | (KB:cal) |              |
|   +----------+    +----+-----+    +----+-----+              |
|                        |               |                    |
|                        v               v                    |
|                   +----------+    +----------+              |
|                   | Knowledge|    | Findings |              |
|                   |   Base   |    | + Events |              |
|                   +----------+    +----------+              |
|                        ^               |                    |
|                        |               v                    |
|                        |          +----------+              |
|                        +----------|Reactions |              |
|                                   | (GitHub) |              |
|                                   +----------+              |
|                                                             |
+-------------------------------------------------------------+
```

Each review produces findings. Findings get reactions. Reactions feed into compilation. Compilation updates the knowledge base. The next review uses the knowledge base, producing better findings, which get better reactions, which improve the knowledge base.

The lint job is the maintenance loop: it validates existing knowledge against fresh data and patches gaps. Without it, the KB would accumulate stale entries and contradictions. With it, knowledge quality compounds alongside knowledge quantity.

---

## Failure Modes and Mitigations

### Silent Poisoning

**Risk:** The LLM hallucinates during compilation. A fabricated pattern or convention enters the KB and persists because no validation gate catches it.

**Mitigations:**
- Every KB entry tracks provenance (`pipeline_run_id`). You can always answer "why does the KB say this?" by examining the findings and reactions from the source run.
- The linting job checks for contradictions and staleness. A hallucinated entry that contradicts actual code patterns will be flagged.
- KB entries carry confidence levels in metadata. Entries derived from small samples start at low confidence and only upgrade with corroborating evidence.
- The supersede-don't-delete model means you can always revert to a prior version of any entry.

### Staleness

**Risk:** Code evolves (refactors, renames, new packages), but KB entries reference the old structure.

**Mitigations:**
- The linting job's staleness check cross-references KB paths against the actual file tree.
- Each entry tracks `compiled_at`. Entries older than a configurable threshold (default: 90 days without recompilation) are flagged for review.
- Area profiles that reference deleted packages are automatically superseded.

### Context Bloat

**Risk:** For very active repos, the KB grows large enough to consume excessive token budget.

**Mitigations:**
- Three-tier loading ensures only L1 (fixed ~2-3K) and L2 (budgeted 10% of slice) are in the review path.
- The compilation step itself is responsible for compression. The LLM is instructed to keep entries concise and to merge related observations rather than accumulating them.
- Hard cap: if the repo profile exceeds 3K tokens, the compilation job must compress it. If an area profile exceeds 1K tokens, same.

### Reward Hacking

**Risk:** The KB optimizes for metrics that diverge from actual review quality. If calibration pushes the system toward "fewer thumbs-down" rather than "more true positives," it converges on safe, obvious, low-value findings.

**Mitigations:**
- Multiple signal types, not just reactions. The KB also tracks: did the finding lead to a code change? (`addressed_status`), did the finding survive the full PR lifecycle? (not dismissed), what was the finding's category and severity distribution?
- The `addressed_in_next_commit` signal (M2) is a stronger proxy than reactions: if the author changed the code in response to the finding, it was probably useful. This is independent of whether they clicked thumbs-up.
- The linting job can compute "finding value" metrics that balance precision against coverage: a system that produces zero findings has perfect precision but zero value.
- Calibration multipliers are clamped to [0.5, 1.5] — the system can't fully suppress or fully inflate any model's scores.

### Compilation Cost

**Risk:** Each compilation is an LLM call. For active repos with many PRs/day, this could become expensive.

**Mitigations:**
- Compilation runs at lower priority than reviews. It can be batched: if 3 reviews complete before the compilation job runs, it processes all 3 in one call.
- The compilation prompt is bounded: existing KB + structured summary of new data. It doesn't load raw findings — just aggregated stats and notable items (new confirmed findings, new FPs, new file areas).
- For low-activity repos, compilation naturally runs infrequently. For high-activity repos, the batching mechanism keeps cost proportional to information gain, not to PR volume.
- Model selection: compilation doesn't need Opus. Sonnet is sufficient for synthesis tasks — the compilation prompt is well-structured and the output format is defined.

---

## Implementation Sequence

### Phase 1: Data Foundation (Late M1 / Early M2)

Nothing to build — M1 already captures all the raw data. But verify:
- [ ] `finding_events` records reactions with correct `finding_id` linkage
- [ ] `pipeline_runs.metadata` contains the full model routing snapshot
- [ ] `findings.metadata` contains slice composition data (token counts per section)
- [ ] The semantic index summary (package list, symbol counts) is available after each run

### Phase 2: Storage + Bootstrap (M2)

- [ ] Schema migration: `repo_knowledge` table
- [ ] Bootstrap compilation job: generates the initial repo profile from the first N pipeline runs for existing repos
- [ ] `KnowledgeBase` type that loads active entries for a repo
- [ ] New `internal/knowledge` package with `KnowledgeAdapter` interface in `pkg/adapter`

### Phase 3: Pipeline Integration (M2)

Order matters — each step is independently valuable and should be shipped and validated before the next.

1. **Prompt injection (highest leverage).** Load the repo profile into the system prompt. This is the smallest code change with the largest review quality impact. Measure: compare finding precision (reaction rates) before and after.

2. **Planner risk scoring.** Query the hot spot registry during `computeRiskScore`. Measure: are we reviewing the right functions? Track whether hot-spot-influenced tasks produce more confirmed findings.

3. **Policy calibration.** Apply review calibration to confidence scores. Measure: are the adjusted thresholds better? Track FP rates at each tier boundary before and after.

4. **Slice builder integration.** Add knowledge context as a fourth slice section. Measure: does area profile context improve finding quality? Compare reaction rates on findings with vs. without KB context.

### Phase 4: Maintenance Loop (M2/M3)

- [ ] `KnowledgeLintJob`: periodic validation and gap detection
- [ ] Compilation batching for high-volume repos
- [ ] Dashboard or CLI: knowledge base health per repo (staleness, coverage, contradiction count)

### Phase 5: Advanced (M3+)

- [ ] Convention extraction from code patterns (tree-sitter across multiple runs), not just finding reactions
- [ ] Cross-repo knowledge: shared conventions and patterns across repos in the same org
- [ ] `mimir knowledge` CLI for inspecting, querying, and manually editing the KB
- [ ] Integration with the replay harness: validate KB changes offline before deploying them

---

## Open Questions

### Package Placement

Does the knowledge base live in `internal/eval` (extending the existing eval package), or in a new `internal/knowledge` package?

Arguments for `internal/eval`: eval already owns reaction data and scorecard queries. Compilation is a natural extension.

Arguments for `internal/knowledge`: the KB is consumed by planner, runtime, and policy — not just eval. Putting it in eval creates an awkward dependency where half the pipeline imports eval. A separate package keeps the dependency direction clean: `knowledge` imports from `store` (like eval does), and other packages access it through a `KnowledgeAdapter` interface in `pkg/adapter`.

Leaning toward: `internal/knowledge` with a `KnowledgeAdapter` interface.

### Compilation Model

Which model runs the compilation job? Options:
- Same model as reviews (Opus): highest quality, highest cost
- Sonnet: good quality for synthesis, much cheaper
- Haiku: cheapest, but may produce lower-quality synthesis

Leaning toward: Sonnet for compilation, Haiku for linting.

### Knowledge Base Versioning

Should KB entries be versioned (v1, v2, v3...) or just superseded? Versioning enables diffing ("what changed in the repo profile between last week and this week"). Supersession enables revert but not structured diff.

Leaning toward: supersession is sufficient for M2. The full history is in the table (superseded rows aren't deleted). Versioning is sugar that can be added later.

### Cross-Repo Knowledge

For orgs with many repos sharing conventions (e.g., a Go monorepo with 50 services), should knowledge transfer across repos? The convention guide for one service likely applies to sibling services.

Deferred to M3. The per-repo model is the right starting point. Cross-repo requires an org-level knowledge layer and a mechanism for repos to inherit from it.

### Token Budget for Compilation

How many tokens should the compilation job be allowed to use? The input is: current KB (~5-10K tokens for a mature repo) + pipeline run summary (~2-5K tokens) + reaction data (~1-2K tokens). Total input: ~8-17K tokens. Output: updated entries, ~2-5K tokens.

This is cheap. A Sonnet call with 15K input and 5K output is well under a dollar. Even daily, this is negligible compared to the review calls.

### Dedup Anchor Validation

The earlier ADR-0006 draft proposed checking whether dedup anchors (prior findings used to suppress new ones) have been thumbs-downed. This is still valuable and fits naturally into the KB: the hot spot registry tracks per-finding reaction data, and the linting job can flag discredited anchors.

Should dedup anchor validation be part of the KB's linting loop, or a standalone policy-layer check?

Leaning toward: both. The policy layer does a fast check at triage time (has this specific anchor been thumbs-downed?), and the linting job does a broader analysis (are there systematic patterns of bad anchors?).

---

## Relationship to Existing Specs

| Spec | Relationship |
|------|-------------|
| **00-overview** | The KB adds a new stage to the pipeline DAG: `CompileKnowledge` runs after `PostResults` |
| **04-planner** | `computeRiskScore` gains a KB parameter for historical hot spot data |
| **05-runtime** | Slice building gains a fourth section (knowledge context) with its own budget cap |
| **06-policy** | Confidence calibration uses `ReviewCalibration` entries from the KB |
| **07-store** | New `repo_knowledge` table and associated queries |
| **08-eval** | Eval scorecard gains KB health metrics; compilation job may live adjacent to eval |
| **12-prompts** | System prompt gains a `[Repo Knowledge]` section populated from the repo profile |

No existing spec needs to be rewritten. Each integration point is additive — new parameters, new slice sections, new prompt blocks. The KB's absence means the pipeline falls back to current M1 behavior exactly.
