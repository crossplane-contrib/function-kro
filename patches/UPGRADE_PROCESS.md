# Function-KRO Upstream Upgrade Process

This document describes the process for upgrading function-kro to a new upstream KRO version. The process is designed to be repeatable and can be executed by humans or AI agents.

## Overview

Function-KRO vendors code from upstream [kubernetes-sigs/kro](https://github.com/kubernetes-sigs/kro) with adaptations for running as a Crossplane composition function. When upstream KRO releases a new version, we need to:

1. Understand what changed upstream
2. Copy the new code
3. Re-apply our adaptations
4. Validate everything works

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
├── v{OLD}_PATCHES.md            # Adaptations we made for the previous version
├── v{OLD}_to_v{NEW}_CHANGES.md  # Analysis of upstream changes (AI-generated)
└── v{NEW}_PATCHES.md            # Adaptations for the new version (created during upgrade)
```

**Example for v0.7.1 → v0.8.0:**
```
patches/
├── UPGRADE_PROCESS.md
├── v0.7.1_PATCHES.md            # What we changed for v0.7.1
├── v0.8.x_UPSTREAM_CHANGES.md   # What upstream changed in v0.8.x
└── v0.8.x_PATCHES.md            # What we changed for v0.8.x (created during upgrade)
```

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
./scripts/diff-upstream-kro.sh -f graph/builder.go -r v{OLD}
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

```bash
# Clone the old version we're currently based on
git clone --depth 1 --branch v{OLD} \
    https://github.com/kubernetes-sigs/kro.git /tmp/kro-old

# Clone the new version we're upgrading to
git clone --depth 1 --branch v{NEW} \
    https://github.com/kubernetes-sigs/kro.git /tmp/kro-new
```

### Step 1.3: Generate Upstream Changes Analysis

Have an AI agent (or manually) analyze the differences and create `v{OLD}_to_v{NEW}_CHANGES.md`:

```bash
# Compare the relevant directories
diff -r /tmp/kro-old/pkg/graph /tmp/kro-new/pkg/graph
diff -r /tmp/kro-old/pkg/cel /tmp/kro-new/pkg/cel
diff -r /tmp/kro-old/pkg/runtime /tmp/kro-new/pkg/runtime
diff -r /tmp/kro-old/pkg/metadata /tmp/kro-new/pkg/metadata
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
2. New types/functions introduced in pkg/graph/, pkg/cel/, pkg/runtime/
3. Changed function signatures (especially constructors)
4. Removed types/functions
5. Dependency version changes

Focus on changes that affect:
- graph.Builder and NewResourceGraphDefinition
- Runtime creation and resource rendering
- CEL environment and expression evaluation
- Schema resolution interfaces
```

---

## Phase 2: Copy New Files

**Goal:** Replace vendored KRO code with the new version.

### Step 2.1: Remove Old Files

```bash
# Remove existing vendored code (but preserve our additions)
rm -rf kro/graph/dag/
rm -rf kro/graph/parser/
rm -rf kro/graph/fieldpath/
rm -rf kro/graph/crd/
rm -rf kro/graph/variable/
rm -rf kro/cel/
rm -rf kro/runtime/
rm -rf kro/metadata/

# Keep these files (our additions):
# - kro/graph/schema/resolver/schema_map_resolver.go
# - kro/graph/schema/resolver/crd_resolver.go
# - kro/graph/schema/resolver/resolver.go (will need merge)
# - kro/graph/schema/schema.go (will need merge)
```

### Step 2.2: Copy New Files

```bash
# Copy from upstream
cp -r /tmp/kro-new/pkg/graph/* kro/graph/
cp -r /tmp/kro-new/pkg/cel/* kro/cel/
cp -r /tmp/kro-new/pkg/runtime/* kro/runtime/
cp -r /tmp/kro-new/pkg/metadata/* kro/metadata/
```

**COMMIT CHECKPOINT 1: Raw upstream code**

```bash
git add -A
git commit -m "$(cat <<'EOF'
chore(upgrade): copy upstream KRO v{NEW} files

Raw copy of upstream KRO v{NEW} packages before any modifications.
Import paths still reference upstream github.com/kubernetes-sigs/kro/pkg/.

This commit captures the unmodified upstream state for diff comparison.
EOF
)"
```

This commit establishes a clean baseline of upstream code. All subsequent
commits will show exactly what we modified.

### Step 2.3: Fix Import Paths

Upstream uses `github.com/kubernetes-sigs/kro/pkg/...` (or `sigs.k8s.io/kro/pkg/...` if they changed their module path) but we use `github.com/crossplane-contrib/function-kro/kro/...`:

```bash
# Update import paths in copied files (handle both possible upstream module paths)
find kro/ -name "*.go" -exec sed -i '' \
    's|github.com/kubernetes-sigs/kro/pkg/|github.com/crossplane-contrib/function-kro/kro/|g' {} \;
find kro/ -name "*.go" -exec sed -i '' \
    's|sigs.k8s.io/kro/pkg/|github.com/crossplane-contrib/function-kro/kro/|g' {} \;

# Also update any api/ imports if present
# IMPORTANT: This also handles the v1alpha1 → v1beta1 version change
find kro/ -name "*.go" -exec sed -i '' \
    's|github.com/kubernetes-sigs/kro/api/v1alpha1|github.com/crossplane-contrib/function-kro/input/v1beta1|g' {} \;
find kro/ -name "*.go" -exec sed -i '' \
    's|sigs.k8s.io/kro/api/v1alpha1|github.com/crossplane-contrib/function-kro/input/v1beta1|g' {} \;
# Handle any other api/ subpaths that aren't version-specific
find kro/ -name "*.go" -exec sed -i '' \
    's|github.com/kubernetes-sigs/kro/api/|github.com/crossplane-contrib/function-kro/input/|g' {} \;
find kro/ -name "*.go" -exec sed -i '' \
    's|sigs.k8s.io/kro/api/|github.com/crossplane-contrib/function-kro/input/|g' {} \;
```

**COMMIT CHECKPOINT 2: Import paths fixed**

```bash
git add -A
git commit -m "$(cat <<'EOF'
chore(upgrade): fix import paths for function-kro

Mechanical transformation of import paths:
- github.com/kubernetes-sigs/kro/pkg/ → github.com/crossplane-contrib/function-kro/kro/
- github.com/kubernetes-sigs/kro/api/ → github.com/crossplane-contrib/function-kro/input/

No functional changes, just import path updates.
EOF
)"
```

This commit isolates the import path changes, making it easy to verify
only the expected substitutions were made.

### Step 2.4: Restore Our Additions

If our addition files were overwritten, restore them from git or re-create:

```bash
git checkout -- kro/graph/schema/resolver/schema_map_resolver.go
git checkout -- kro/graph/schema/resolver/crd_resolver.go
# Note: resolver.go and schema.go may need manual merge
```

**COMMIT CHECKPOINT 3: Our additions restored**

```bash
git add -A
git commit -m "$(cat <<'EOF'
chore(upgrade): restore function-kro custom files

Restored files that are unique to function-kro (not from upstream):
- kro/graph/schema/resolver/schema_map_resolver.go
- kro/graph/schema/resolver/crd_resolver.go

These files provide Crossplane-specific schema resolution.
EOF
)"
```

This commit restores our custom files that may have been overwritten
during the copy operation. The codebase likely won't compile yet.

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
5. Current compilation errors from `go build`

IMPORTANT: Do NOT blindly paste old adaptation code into the new upstream.
For EACH adaptation in v{OLD}_PATCHES.md:
1. Read the NEW upstream code in that area first
2. Understand WHY the adaptation existed (what gap it bridges)
3. Check if upstream already solved the problem (new interfaces, options, etc.)
4. Decide the outcome: Same / Moved / Shrunk / Gone / Grown
5. Only then write the adaptation — tailored to the new code, not copied from the old

The fundamental gaps we bridge (these are the WHYs):
- No API server access: no REST mapper, no dynamic client, no CRD fetching
- Schema from Crossplane: XR schema arrives as *spec.Schema, not via SimpleSchema
- No CRD generation: Crossplane manages the XR CRD, not us
- No namespace defaulting: Crossplane handles namespace assignment
- Input type differences: our ResourceGraph vs upstream's ResourceGraphDefinition CR

If upstream added extension points or restructured code to make any of these
gaps smaller, prefer upstream's approach over our old adaptation.

If upstream added NEW code that has the same gaps (e.g., a new function that
calls the REST mapper), that needs a NEW adaptation not in the old patches doc.

After applying adaptations:
- Run `go generate ./...` to update generated methods and CRD schemas
- Run `go build ./...` to verify compilation
- Run `go test ./...` and fix any failures
```

**COMMIT CHECKPOINT 4: Adaptations applied**

```bash
git add -A
git commit -m "$(cat <<'EOF'
chore(upgrade): apply function-kro adaptations to v{NEW}

Re-applied adaptations for Crossplane function compatibility:
- Modified Builder constructor to accept resolver instead of clientConfig
- Updated NewResourceGraphDefinition to accept schema parameter
- Namespace scope inferred from template (no RESTMapper)
- Injected ObjectMeta schema in resolution paths
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

1. **Update input types** (`input/v1beta1/input.go`) with new fields
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
- [ ] Archive or update `patches/v{OLD}_to_v{NEW}_CHANGES.md`
- [ ] Update `AGENTS.md` if architecture changed significantly
- [ ] Update `README.md` with new KRO version

### Step 5.2: Update Dependencies

Check if go.mod needs updates based on upstream:

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

Upgraded vendored KRO code from v{OLD} to v{NEW}.

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

## Quick Reference

### Files We Add (not in upstream)

| File | Purpose |
|------|---------|
| `kro/graph/schema/resolver/schema_map_resolver.go` | Resolve schemas from Crossplane's required_schemas |
| `kro/graph/schema/resolver/crd_resolver.go` | Resolve schemas from CRDs (fallback path) |

### Files We Modify

Based on the v0.8.3 audit, these are all files we modify from upstream:

| File | Adaptation |
|------|------------|
| `kro/graph/builder.go` | Constructor accepts resolver; NewResourceGraphDefinition accepts schema; remove SimpleSchema/CRD gen; remove REST mapper |
| `kro/graph/node.go` | Remove `GVR` and `Namespaced` from NodeMeta |
| `kro/graph/validation.go` | API type adaptation; remove `validateResourceGraphDefinitionNamingConventions` and `validateTemplateConstraints` |
| `kro/graph/schema/resolver/resolver.go` | Add combined resolver factories and `combinedResolver` type |
| `kro/graph/schema/schema.go` | Add `DeepCopySchema` |
| `kro/runtime/node.go` | Remove `normalizeNamespaces` and all namespace auto-defaulting |
| `kro/metadata/finalizers.go` | Import path change |
| `kro/metadata/labels.go` | Import path change |
| `kro/metadata/groupversion.go` | Import path change; remove `GetResourceGraphDefinitionInstanceGVR` |

### Files We Intentionally Exclude

| Upstream File | Reason |
|---------------|--------|
| `graph/crd/*` (6 files) | CRD synthesis/compat — function-kro doesn't generate CRDs |
| `metadata/owner_reference.go` | Owner reference helpers — Crossplane manages resource ownership |

### What We Vendor (Allowlist)

**Guiding principle:** Function-kro only vendors KRO's graph-building, CEL evaluation, and runtime execution layers. Everything else (controllers, API types, test utilities) is replaced by Crossplane's composition function framework.

These are the **only** upstream `pkg/` packages we copy:

| Upstream Path | Our Path | What It Provides |
|---------------|----------|------------------|
| `pkg/graph/` | `kro/graph/` | Graph building, DAG, parsing, schema resolution, validation |
| `pkg/cel/` | `kro/cel/` | CEL environment, AST inspection, custom libraries |
| `pkg/runtime/` | `kro/runtime/` | Runtime execution, template resolution |
| `pkg/metadata/` | `kro/metadata/` | Labels, finalizers, GVK utilities |

**IMPORTANT: If upstream introduces a new `pkg/` directory, do NOT copy it** unless it is a subdirectory of one of the four packages above or you have explicitly evaluated that function-kro needs it. The default should be to skip unknown packages.

Examples of upstream packages we skip (non-exhaustive):

| Upstream Package | Why We Skip It |
|------------------|----------------|
| `api/` | We have our own input types (`input/v1beta1/`) |
| `pkg/controller/` | Replaced by Crossplane function framework (`fn.go`) |
| `pkg/simpleschema/` | Crossplane provides OpenAPI schemas directly |
| `pkg/dynamiccontroller/` | Replaced by Crossplane function framework |
| `pkg/testutil/` | Upstream test utilities tied to controller patterns we don't use |

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
