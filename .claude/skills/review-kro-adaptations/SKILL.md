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

### Scope: Our Changes Only

**CRITICAL:** This review covers only code WE wrote or modified. It does NOT cover upstream KRO code we left unchanged.

- **In modified files:** Only review the diff hunks — the lines we changed, added, or removed. Do not critique upstream code surrounding our changes (comments, style, patterns) unless we modified it.
- **In local-only files:** Review everything — we own these entirely.
- **Upstream code left untouched:** Do NOT suggest changes to upstream comments, formatting, naming, or patterns that we haven't modified. Touching upstream code to "improve" it adds diff noise that makes upgrades harder. If upstream has a stale comment and we didn't change it, that's upstream's problem, not ours.
- **Dead code from our deletions:** If we deleted a caller but left the callee (e.g., we removed `validateResourceGraphDefinition` but left `isValidKindName`), note this as context but do NOT recommend deleting upstream's function — keeping it reduces our diff. Only flag dead code if WE introduced it in our additions.

The goal is to minimize our fork delta. Every line we touch is a line we must reconcile on every upgrade. The best adaptation is the smallest one that achieves the goal.

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

**Reminder:** Only review code in the diff hunks (our changes) and in local-only files. Do not critique upstream code we left untouched.

### 3a. Minimality — THE MOST IMPORTANT CHECK

**The bar: every modification to an upstream file must be functionally required.** If removing a change would still leave the code compiling and working correctly as a Crossplane composition function, the change should not exist. This is the single most common failure mode in reviews — cosmetic improvements slip through because they "look reasonable" even though they aren't necessary.

For each diff hunk in each modified file, ask this question explicitly:

> "If I reverted this specific change back to upstream's version, would function-kro fail to compile, fail tests, or produce incorrect behavior?"

If the answer is no, flag it as **REVERT: not functionally required**.

Common violations to watch for — these are NEVER acceptable in upstream-vendored files:
- **Extracting helpers** to deduplicate upstream code (e.g., creating a `stringMapSchema()` to replace repeated inline schemas)
- **Reformatting** upstream code (reordering fields, changing line breaks, adjusting whitespace)
- **Renaming** upstream variables or functions to "better" names
- **Adding comments** to upstream code that we didn't otherwise modify
- **Refactoring** upstream patterns we find inelegant (e.g., collapsing verbose structs)

Also check:
- **Are there changes that could be avoided** by using interfaces, adapters, or wrapper patterns instead of modifying upstream code directly?
- **Were files written from scratch when they should have been copied from upstream and modified?** This is a common source of unnecessary divergence. If the diff shows the entire file changed, check whether the upstream file was used as a starting point.

### 3b. Correctness

- **Are there edge cases our modifications don't handle?** Think about nil maps, empty slices, missing fields, unexpected types — but only in code we changed.
- **Does removing upstream code remove important safety checks?** When we delete validation or normalization, are we sure Crossplane handles it elsewhere, or are we creating a gap?
- **Are error paths correct in our modifications?** Do our changes properly propagate errors, or do they swallow/ignore failures?
- **Are there concurrency concerns in code we wrote?** Shared state, missing locks, race conditions.

### 3c. Clarity and Intent

- **Would a new team member understand WHY each of our changes was made** just by reading the code? Or does it require tribal knowledge?
- **Are our adapter/wrapper patterns clearly named** to signal "this is a Crossplane-specific bridge"?
- **Do our NOTE comments adequately explain why upstream code was removed?**

### 3d. Maintainability and Upgrade Cost

- **How painful will each of our modifications be during the next upgrade?** Rate each file: trivial (mechanical), moderate (needs thought), painful (likely to break).
- **Are there modifications that could be restructured** to isolate our changes from upstream code? For example: wrapping upstream functions instead of modifying them, using composition over modification, or introducing thin adapter layers.
- **Is there duplicated logic** between our adaptations and upstream code that could diverge silently?

### 3e. Principal-Level Design Review

This is the highest-level assessment. Step back from individual changes and ask:

- **Is the overall adaptation strategy sound?** Is "vendor and modify" the right approach for each package, or would some packages be better served by a different integration pattern?
- **Are there architectural improvements** that would reduce total adaptation surface? For example: could an interface or adapter layer between fn.go and the KRO libraries absorb most modifications, leaving upstream code closer to untouched?
- **Are we fighting upstream's design** in places where we should instead embrace it and adapt our wrapper layer?
- **Are there upstream extension points** (interfaces, hooks, options patterns) that we're ignoring in favor of direct modification?

### 3f. Local-Only Files

For files we've added (not in upstream):

- **Do they follow upstream's coding patterns** (naming, error handling, package organization)?
- **Are they well-scoped** or do they accumulate unrelated responsibilities?
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

### Changes to Revert (Not Functionally Required)

{List every diff hunk that should be reverted to match upstream because it is not functionally required for compilation or correct behavior. For each, name the file, describe the change, and explain why it's not needed. If none found, state "None — all changes are functionally required."}

### File-by-File Findings

#### {file path}
- **Purpose of adaptation:** {one line}
- **Functionally required:** {yes/no — if no, this should appear in "Changes to Revert" above}
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
{Changes that are small effort, high impact on quality or maintainability — focused on OUR code}

#### Strategic Improvements
{Larger refactors that would significantly reduce adaptation surface or improve quality}

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
- **Only critique OUR changes.** Every finding must be about code we wrote, modified, or added — never about upstream code we left untouched. If you're about to flag something, ask: "Did we write/change this line?" If no, skip it.
- **Be specific.** "This could be better" is useless. "Lines 45-52 of builder.go remove the REST mapper check, but the replacement doesn't handle the case where..." is useful.
- **Cite line numbers and diff hunks.** Every finding should reference the specific code.
- **Distinguish severity.** Not every finding needs immediate action. Use: `critical` (correctness risk), `important` (significant quality/maintenance concern), `suggestion` (would be nice), `nitpick` (style/preference).
- **Acknowledge good work.** If an adaptation is clean, minimal, and well-done, say so. This calibrates the review — if everything is flagged, nothing stands out.
