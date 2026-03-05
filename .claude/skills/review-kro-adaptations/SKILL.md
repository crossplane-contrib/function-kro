---
name: review-kro-adaptations
description: Principal-level engineering review of all function-kro adaptations to upstream KRO code. Assesses quality, minimality, correctness, and maintainability of every change we've made.
arguments: [upstream-tag]
!command: ./scripts/diff-upstream-kro.sh -s -r $ARGUMENTS
---

# Review KRO Adaptations Skill

**STOP CHECK:** If "$ARGUMENTS" is empty or was not provided, do NOT proceed. Tell the user: "This skill requires an upstream KRO tag. Usage: `/review-kro-adaptations v0.8.3`" and stop immediately.

You are performing a principal-level engineering review of every adaptation function-kro has made to upstream KRO code. The diff summary from `./scripts/diff-upstream-kro.sh -s -r $ARGUMENTS` has been pre-injected above.

**Your job:** Review every modification and addition as a principal engineer would — not just "does it work?" but "is this the best way to achieve our goals?"

**Your mindset:** You are an expert in Go, Kubernetes controllers, Crossplane composition functions, and CEL. You have high standards. You care about:
- Minimizing divergence from upstream (every changed line is upgrade debt)
- Correctness under edge cases
- Code that communicates intent clearly
- Simplicity over cleverness
- Making the next upgrade easier, not harder

---

## Phase 1: Inventory

From the pre-injected diff summary, extract:

1. **MODIFIED files** — these are the focus of the review
2. **LOCAL ONLY files** — our additions, also reviewed
3. **Counts** — total files modified, added, excluded

Read `AGENTS.md` to understand the architecture and the purpose of each component. Read `patches/v*_PATCHES.md` (glob for it) to understand the documented intent behind each adaptation.

---

## Phase 2: Clone and Collect All Diffs

Clone upstream once for reuse:

```bash
AUDIT_DIR="/tmp/kro-review-$ARGUMENTS"
if [ ! -d "$AUDIT_DIR" ]; then
    git clone --depth 1 --branch $ARGUMENTS https://github.com/kubernetes-sigs/kro.git "$AUDIT_DIR"
fi
```

For **every** modified file, collect the full diff:

```bash
./scripts/diff-upstream-kro.sh -f <file> -u "$AUDIT_DIR" -l 0
```

Also read each local-only file in full.

---

## Phase 3: Per-File Engineering Review

For each modified file and each local-only file, evaluate against ALL of the following criteria. Be thorough — this is the core of the review.

### 3a. Minimality

- **Is every changed line necessary?** Could the same goal be achieved with fewer modifications?
- **Are there changes that could be avoided** by using interfaces, adapters, or wrapper patterns instead of modifying upstream code directly?
- **Are there drive-by changes** (formatting, renaming, reordering) mixed in with functional changes? These add upgrade noise for zero benefit.
- **Could any modification be pushed upstream** instead of maintained as a fork delta? If a change would benefit all KRO users, it shouldn't live only here.

### 3b. Correctness

- **Are there edge cases the modification doesn't handle?** Think about nil maps, empty slices, missing fields, unexpected types.
- **Does removing upstream code remove important safety checks?** When we delete validation or normalization, are we sure Crossplane handles it elsewhere, or are we creating a gap?
- **Are error paths correct?** Do modifications properly propagate errors, or do they swallow/ignore failures?
- **Are there concurrency concerns?** Shared state, missing locks, race conditions in the runtime execution path?

### 3c. Clarity and Intent

- **Would a new team member understand WHY each change was made** just by reading the code? Or does it require tribal knowledge?
- **Are adapter/wrapper patterns clearly named** to signal "this is a Crossplane-specific bridge"?
- **Are removed features clearly absent** or do they leave confusing dead code, unused parameters, or empty interfaces?

### 3d. Maintainability and Upgrade Cost

- **How painful will each modification be during the next upgrade?** Rate each file: trivial (mechanical), moderate (needs thought), painful (likely to break).
- **Are there modifications that could be restructured** to isolate our changes from upstream code? For example: wrapping upstream functions instead of modifying them, using composition over modification, or introducing thin adapter layers.
- **Is there duplicated logic** between our adaptations and upstream code that could diverge silently?

### 3e. Principal-Level Design Review

This is the highest-level assessment. Step back from individual changes and ask:

- **Is the overall adaptation strategy sound?** Is "vendor and modify" the right approach for each package, or would some packages be better served by a different integration pattern?
- **Are there architectural improvements** that would reduce total adaptation surface? For example: could an interface or adapter layer between fn.go and the KRO libraries absorb most modifications, leaving upstream code closer to untouched?
- **Are we fighting upstream's design** in places where we should instead embrace it and adapt our wrapper layer?
- **Are there upstream extension points** (interfaces, hooks, options patterns) that we're ignoring in favor of direct modification?
- **Would a principal engineer redesign any of these adaptations?** If you had unlimited time to refactor (but still needed to vendor KRO), what would you change?
- **Are there opportunities to contribute adapter interfaces upstream** that would make function-kro a thin wrapper instead of a fork?

### 3f. Local-Only Files

For files we've added (not in upstream):

- **Do they follow upstream's coding patterns** (naming, error handling, package organization)?
- **Are they well-scoped** or do they accumulate unrelated responsibilities?
- **Could any of them be contributed upstream** as general-purpose utilities?
- **Are they tested adequately?**

---

## Phase 4: Cross-Cutting Concerns

After reviewing all files individually, assess these system-level concerns:

### 4a. Consistency

- Are similar adaptations done the same way across files? Or does each file use a different approach to solve the same problem?
- Are naming conventions consistent across our additions and modifications?

### 4b. Test Coverage

- Are our adaptations tested? Not just "do tests exist" but "do tests verify the adaptation behavior specifically"?
- Are there modifications that silently change behavior but have no corresponding test changes?
- Run `go test -cover ./kro/...` and note coverage for modified packages.

### 4c. Error Surface

- Do our adaptations increase or decrease the error surface compared to upstream?
- Are there failure modes that only exist because of our modifications?

---

## Phase 5: Findings Report

Present findings in this structure. Be direct — praise what's good, be specific about what should improve.

```
## Engineering Review: function-kro adaptations vs upstream KRO {tag}

### Executive Summary
{2-3 sentences: overall quality assessment, biggest concern, biggest strength}

### Adaptation Surface
- Modified files: {N}
- Local-only files: {N}
- Total lines changed: ~{estimate from diffs}
- Upgrade difficulty estimate: {trivial / moderate / significant}

### File-by-File Findings

#### {file path}
- **Purpose of adaptation:** {one line}
- **Minimality:** {assessment}
- **Correctness:** {assessment}
- **Upgrade cost:** {trivial / moderate / painful}
- **Findings:** {specific issues or "Clean — no concerns"}
- **Recommendations:** {specific actionable items, or "None"}

{repeat for each file}

### Cross-Cutting Findings
{Consistency, test coverage, error surface observations}

### Principal-Level Recommendations

#### Quick Wins
{Changes that are small effort, high impact on quality or maintainability}

#### Strategic Improvements
{Larger refactors that would significantly reduce adaptation surface or improve quality}

#### Upstream Contributions
{Changes that could/should be proposed upstream to reduce our fork delta}

### Summary Table

| File | Minimality | Correctness | Upgrade Cost | Action Needed |
|------|-----------|-------------|--------------|---------------|
| ... | ... | ... | ... | ... |
```

---

## Phase 6: Cleanup

```bash
rm -rf "/tmp/kro-review-$ARGUMENTS"
```

---

## Important Guidelines

- **Do NOT make code changes.** This skill produces a review report only. The user decides what to act on.
- **Be specific.** "This could be better" is useless. "Lines 45-52 of builder.go remove the REST mapper check, but the replacement doesn't handle the case where..." is useful.
- **Cite line numbers and diff hunks.** Every finding should reference the specific code.
- **Distinguish severity.** Not every finding needs immediate action. Use: `critical` (correctness risk), `important` (significant quality/maintenance concern), `suggestion` (would be nice), `nitpick` (style/preference).
- **Acknowledge good work.** If an adaptation is clean, minimal, and well-done, say so. This calibrates the review — if everything is flagged, nothing stands out.
