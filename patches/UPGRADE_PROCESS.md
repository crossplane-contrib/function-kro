# Function-KRO Upstream Upgrade Process

This document describes the process for upgrading function-kro to a new upstream KRO version. The process is designed to be repeatable and can be executed by humans or AI agents.

## Overview

Function-KRO uses code from upstream [kubernetes-sigs/kro](https://github.com/kubernetes-sigs/kro) to run as a Crossplane composition function. Most of it is imported unmodified as the Go module `github.com/kubernetes-sigs/kro`. The upstream packages we have to change are vendored under `kro/` with adaptations. The "Vendoring Rules" section at the end of this document says when to vendor, and `v{CURRENT}_PATCHES.md` lists what we vendor today. When upstream KRO releases a new version, we need to:

1. Understand what changed upstream
2. Bump the module dependency and copy the new vendored code
3. Re-apply our adaptations
4. Validate everything works

This applies to every upstream release, patch versions included. Do not size the job from the version number. Patch releases from upstream routinely contain feature work. The v0.9.0 to v0.9.3 range reworked the graph builder and the runtime, both of which we adapted at the time, and added new CEL libraries and a conditions feature. The scope check in Phase 1 tells you how big the upgrade actually is.

Releases can also move the layers we depend on. v0.10.0 deleted `pkg/runtime`, which we vendored, and replaced it with `pkg/graphengine`. That upgrade shrank what we vendor from six packages to one, because the new engine takes the schema resolver and REST mapper we need to inject as dependencies. If the scope check shows upstream reorganized the packages we vendor or import, stop and work out how the layers fit together now before copying anything.

## Commit Strategy

**Make incremental commits after each major step.** This provides:
- Clear diffs showing exactly what changed at each phase
- Recovery points if something goes wrong
- Easier debugging when tests fail
- Ability to pause and resume the upgrade

The commits can be squashed into a single commit before merging if desired, but keeping them separate during development is essential for tracking progress and debugging.

**Commit points in this process:**
1. After copying upstream files (raw upstream code)
2. After fixing import paths (mechanical transformation)
3. After restoring our additions (preserve our custom files)
4. After applying adaptations (our modifications to upstream code)
5. After documenting changes (patches documentation)
6. Final commit with any remaining cleanup

## Directory Structure

```
patches/
├── UPGRADE_PROCESS.md           # This document
├── UPSTREAM_DIFFERENCES.md      # Where function-kro behaves differently from upstream kro
└── v{CURRENT}_PATCHES.md        # Adaptations for the current version (the only patches doc)
```

**Only one patches doc should exist at a time** — the one for the current baseline version. Old patches docs and pre-upgrade analysis docs must be deleted after an upgrade completes (see Phase 5). Keeping superseded docs around confuses agents and humans alike — they read stale planning notes (e.g., "metrics may not work") and treat them as current truth, even when the actual outcome is different.

---

## Phase 1: Pre-Analysis

**Goal:** Understand what changed upstream before touching any code.

### Step 1.0: Validate Current Patches Documentation

Before relying on `v{OLD}_PATCHES.md` as your migration guide, verify it actually matches the current codebase. Stale or inaccurate patch docs will cause you to apply wrong adaptations to the new version.

**Preferred method — use the `/audit-patches` skill:**

```
/audit-patches v{OLD}
```

This skill automates the entire validation process: runs the diff script, cross-references every file against the patches doc, deep-dives each modification, fixes any discrepancies, and produces a structured audit report. It is the recommended way to validate patches documentation.

**Manual fallback** (if the skill is unavailable or you need to debug):

Run the diff script — this is the source of truth for what we've actually changed:

```bash
# Summary: which files are identical, modified, local-only, or upstream-only
./scripts/diff-upstream-kro.sh -s -r v{OLD}

# Full diff of a specific file
./scripts/diff-upstream-kro.sh -f graphengine/executor/simple.go -r v{OLD}
```

Then verify the patches doc against the script output:

1. **Every `[MODIFIED]` file from the script should be documented** in `v{OLD}_PATCHES.md`. If a file shows as modified but isn't in the doc, the doc is incomplete.

2. **Every `[LOCAL ONLY]` file should be in the "Files Added" table.** If a local-only file isn't documented, add it.

3. **Every `[IDENTICAL]` file should NOT be listed as modified** in the patches doc. If the doc claims a file was modified but the script shows it's identical, the doc is wrong.

4. **Every `[UPSTREAM ONLY]` file in a vendored package** should be in the "Files Removed" table or intentionally excluded.

5. **Check for undocumented changes since the doc was last updated:**
   ```bash
   # Find the last commit that touched the patches doc
   git log -1 --format="%H %ci" -- patches/v{OLD}_PATCHES.md

   # Find any kro/ changes after that date
   git log --oneline --after="<date from above>" -- kro/
   ```

6. **If discrepancies are found:** Stop and report them to the user before proceeding. The patches doc is the source of truth for the adaptation step (Phase 3) — if it's wrong, the entire upgrade will apply wrong adaptations. Present the discrepancies clearly, update `v{OLD}_PATCHES.md` to reflect reality, and get user confirmation that the updated doc is accurate before continuing to Step 1.1.

   **Do not skip this confirmation.** Even if the fixes seem obvious, the user may have context about why the code diverged from the doc (e.g., an intentional change that was never documented, or a work-in-progress that shouldn't be carried forward).

### Step 1.1: Review Release Notes

Read the upstream release notes to understand the high-level changes:
```
https://github.com/kubernetes-sigs/kro/releases/tag/v{NEW}
```

Look for:
- New features (may require new adaptations)
- Breaking changes (will definitely require attention)
- Bug fixes (usually come free)
- Dependency updates (may affect go.mod)

### Step 1.2: Clone Both Versions

One blobless clone plus two worktrees gives you a checkout of each version and a repo where `git diff v{OLD}..v{NEW}` works. Phase 2 copies from the checkouts, and the scope check below runs the cross-tag diff.

```bash
git clone --filter=blob:none \
    https://github.com/kubernetes-sigs/kro.git /tmp/kro

git -C /tmp/kro worktree add /tmp/kro-old v{OLD}
git -C /tmp/kro worktree add /tmp/kro-new v{NEW}
```

### Step 1.3: Scope Check

Before reading any diffs, measure how much of the code we depend on actually moved. That's the vendored packages, the module packages we call, and the upstream code our own code depends on (listed under "Our Code That Depends on Upstream Behavior" in the patches doc):

```bash
git -C /tmp/kro diff --stat v{OLD}..v{NEW} -- \
    pkg/graphengine/executor pkg/graphengine/rgdadapter \
    api/v1alpha1 pkg/graphengine/compiler pkg/graphengine/runtime \
    pkg/graph/builder.go pkg/controller/instance/controller_graph_engine.go
```

From that output, check:

1. **Total size.** Changes under the vendored packages decide how much Phase 2 and 3 work there is. Changes to the module packages come along with the dependency bump, but read them for behavior changes that affect us.

2. **Which files we modify are in the list.** Cross-reference against the "Modified Files" table in `v{OLD}_PATCHES.md`. Those are where re-applying adaptations takes real thought. Everything else is a straight copy.

3. **New API server calls in the vendored code.** Our vendored executor keeps upstream's `Client` field nil and reroutes the calls an RGD reaches. A new `s.Client` call on one of those paths would panic at runtime, and the patches doc won't mention it because it didn't exist at v{OLD}. List the `s.Client` calls at each version and look at any new ones:

   ```bash
   git -C /tmp/kro grep -n 's\.Client\.' v{OLD} -- pkg/graphengine/executor ':!*_test.go'
   git -C /tmp/kro grep -n 's\.Client\.' v{NEW} -- pkg/graphengine/executor ':!*_test.go'
   ```

4. **Our code that depends on upstream behavior.** Diff the upstream code behind each row of the patches doc's "Our Code That Depends on Upstream Behavior" table between the two versions. None of it copies upstream, so the diff script can't see it.

5. **Upstream logic our additions repeat.** For each row of the patches doc's "Upstream Logic Our Additions Repeat" table, diff the named upstream function between the two versions. That code is in our local-only `crossplane.go` or in our own code outside `kro/`, so the diff script never compares it against the upstream code it copies. For example:

   ```bash
   fn='/^func (s \*Simple) prepareItem(/,/^}/p'
   diff <(git -C /tmp/kro show v{OLD}:pkg/graphengine/executor/simple.go | sed -n "$fn") \
        <(git -C /tmp/kro show v{NEW}:pkg/graphengine/executor/simple.go | sed -n "$fn")
   ```

Record the current dependency skew too. The vendored packages compile against the module packages at the version pinned in `go.mod`, so the pin and the vendored code must always come from the same tag:

```bash
grep "kubernetes-sigs/kro " go.mod
```

### Step 1.4: Analyze Upstream Changes

Have an AI agent (or manually) analyze the differences. This analysis is a **working document for the upgrade conversation only** — do NOT commit it to the repo (see Phase 5 for cleanup rules).

```bash
# Compare the vendored package and the module packages we call
diff -r /tmp/kro-old/pkg/graphengine /tmp/kro-new/pkg/graphengine
diff -r /tmp/kro-old/api/v1alpha1 /tmp/kro-new/api/v1alpha1
```

**The analysis document should include:**

1. **Executive Summary** - High-level impact assessment
2. **New Features** - What was added and whether function-kro needs it
3. **API Changes** - Constructor signatures, method signatures, type changes
4. **File Changes** - Which files were added/modified/deleted
5. **Dependency Changes** - go.mod differences
6. **Action Items** - Specific things that need adaptation

**AI Agent Prompt for Analysis:**
```
Review the upstream KRO changes between v{OLD} and v{NEW}.

Context:
- Release notes: https://github.com/kubernetes-sigs/kro/releases/tag/v{NEW}
- Comparison: https://github.com/kubernetes-sigs/kro/compare/v{OLD}...v{NEW}

Generate a document covering:
1. Summary of changes with impact assessment for function-kro
2. New types/functions introduced in pkg/graphengine/ and api/v1alpha1/
3. Changed function signatures (especially constructors and options)
4. Removed types/functions
5. Dependency version changes

Focus on changes that affect:
- The executor's Apply loop and how it handles each node kind
- rgdadapter.BuildRuntimeForInstance and its compile and runtime options
- Compiler extension points (schema resolver, REST mapper, node options)
- Runtime node evaluation, readiness and scope publishing
```

---

## Phase 2: Copy New Files

**Goal:** Replace vendored KRO code with the new version, and move the module dependency to the same version.

### Step 2.1: Remove Old Files

Remove only the upstream files in each vendored package. Our own files in those packages stay put, and the copy below won't touch them because upstream has no file by that name.

```bash
# Remove existing vendored upstream files (see the allowlist and file tables
# in v{OLD}_PATCHES.md)
rm -f kro/graphengine/executor/executor.go
rm -f kro/graphengine/executor/simple.go

# Keep these files (our additions):
# - kro/graphengine/executor/crossplane.go
```

### Step 2.2: Copy New Files and Bump the Module

```bash
# Copy from upstream (see the allowlist in v{OLD}_PATCHES.md). We vendor
# non-test files only.
for pkg in graphengine/executor; do
    find /tmp/kro-new/pkg/$pkg -maxdepth 1 -name '*.go' ! -name '*_test.go' \
        -exec cp {} kro/$pkg/ \;
done

# The vendored code compiles against the module packages, so they must come
# from the same tag.
go get github.com/kubernetes-sigs/kro@v{NEW}
go mod tidy
```

**IMPORTANT: Always copy upstream files verbatim first.** Never write vendored files
from scratch; always start from upstream's version, then modify. Writing files fresh
risks introducing formatting divergence, missing code, and unnecessary diff noise
against upstream. The copy-then-modify approach ensures our diffs show only the
intentional adaptations.

**COMMIT CHECKPOINT 1: Raw upstream code**

```bash
git add -A
git commit -m "$(cat <<'EOF'
chore(upgrade): copy upstream KRO v{NEW} files

Raw copy of the upstream KRO v{NEW} packages we vendor, before any
modifications, and the github.com/kubernetes-sigs/kro module bumped to
the same tag.

This commit captures the unmodified upstream state for diff comparison.
EOF
)"
```

This commit establishes a clean baseline of upstream code. All subsequent
commits will show exactly what we modified.

### Step 2.3: Fix Import Paths

Vendored files import other upstream packages as `github.com/kubernetes-sigs/kro/pkg/...`. Leave those imports alone: they resolve to the module dependency, which is what we want for every package we don't vendor.

Rewrite an import only when it points at another package we vendor, so the vendored files use our copy:

```bash
# Example, if we vendored pkg/graphengine/<pkg> and another vendored file imported it:
#   sed -i 's|"github.com/kubernetes-sigs/kro/pkg/graphengine/<pkg>"|"github.com/crossplane-contrib/function-kro/kro/graphengine/<pkg>"|' kro/graphengine/*/*.go
```

As of v0.10.0 we vendor only the executor, so there is nothing to rewrite.

**COMMIT CHECKPOINT 2: Import paths fixed**

Skip this commit if nothing changed.

```bash
git add -A
git commit -m "$(cat <<'EOF'
chore(upgrade): fix import paths for function-kro

Mechanical transformation of import paths between vendored packages.
No functional changes.
EOF
)"
```

This commit isolates the import path changes, making it easy to verify
only the expected substitutions were made.

### Step 2.4: Restore Our Additions

Step 2.1 removes only upstream's files, so our additions should be untouched. If one was overwritten or deleted anyway, restore it from git:

```bash
git checkout HEAD~1 -- kro/graphengine/executor/crossplane.go
```

**COMMIT CHECKPOINT 3: Our additions restored**

Skip this commit if nothing changed.

```bash
git add -A
git commit -m "$(cat <<'EOF'
chore(upgrade): restore function-kro custom files

Restored files that are unique to function-kro (not from upstream):
- kro/graphengine/executor/crossplane.go
EOF
)"
```

The codebase likely won't compile yet.

---

## Phase 3: Re-Apply Adaptations

**Goal:** Make the new upstream code work with Crossplane's function model.

### Mindset: Intelligent Merging, Not Mechanical Pasting

**CRITICAL:** Start from the new upstream code and ask "what does this need to work in our context?" — NOT "where do I paste our old changes?" The patches doc records what we changed *last time*, but the new upstream may have evolved in ways that change what's needed.

Every adaptation we maintain exists to bridge a specific gap between upstream's assumptions (Kubernetes controller with API server access) and our runtime environment (Crossplane composition function with no API access). The question for each adaptation during upgrade is: **does this gap still exist in the new code?**

#### Principles

1. **Evaluate each adaptation independently against the new code.** For each one, ask:
   - Did upstream add extension points (interfaces, options, hooks) that make this adaptation unnecessary?
   - Did upstream refactor so the adaptation needs to land in a different place or look different?
   - Did upstream add new functionality in the same area that our old code would silently break or overwrite?
   - Is the gap smaller now — maybe we only need half the old adaptation?

2. **Prefer upstream's solution when one exists.** If upstream added a way to inject a `SchemaResolver`, use that — don't replace their constructor with ours. Less divergence is always better. Our old adaptation was the best solution *at the time*; the new upstream may offer a better one.

3. **Preserve upstream improvements in code we modify.** If upstream added better error handling, validation, or edge case coverage in a function we rewrite, our replacement should incorporate those improvements, not overwrite them with our old version. Read the new code carefully before replacing it.

4. **Watch for new gaps.** If upstream added a new feature that calls the REST mapper or requires API access, that needs a *new* adaptation — not just re-applying old ones. The patches doc won't mention these because they didn't exist before.

5. **The adaptation surface should shrink over time, not grow.** Each upgrade is a chance to reduce divergence. If upstream moved closer to what we need, take advantage of it.

6. **Every adaptation must be functionally required.** Do not make cosmetic changes, refactors, deduplication, or "improvements" to upstream code. If the upstream code compiles and works correctly without a change, do not make that change. Drive-by cleanups (extracting helpers, renaming variables, reformatting) create diff noise that makes every future upgrade harder. The only valid reason to modify an upstream file is to bridge a functional gap between upstream's controller model and our composition function model.

#### Decision Framework

For each adaptation in the old patches doc, the outcome is one of:

| Outcome | When | Action |
|---------|------|--------|
| **Same** | Gap still exists, same location | Apply, adapted to surrounding code changes |
| **Moved** | Gap still exists, code restructured | Find new location, apply the *intent* there |
| **Shrunk** | Upstream partially solved it | Only apply what's still necessary |
| **Gone** | Upstream eliminated the gap | Drop the adaptation entirely |
| **Grown** | New upstream code has the same gap | Extend adaptation to cover new code too |

**The patches doc documents WHAT we changed. You must reason about WHY we changed it and whether that WHY still applies.**

### Step 3.1: Identify Compilation Errors

```bash
go build ./...
```

This will show what broke. Common issues:
- Constructor signature mismatches
- Missing/renamed types
- Changed method signatures

### Step 3.2: Apply Adaptations

Using the previous `v{OLD}_PATCHES.md` as a guide and the "Intelligent Merging" principles above, adapt the new upstream code to work in our context.

**AI Agent Prompt for Adaptations:**
```
I'm upgrading function-kro from KRO v{OLD} to v{NEW}.

Context files:
1. patches/v{OLD}_PATCHES.md - Our previous adaptations and their intent
2. patches/v{OLD}_to_v{NEW}_CHANGES.md - What changed upstream
3. patches/UPGRADE_PROCESS.md - Read the "Intelligent Merging" section carefully
4. fn.go - Our Crossplane function entry point
5. engine.go - How we prepare the input, REST mapper and schema resolver
6. kro/graphengine/executor/crossplane.go - Our additions to the vendored executor
7. Current compilation errors from `go build`

IMPORTANT: Do NOT blindly paste old adaptation code into the new upstream.
For EACH adaptation in v{OLD}_PATCHES.md:
1. Read the NEW upstream code in that area first
2. Understand WHY the adaptation existed (what gap it bridges)
3. Check if upstream already solved the problem (new interfaces, options, etc.)
4. Decide the outcome: Same / Moved / Shrunk / Gone / Grown
5. Only then write the adaptation — tailored to the new code, not copied from the old

The fundamental gaps we bridge (these are the WHYs):
- No API server access: no applies, reads, watches or deletes, no live REST
  mapper, no CRD fetching
- Crossplane owns composed resources: it applies them, fetches external
  resources when asked, and deletes any resource the function stops returning
- Schema from Crossplane: XR schema arrives as *spec.Schema, not via SimpleSchema
- No CRD generation: Crossplane manages the XR CRD, not us
- Namespaces: Crossplane sets the namespace on the composed resources we return
- Input type differences: our ResourceGraph vs upstream's ResourceGraphDefinition CR

If upstream added extension points or restructured code to make any of these
gaps smaller, prefer upstream's approach over our old adaptation. If that
means a vendored package no longer needs any changes, stop vendoring it and
import it as a module instead.

If upstream added NEW code that has the same gaps (e.g., a new s.Client call
on a path an RGD reaches), that needs a NEW adaptation not in the old patches
doc.

Then, for each row in the "Our Code That Depends on Upstream Behavior"
table in v{OLD}_PATCHES.md, diff the upstream code it depends on between
v{OLD} and v{NEW} and update ours if its assumptions changed. If a change
would mean reproducing upstream logic, follow the "Vendoring Rules" section
of UPGRADE_PROCESS.md.

After applying adaptations:
- Run `go generate ./...` to update generated methods and CRD schemas
- Run `go build ./...` to verify compilation
- Run `go test ./...` and fix any failures
```

#### Re-applying test adaptations

We vendor no upstream tests as of v0.10.0, so this applies only if a vendored package brings its tests along again.

Phase 2 overwrites the vendored `*_test.go` files along with everything else, and the diff script does not compare them, so the compiler is what surfaces this work: `go test ./...` fails to build the packages whose tests still expect the upstream API. `v{OLD}_PATCHES.md` lists which test files diverge and why.

Hand-porting them is error-prone, because our copies both delete upstream cases and make local edits. A three-way merge re-applies both mechanically:

```bash
# base   = upstream v{OLD}, what our adaptations were written against
# ours   = our pre-upgrade copy of the file
# theirs = upstream v{NEW}
# Rewrite upstream import paths to ours in base and theirs first, or upstream
# import paths leak into the merged file and cause extra conflicts.
# The merged result is written back to ours.go.
git merge-file -L function-kro -L "upstream v{OLD}" -L "upstream v{NEW}" \
    ours.go base.go theirs.go
```

Upstream's new cases come along, our deletions stay deleted, and the only conflicts are where both sides touched the same lines. Resolve those by asking the same question as for source code: does the gap this case covers still exist here?

**COMMIT CHECKPOINT 4: Adaptations applied**

```bash
git add -A
git commit -m "$(cat <<'EOF'
chore(upgrade): apply function-kro adaptations to v{NEW}

Re-applied adaptations for Crossplane function compatibility:
- Template nodes become desired composed resources instead of applies
- The status patch node becomes the desired XR status
- External refs read what Crossplane fetched
- Existing resources are kept for nodes the executor can't process
- [Add any new adaptations specific to this version]

Build passes: yes
Tests pass: yes
EOF
)"
```

This is the most important commit - it shows exactly what we changed
from upstream to make it work as a Crossplane function. The diff from
the previous commit reveals all our adaptations clearly.

### Step 3.3: Handle New Features

If upstream added features that function-kro should support (e.g., Collections/forEach):

1. **New input fields come with the dependency bump** in Phase 2: new fields on `Resource`, `ExternalRef`, `ForEachDimension`, etc. arrive automatically. Only update `input/v1alpha1/input.go` if the new fields affect `ResourceGraph` (our unique type), then run `go generate ./...` to regenerate the input CRD.
2. **Update fn.go** to handle new runtime behaviors
3. **Add tests** for new functionality

### Step 3.4: Document New Adaptations

Create `v{NEW}_PATCHES.md` documenting:
- All adaptations (carried forward + new)
- Any changes to the adaptation approach
- New features and how they're integrated
- Adaptations that were **dropped** because upstream eliminated the gap (record why — this is valuable for future upgrades)
- Adaptations that **changed shape** because upstream restructured the code (record what's different and why)

**COMMIT CHECKPOINT 5: Documentation updated**

```bash
git add -A
git commit -m "$(cat <<'EOF'
docs(upgrade): document v{NEW} patches and changes

Added/updated patch documentation:
- patches/v{NEW}_PATCHES.md - All adaptations for this version
- [Any other documentation changes]

This documents what we changed and why for future upgrades.
EOF
)"
```

Documenting the patches in a separate commit keeps the code changes
clean and makes it clear what was modified vs documented.

---

## Phase 4: Validation

**Goal:** Ensure everything works correctly.

### Step 4.1: Build

```bash
go build ./...
```

Must complete with no errors.

### Step 4.2: Run Tests

```bash
go test -v ./...
```

Fix any test failures. Some may be due to:
- Changed test utilities in upstream
- New validation that catches previously-ignored issues
- Changed behavior that tests need to account for

### Step 4.3: Lint

```bash
golangci-lint run
```

Fix any lint issues.

### Step 4.4: Manual Testing

Let the human handle manual testing and validation for now until we have more automation in place for this.

---

## Phase 5: Finalize

### Step 5.1: Update Documentation

- [ ] Update `patches/v{NEW}_PATCHES.md` with final adaptations
- [ ] **Delete `patches/v{OLD}_PATCHES.md`** — superseded by the new patches doc
- [ ] **Delete any `patches/v{OLD}_to_v{NEW}_CHANGES.md`** — this was a working doc for the upgrade, not a permanent artifact. Keeping it causes agents to treat pre-upgrade speculation as current truth.
- [ ] Update `patches/UPSTREAM_DIFFERENCES.md` for any difference from upstream kro the upgrade added or removed
- [ ] Verify only `UPGRADE_PROCESS.md`, `UPSTREAM_DIFFERENCES.md` and `v{NEW}_PATCHES.md` remain in `patches/`
- [ ] Update every reference to the old baseline version. Renaming the patches doc breaks links elsewhere, so run `git grep -n "v{OLD}"` and fix each hit that refers to the current baseline. Leave historical examples alone, such as the version ranges cited earlier in this document. Expect at least `AGENTS.md` (the directory tree, the reference-documents list, and the `/audit-patches` baseline note).
- [ ] Update `AGENTS.md` if architecture changed significantly
- [ ] Update `README.md` with new KRO version

### Step 5.2: Update Dependencies

**Confirm the `github.com/kubernetes-sigs/kro` pin is `v{NEW}`.** Phase 2 bumps it alongside the copy. The vendored executor compiles against the module packages at that pin, so the pin and the vendored code must always come from the same tag. Renovate bumps the pin on its own schedule, so it can move ahead of the vendored code between upgrades. If it has, the build usually breaks, but not always.

```bash
grep "kubernetes-sigs/kro " go.mod
```

kro is a single Go module, so its `go` directive becomes our minimum Go version. If it rose, bump `GO_VERSION` in `.github/workflows/ci.yml`. golangci-lint refuses to lint a module whose Go version is newer than the Go it was built with, so bump `GOLANGCI_VERSION` too if needed.

Then check whether upstream's other dependency versions moved:

```bash
# Compare go.mod files
diff /tmp/kro-new/go.mod go.mod

# Update dependencies as needed
go get <package>@<version>
go mod tidy
```

### Step 5.3: Final Cleanup Commit (if needed)

If there are any remaining changes after validation (dependency updates,
lint fixes, etc.), commit them:

**COMMIT CHECKPOINT 6: Final cleanup**

```bash
git add -A
git commit -m "$(cat <<'EOF'
chore(upgrade): final cleanup for KRO v{NEW} upgrade

- Updated go.mod dependencies to match upstream
- Fixed lint issues
- [Any other cleanup items]
EOF
)"
```

### Step 5.4: Squash for PR (Optional)

If you prefer a clean single-commit history for the PR, you can squash
all upgrade commits:

```bash
# Count how many commits in the upgrade (adjust N as needed)
git rebase -i HEAD~6

# Or create a squashed commit message:
git reset --soft HEAD~6
git commit -m "$(cat <<'EOF'
chore(deps): bump KRO packages to v{NEW}

Upgraded KRO from v{OLD} to v{NEW}, both the vendored packages and the
github.com/kubernetes-sigs/kro module.

Changes:
- Copied upstream KRO v{NEW} packages
- Re-applied function-kro adaptations
- [List any new features integrated]
- [List any notable changes]

See patches/v{NEW}_PATCHES.md for detailed adaptation documentation.
EOF
)"
```

**Note:** Keeping commits separate during review can help reviewers understand
the upgrade process. Squashing is optional and depends on team preference.

---

## Vendoring Rules

These rules hold for every upgrade. Which packages we vendor and import, and which files we modify, add and exclude, change from version to version, so they're listed in `v{CURRENT}_PATCHES.md` and checked by `/audit-patches`.

- Vendor an upstream package only when we have to change code inside it. Look for an upstream extension point first (an interface, an option, an injected dependency), and if one gets us what we need, import the package unmodified as the Go module `github.com/kubernetes-sigs/kro` instead. If a vendored package no longer needs any changes, stop vendoring it.
- Any code that reproduces upstream logic lives in a vendored package, as a modification of the upstream file it reproduces, so the diff script shows it.
- The one exception is a small piece of upstream logic we can't call directly. It may be repeated in a local-only `crossplane.go` in a vendored package, or, if it comes from a package we import rather than vendor, in our own code outside `kro/`. Either way the patches doc lists it under "Upstream Logic Our Additions Repeat" so upgrades carry upstream changes into it.
- Anything more than a small piece is never repeated. Vendor the upstream package if we don't already, and change the upstream file.
- Everything related to being a standalone Kubernetes controller is replaced by Crossplane's composition function framework.

---

## Troubleshooting

### "undefined: SomeType" errors

The type may have been renamed or moved. Check the upstream changes analysis for renames.

### "cannot use X as Y" errors

A type's definition changed. Look at the new definition and update callers.

### Tests fail with "unexpected call"

Mock interfaces may have changed. Update test mocks to match new interfaces.

### Runtime panics

Often caused by nil pointer issues when upstream added new required fields. Check struct initialization.

---

## Commit Checkpoint Summary

| Checkpoint | After Step | Purpose |
|------------|------------|---------|
| 1 | 2.2 | Raw upstream code - establishes baseline |
| 2 | 2.3 | Import paths fixed - mechanical change only |
| 3 | 2.4 | Our additions restored - custom files back |
| 4 | 3.2 | Adaptations applied - **most important diff** |
| 5 | 3.4 | Documentation updated - patches documented |
| 6 | 5.3 | Final cleanup - deps, lint, etc. |

**Why these checkpoints matter:**
- Checkpoint 4 (adaptations) is the key diff - it shows exactly what we
  changed from upstream to make it work with Crossplane
- If tests fail after adaptations, compare against checkpoint 3 to see
  exactly what broke
- Checkpoints 1-3 are mechanical and should be reviewable at a glance
