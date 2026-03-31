---
name: test-examples
description: End-to-end validation of function-kro examples. Creates a kind cluster, installs Crossplane and extensions, then executes each example from example/README.md — running commands, interpreting output, and making pass/fail assertions. Stops on first failure.
arguments: Optional example filter — a case-insensitive substring matched against example headings. If provided, only matching examples are run. If omitted, all examples are run.
---

# Test Examples Skill

You are validating function-kro's examples end-to-end by executing the instructions in `example/README.md` against a real cluster with real AWS resources.

**Your job:** Create infrastructure, run each example, make automated pass/fail assertions based on the README's descriptions of expected behavior, and report results. Stop on the first failure.

**Your mindset:** You are a QA engineer who takes the README literally. If the README says "confirm that `enableDnsHostnames` is absent," you check the actual resource and verify that field is not present. If the README says "see the fatal error," you check the trace output for a fatal error. When you're unsure whether output matches expectations, bias toward failure — false passes are worse than false failures.

**This skill does not make any git commits.** Skip GPG signing checks and any other git-commit-related setup steps — they are not needed here.

---

## Phase 1: Setup

**Before creating a cluster**, check if a kind cluster already exists:

```bash
kind get clusters
```

If any cluster exists, **STOP immediately** and report an error:
> A kind cluster already exists. Please delete it manually (`kind delete cluster`) or use a different cluster name if you want to keep it. This skill refuses to delete pre-existing clusters to avoid destroying your work.

Do NOT delete or recreate an existing cluster. This is a safety guardrail — the user may have an active cluster with important state.

**If no cluster exists**, proceed with setup. Read the `## Pre-Requisites` section of `example/README.md` and execute every step — cluster creation, Crossplane install, extensions, and AWS credential configuration. The README is the source of truth for setup; do not hardcode those steps here.

After setup completes, verify the full stack is working:
- All packages are installed and healthy (`kubectl get pkg` — all should show `INSTALLED=True` and `HEALTHY=True`)
- ProviderConfig exists and is ready

If any setup step fails, **STOP** and report the failure with full output. Do not attempt examples with a broken setup.

---

## Phase 2: Parse README

Read `example/README.md` and extract each example section. An example section starts with a `## ... Example` heading and ends at the next `##` heading or end of file.

For each example section, identify:
1. **Name** — the heading text (e.g., "Basic Example", "Omit Example")
2. **Description** — the prose explaining what the example demonstrates and what to expect
3. **Commands** — all shell commands in code blocks, in order
4. **Assertions** — what the README says you should observe (e.g., "confirm that X is absent," "see the fatal error," "note that conditional resources are only included when...")
5. **Interactive steps** — commands that modify state mid-example (patches, alternate file applies)

### Filtering

If an argument was provided (e.g., `/test-examples external`), filter the parsed examples to only those whose heading contains the argument as a case-insensitive substring. For example, "external" matches "External Reference Example".

If no examples match, **STOP** and report an error listing the available example names.

If no argument was provided, run all examples.

---

## Phase 3: Execute Examples

Process each example in the order they appear in the README. For each example:

### 3.1 Apply Resources

Run the `kubectl apply` commands for XRD, composition, and any supporting resources (configmaps, etc.).

Verify each apply succeeds (exit code 0). If an apply fails, this is a **FAIL**.

### 3.2 Apply XR and Wait for Readiness

Run the `kubectl apply` command for the XR.

Then poll for readiness. Run `crossplane beta trace <resource>` every 15 seconds until:
- **Success case:** All resources show `SYNCED=True` and `READY=True` (or the expected state for this example)
- **Failure case:** An expected fatal error appears (e.g., collection limits example)
- **Timeout:** 5 minutes with no progress — this is a **FAIL**

The README's description tells you which case to expect. Most examples expect all resources to become Ready. The collection limits example expects a fatal error after applying the exceed file.

**IMPORTANT — Parsing trace output:** The `crossplane beta trace` output uses tree-drawing characters (`├─`, `└─`) that shift columns and break `awk`-based column parsing. Do NOT parse columns with awk or cut. Instead, use this simple and robust approach:

```bash
OUTPUT=$(crossplane beta trace <resource> 2>&1)
echo "$OUTPUT"
# Ready: output contains "True" and does NOT contain "False"
if echo "$OUTPUT" | grep -q "True" && ! echo "$OUTPUT" | grep -q "False"; then
    echo "ALL READY"
fi
```

This works because a fully-ready trace has `True True` on every resource row and no `False` anywhere. For error detection (e.g., collection limits), grep for the expected error text in the output.

### 3.3 Run Verification Commands

Run any `kubectl get ... | jq ...` or similar verification commands from the README.

**Interpret the output against the README's description.** This is the core assertion logic:

- If the README says "confirm that field X is absent" → check the JSON output and verify the field key does not exist
- If the README says "see the aggregated networking info" → verify the expected status fields are populated (not empty/null)
- If the README says "note that conditional resources are only included when flags are true" → verify the trace shows only the expected resources
- If the README says "see the fatal error indicating the collection size was exceeded" → verify the trace or status contains a fatal condition with a message about collection size
- If the README says "see `enableDnsHostnames` appear" → verify the field is now present in the output

**When evaluating output, be literal.** If the README says a field should be absent and it's present (even with a zero value), that's a **FAIL**. If the README says resources should be Ready and they're not after the timeout, that's a **FAIL**.

### 3.4 Run Interactive Steps

If the example has interactive steps (kubectl patch, applying alternate files):

1. Run the interactive command
2. Wait for reconciliation (poll trace for 60 seconds or until state changes)
3. Run the subsequent verification commands
4. Assert against the README's expectations for the post-interaction state

### 3.5 Clean Up

Run the clean-up commands from the README's `### Clean-up` section for this example. Verify all managed resources are deleted before proceeding to the next example:

```bash
kubectl get managed
```

If managed resources from this example still exist after 3 minutes, log a warning but proceed — leftover resources shouldn't block other examples since each uses a different API group.

### 3.6 Record Result

Record the result for this example:
- **PASS** — all assertions met, all commands succeeded
- **FAIL** — any assertion failed, any command failed unexpectedly, or timeout

---

## Phase 4: Stop on Failure

If any example **FAILs**:

1. **STOP immediately** — do not run further examples
2. Report:
   - Which example failed
   - Which specific step failed
   - The full command output that triggered the failure
   - What the README said should happen vs what actually happened
   - Your assessment of the root cause (is it a code bug, a README error, a timing issue, an infrastructure problem?)
   - Suggested fixes — be specific (e.g., "the jq command checks `.status.networkingInfo` but the XRD defines the field as `.status.info`")
3. Do NOT clean up the cluster — leave it running so the user can investigate

---

## Phase 5: Success Report

If all examples pass:

```
## Examples Test Report

### Results
- Total examples: {N}
- Passed: {N}
- Failed: 0

### Per-Example Summary
| Example | Resources Created | Time to Ready | Assertions | Result |
|---------|------------------|---------------|------------|--------|
| Basic | VPC, 3 Subnets, SG | ~2m | Status populated | PASS |
| ... | ... | ... | ... | ... |

### Cluster Cleanup
{status of kind cluster deletion}
```

Before deleting the cluster, verify that no managed resources remain in AWS:

```bash
kubectl get managed -A
```

If this returns any resources, **STOP** — do NOT delete the cluster. Report the leftover resources and ask the user to investigate. Deleting the cluster while managed resources exist would orphan them in AWS with no controller to clean them up.

Only after `kubectl get managed -A` returns `No resources found` should you delete the cluster:

```bash
kind delete cluster
```

---

## Important Guidelines

- **Narrate your progress.** Print human-readable status updates as you go. Before each phase, announce what you're about to do. After each command, summarize what you observed. For each example, print a header (e.g., `--- Running: Omit Example ---`), announce each step, and print a clear PASS/FAIL result when done. The user should be able to follow along in real time without reading tool call details.
- **The README is the test spec.** Do not add assertions beyond what the README describes. If the README doesn't mention checking something, don't check it. If the README is incomplete, that's a finding to report, not something to silently work around.
- **Bias toward failure.** If output is ambiguous or you're not sure whether it matches the README's expectations, report it as a failure. False passes hide real problems.
- **Log everything.** For each command you run, record the full output. This makes failures debuggable after the fact.
- **Be patient with reconciliation.** Cloud resources take time. 5-second polling intervals, 2-minute timeouts for examples. Don't fail fast on timing — fail on wrong state.
- **Each example is independent.** Don't assume state from a previous example. Each example applies its own XRD, composition, and XR from scratch.
- **Real resources cost money.** Always run clean-up, even on failure of the clean-up commands themselves. The kind cluster deletion at the end is the ultimate safety net.
