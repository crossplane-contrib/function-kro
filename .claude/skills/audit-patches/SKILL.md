---
name: audit-patches
description: Audit function-kro's patches documentation against the actual upstream KRO diff. Validates that v*_PATCHES.md accurately documents all modifications, additions, and exclusions.
arguments: [upstream-tag]
!command: ./scripts/diff-upstream-kro.sh -s -r $ARGUMENTS
---

# Audit Patches Skill

**STOP CHECK:** If "$ARGUMENTS" is empty or was not provided, do NOT proceed. Tell the user: "This skill requires an upstream KRO tag. Usage: `/audit-patches v0.8.3`" and stop immediately.

You are auditing function-kro's patches documentation against the actual upstream KRO code. The diff summary output from `./scripts/diff-upstream-kro.sh -s -r $ARGUMENTS` has been pre-injected above this prompt.

**Your job:** Verify that the patches documentation (`patches/v*_PATCHES.md`) accurately and completely describes every difference between our code and upstream KRO at the specified tag.

## ARGUMENTS

The user provides an upstream KRO git tag (e.g., `v0.8.3`). This was already passed to the diff script. Use this same tag throughout.

---

## Phase 1: Parse Diff Results

From the pre-injected diff summary output, extract three lists:

1. **MODIFIED files** — files that differ from upstream (after import path normalization)
2. **LOCAL ONLY files** — files that exist only in function-kro (our additions)
3. **UPSTREAM ONLY files** — files that exist only in upstream (we intentionally exclude them)

Also extract the summary counts (Identical, Modified, Local only, Upstream only).

Record these lists — they are the source of truth for the rest of the audit.

---

## Phase 2: Find and Read Patches Doc

1. Glob for `patches/v*_PATCHES.md` to find the matching patches document.
2. Read the entire patches document.
3. Note the document's structure: which sections exist, what files are listed where, what counts are claimed.

---

## Phase 3: Cross-Reference Coverage

Check each category systematically:

### 3a. Modified Files Coverage

For every `[MODIFIED]` file from Phase 1:
- Is it documented in the patches doc with a dedicated section or table entry?
- If not, flag it as **MISSING: undocumented modification**.

### 3b. Local-Only Files Coverage

For every `[LOCAL ONLY]` file from Phase 1:
- Is it listed in the "Files Added" table (or equivalent section)?
- If not, flag it as **MISSING: undocumented local-only file**.

### 3c. Upstream-Only Files Coverage

For every `[UPSTREAM ONLY]` file from Phase 1:
- Is it listed in the "Files Excluded" table (or equivalent section)?
- If not, flag it as **MISSING: undocumented upstream-only exclusion**.

### 3d. Phantom Entries

For every file the patches doc claims is modified:
- Does it actually appear as `[MODIFIED]` in the diff output?
- If the diff shows it as `[IDENTICAL]`, flag it as **PHANTOM: doc claims modification but file is identical to upstream**.

### 3e. Summary Counts

Compare the patches doc's claimed counts (e.g., "9 modified files") against the actual diff summary counts. Flag any mismatches.

---

## Phase 4: Deep-Dive Modified Files

This is the most important phase. For each `[MODIFIED]` file:

### 4.0 Clone Upstream Once

Clone upstream to a reusable location so per-file diffs don't each trigger a fresh clone:

```bash
AUDIT_DIR="/tmp/kro-audit-$ARGUMENTS"
if [ ! -d "$AUDIT_DIR" ]; then
    git clone --depth 1 --branch $ARGUMENTS https://github.com/kubernetes-sigs/kro.git "$AUDIT_DIR"
fi
```

### 4.1 Get the Full Diff

For each modified file, run:

```bash
./scripts/diff-upstream-kro.sh -f <file> -u "$AUDIT_DIR" -l 0
```

The `-u` flag reuses the existing clone. The `-l 0` flag shows the complete diff without truncation.

### 4.2 Assess Documentation Quality

For each modified file, evaluate:

1. **Accuracy** — Does the patches doc correctly describe what was changed? Are the described modifications actually present in the diff?
2. **Completeness** — Does the doc capture ALL modifications shown in the diff, or are some changes undocumented?
3. **Gaps** — Are there significant diff hunks that the doc doesn't mention at all?
4. **Quality** — Are the descriptions clear enough that an engineer could re-apply these changes to a new upstream version?

Record your findings per file.

---

## Phase 5: Quality and Effectiveness Assessment

### 5a. Quick Reference Tables

Check that the "Quick Reference" tables at the bottom of the patches doc are complete and accurate:
- Do they list all modified files with correct adaptation summaries?
- Do they list all local-only files?
- Do they list all upstream-only exclusions with reasons?

### 5b. Upgrade Readiness

Ask: If an agent were following `UPGRADE_PROCESS.md` Phase 3 (Re-Apply Adaptations), using this patches doc as their guide, would they:
- Know every file that needs modification?
- Understand the intent of each adaptation well enough to re-apply it to changed upstream code?
- Know which files to add back that aren't in upstream?
- Know which upstream files to intentionally skip?

Flag any gaps that would cause an upgrade to fail or produce incorrect results.

---

## Phase 6: Update Patches Doc (if needed)

If any issues were found in Phases 3-5, fix them:

1. **Fix summary counts** if they don't match the diff
2. **Add missing file entries** for undocumented modifications, additions, or exclusions
3. **Correct inaccurate descriptions** where the doc doesn't match the actual diff
4. **Add undocumented adaptations** discovered in Phase 4
5. **Remove phantom entries** for files that are actually identical
6. **Update quick reference tables** to match corrections

**Important:** Only make changes that are clearly necessary based on diff evidence. Do not rewrite sections that are already accurate.

---

## Phase 7: Validate and Report

### 7a. Re-validate

If changes were made in Phase 6:
- Re-read the updated patches doc
- Verify all Phase 3 checks now pass
- Verify quick reference tables are consistent with the detailed sections

### 7b. Structured Report

Present a final report with this structure:

```
## Audit Report: patches/v{X}_PATCHES.md vs upstream {tag}

### Summary
- Modified files: {N} (documented: {M}, missing: {K})
- Local-only files: {N} (documented: {M}, missing: {K})
- Upstream-only files: {N} (documented: {M}, missing: {K})
- Phantom entries: {N}
- Overall accuracy: {percentage or qualitative}

### Issues Found
{List of issues, or "None — documentation is accurate"}

### Changes Made
{List of changes to patches doc, or "None — no changes needed"}

### Upgrade Readiness
{Assessment of whether the doc is sufficient for a future upgrade}
```

### 7c. Cleanup

Remove the temporary upstream clone:

```bash
rm -rf "/tmp/kro-audit-$ARGUMENTS"
```

---

## Escalation Triggers

**STOP and ask the user** before continuing if you encounter any of these:

- A modified file has a large diff (>100 lines changed) with no corresponding documentation — this may indicate an intentional but undocumented change
- You find a local-only file you don't recognize — the user may have context about its purpose
- The adaptation intent is unclear — better to ask than guess wrong
- The patches doc structure is significantly different from what this skill expects — the user may have reorganized it intentionally
