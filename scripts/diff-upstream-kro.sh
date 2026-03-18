#!/bin/bash
#
# diff-upstream-kro.sh - Compare function-kro's KRO libraries against upstream KRO
#
# Usage:
#   ./scripts/diff-upstream-kro.sh [options]
#
# Options:
#   -u, --upstream-dir DIR   Use existing upstream KRO checkout (default: clone to temp dir)
#   -r, --ref REF            Git ref to compare against (default: main)
#   -f, --file FILE          Only diff a specific file (relative to kro/ dir)
#   -s, --summary            Show summary only, not full diffs
#   -l, --lines NUM          Max lines of diff to show per file (0 for unlimited, default: 100)
#   -n, --no-normalize       Don't normalize import paths before diffing
#   -h, --help               Show this help message
#
# Examples:
#   ./scripts/diff-upstream-kro.sh                          # Full diff against main
#   ./scripts/diff-upstream-kro.sh -r v0.7.1                # Diff against tag v0.7.1
#   ./scripts/diff-upstream-kro.sh -f graph/builder.go      # Diff only builder.go
#   ./scripts/diff-upstream-kro.sh -s                       # Summary only
#

set -eo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Script directory and project root
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
LOCAL_KRO_DIR="$PROJECT_ROOT/kro"

# Default options
UPSTREAM_DIR=""
GIT_REF="main"
SPECIFIC_FILE=""
SUMMARY_ONLY=false
MAX_LINES=100  # 0 = unlimited
NORMALIZE_IMPORTS=true
CLEANUP_TEMP=false

# Upstream KRO repository
UPSTREAM_REPO="https://github.com/kubernetes-sigs/kro.git"

# Import path mappings (local -> upstream patterns)
LOCAL_IMPORT_PREFIX="github.com/crossplane-contrib/function-kro/kro"
UPSTREAM_IMPORT_PREFIX="sigs.k8s.io/kro/pkg"

# Input type import path mappings (we use input/v1beta1, upstream uses api/v1alpha1)
LOCAL_INPUT_PREFIX="github.com/crossplane-contrib/function-kro/input/v1beta1"
UPSTREAM_INPUT_PREFIX="sigs.k8s.io/kro/api/v1alpha1"

# Upstream packages we vendor (only show upstream-only files from these)
# Everything else in upstream (controllers, simpleschema, etc.) is intentionally excluded.
VENDORED_PACKAGES="graph/ cel/ runtime/ metadata/"

usage() {
    head -30 "$0" | grep -E '^#' | sed 's/^# \?//'
    exit 0
}

log_info() {
    echo -e "${BLUE}INFO:${NC} $*"
}

log_success() {
    echo -e "${GREEN}OK:${NC} $*"
}

log_warning() {
    echo -e "${YELLOW}WARN:${NC} $*"
}

log_error() {
    echo -e "${RED}ERROR:${NC} $*"
}

log_header() {
    echo ""
    echo -e "${BLUE}=== $* ===${NC}"
    echo ""
}

# Check if a file belongs to one of the vendored upstream packages
is_vendored_package() {
    local file="$1"
    for pkg in $VENDORED_PACKAGES; do
        if [[ "$file" == "$pkg"* ]]; then
            return 0
        fi
    done
    return 1
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        -u|--upstream-dir)
            UPSTREAM_DIR="$2"
            shift 2
            ;;
        -r|--ref)
            GIT_REF="$2"
            shift 2
            ;;
        -f|--file)
            SPECIFIC_FILE="$2"
            shift 2
            ;;
        -s|--summary)
            SUMMARY_ONLY=true
            shift
            ;;
        -l|--lines)
            MAX_LINES="$2"
            shift 2
            ;;
        -n|--no-normalize)
            NORMALIZE_IMPORTS=false
            shift
            ;;
        -h|--help)
            usage
            ;;
        *)
            log_error "Unknown option: $1"
            usage
            ;;
    esac
done

# Clone or use existing upstream directory
setup_upstream() {
    if [[ -n "$UPSTREAM_DIR" ]]; then
        if [[ ! -d "$UPSTREAM_DIR" ]]; then
            log_error "Upstream directory does not exist: $UPSTREAM_DIR"
            exit 1
        fi
        log_info "Using existing upstream directory: $UPSTREAM_DIR"
    else
        UPSTREAM_DIR=$(mktemp -d)
        CLEANUP_TEMP=true
        log_info "Cloning upstream KRO to $UPSTREAM_DIR..."
        git clone --depth 1 --branch "$GIT_REF" "$UPSTREAM_REPO" "$UPSTREAM_DIR" 2>/dev/null || \
        git clone --depth 1 "$UPSTREAM_REPO" "$UPSTREAM_DIR" 2>/dev/null

        if [[ "$GIT_REF" != "main" ]]; then
            (cd "$UPSTREAM_DIR" && git fetch --depth 1 origin "$GIT_REF" && git checkout "$GIT_REF") 2>/dev/null || true
        fi
    fi

    UPSTREAM_PKG_DIR="$UPSTREAM_DIR/pkg"
    if [[ ! -d "$UPSTREAM_PKG_DIR" ]]; then
        log_error "Upstream pkg/ directory not found at $UPSTREAM_PKG_DIR"
        exit 1
    fi

    log_info "Comparing against upstream ref: $GIT_REF"
}

cleanup() {
    if [[ "$CLEANUP_TEMP" == "true" && -n "$UPSTREAM_DIR" ]]; then
        log_info "Cleaning up temporary directory..."
        rm -rf "$UPSTREAM_DIR"
    fi
}

trap cleanup EXIT

# Normalize imports in a file for comparison
normalize_imports() {
    local file="$1"
    if [[ "$NORMALIZE_IMPORTS" == "true" ]]; then
        # Normalize all known import paths to a canonical form for comparison
        # This handles: our local imports, old upstream path, current upstream path,
        # and input type imports (our input/v1beta1 vs upstream api/v1alpha1)
        sed -e "s|$LOCAL_IMPORT_PREFIX|$UPSTREAM_IMPORT_PREFIX|g" \
            -e "s|$LOCAL_INPUT_PREFIX|$UPSTREAM_INPUT_PREFIX|g" \
            -e "s|github.com/kro-run/kro/pkg|$UPSTREAM_IMPORT_PREFIX|g" \
            -e "s|github.com/kubernetes-sigs/kro/pkg|$UPSTREAM_IMPORT_PREFIX|g" \
            -e "s|v1beta1\\.KRODomainName|v1alpha1.KRODomainName|g" \
            "$file"
    else
        cat "$file"
    fi
}

# Map local path to upstream path
# Our kro/ directory maps to upstream's pkg/ directory
get_upstream_path() {
    local local_rel_path="$1"
    # Simply replace kro/ prefix concept - local path is relative to kro/
    # upstream path is relative to pkg/
    echo "$UPSTREAM_PKG_DIR/$local_rel_path"
}

# Compare a single file
diff_file() {
    local local_rel_path="$1"  # e.g., "graph/builder.go"
    local local_file="$LOCAL_KRO_DIR/$local_rel_path"
    local upstream_file
    upstream_file=$(get_upstream_path "$local_rel_path")

    # Check if files exist
    if [[ ! -f "$local_file" ]]; then
        log_error "Local file not found: $local_file"
        return 1
    fi

    if [[ ! -f "$upstream_file" ]]; then
        if [[ "$SUMMARY_ONLY" != "true" ]]; then
            echo -e "${YELLOW}[LOCAL ONLY]${NC} $local_rel_path (no upstream equivalent)"
        fi
        return 2
    fi

    # Create temp files with normalized imports
    local tmp_local
    local tmp_upstream
    tmp_local=$(mktemp)
    tmp_upstream=$(mktemp)

    normalize_imports "$local_file" > "$tmp_local"
    normalize_imports "$upstream_file" > "$tmp_upstream"

    # Perform diff (plain for stats, colored for display)
    local diff_output
    diff_output=$(diff -u "$tmp_upstream" "$tmp_local" 2>/dev/null || true)

    if [[ -z "$diff_output" ]]; then
        rm -f "$tmp_local" "$tmp_upstream"
        if [[ "$SUMMARY_ONLY" != "true" ]]; then
            echo -e "${GREEN}[IDENTICAL]${NC} $local_rel_path"
        fi
        return 0
    else
        local added
        local removed
        added=$(echo "$diff_output" | grep -c '^+[^+]' || true)
        removed=$(echo "$diff_output" | grep -c '^-[^-]' || true)

        if [[ "$SUMMARY_ONLY" == "true" ]]; then
            rm -f "$tmp_local" "$tmp_upstream"
            echo -e "${RED}[MODIFIED]${NC} $local_rel_path (+$added/-$removed lines)"
        else
            echo ""
            echo -e "${RED}[MODIFIED]${NC} $local_rel_path (+$added/-$removed lines)"
            echo "─────────────────────────────────────────────────────────────────"

            # Use git diff for colored output (green adds, red removals)
            local colored_diff
            colored_diff=$(git diff --no-index --color=always \
                -- "$tmp_upstream" "$tmp_local" 2>/dev/null | \
                sed -e "s|a$tmp_upstream|a/upstream/$local_rel_path|g" \
                    -e "s|b$tmp_local|b/local/$local_rel_path|g" \
                    -e "s|$tmp_upstream|upstream/$local_rel_path|g" \
                    -e "s|$tmp_local|local/$local_rel_path|g" || true)

            rm -f "$tmp_local" "$tmp_upstream"

            # Show the colored diff, optionally limited
            if [[ "$MAX_LINES" -eq 0 ]]; then
                echo "$colored_diff"
            else
                echo "$colored_diff" | head -"$MAX_LINES"
                local total_lines
                total_lines=$(echo "$colored_diff" | wc -l)
                if [[ $total_lines -gt $MAX_LINES ]]; then
                    echo ""
                    echo -e "${YELLOW}... (output truncated at $MAX_LINES lines, $total_lines total - use -l 0 for unlimited)${NC}"
                fi
            fi
            echo ""
        fi
        return 3  # Modified
    fi
}

# Find all Go files in local kro directory (excluding tests)
find_local_files() {
    find "$LOCAL_KRO_DIR" -type f -name "*.go" ! -name "*_test.go" | \
        sed "s|$LOCAL_KRO_DIR/||" | \
        sort
}

# Find all Go files in upstream pkg directory (excluding tests)
find_upstream_files() {
    find "$UPSTREAM_PKG_DIR" -type f -name "*.go" ! -name "*_test.go" | \
        sed "s|$UPSTREAM_PKG_DIR/||" | \
        sort
}

# Main comparison logic
main() {
    setup_upstream

    log_header "Comparing function-kro/kro against upstream KRO ($GIT_REF)"

    local identical=0
    local modified=0
    local local_only=0
    local upstream_only=0
    local errors=0

    # Track which files we've seen
    local seen_files_file
    seen_files_file=$(mktemp)

    if [[ -n "$SPECIFIC_FILE" ]]; then
        # Diff specific file
        diff_file "$SPECIFIC_FILE"
        rm -f "$seen_files_file"
        exit $?
    fi

    # Compare all local files
    log_header "Comparing local files against upstream"

    while IFS= read -r rel_path; do
        echo "$rel_path" >> "$seen_files_file"

        local rc=0
        diff_file "$rel_path" || rc=$?
        case $rc in
            0) ((identical++)) ;;
            2) ((local_only++)) ;;
            3) ((modified++)) ;;
            *) ((errors++)) ;;
        esac
    done < <(find_local_files)

    # Check for upstream files we don't have (only in vendored packages)
    log_header "Checking for upstream-only files (in vendored packages)"

    while IFS= read -r rel_path; do
        if ! grep -q "^${rel_path}$" "$seen_files_file" 2>/dev/null; then
            # Only report files from packages we vendor
            if is_vendored_package "$rel_path"; then
                echo -e "${YELLOW}[UPSTREAM ONLY]${NC} $rel_path"
                ((upstream_only++))
            fi
        fi
    done < <(find_upstream_files)

    rm -f "$seen_files_file"

    # Print summary
    log_header "Summary"

    echo "Comparison against: $GIT_REF"
    echo ""
    echo -e "${GREEN}Identical:${NC}     $identical files"
    echo -e "${RED}Modified:${NC}      $modified files"
    echo -e "${YELLOW}Local only:${NC}    $local_only files (function-kro specific)"
    echo -e "${YELLOW}Upstream only:${NC} $upstream_only files (not in function-kro)"

    if [[ $errors -gt 0 ]]; then
        echo -e "${RED}Errors:${NC}        $errors files"
    fi

    echo ""

    if [[ $modified -gt 0 ]]; then
        echo "Modified files require manual review when updating from upstream."
        echo "Run with specific file to see full diff: $0 -f <file>"
    fi

    if [[ $upstream_only -gt 0 ]]; then
        echo ""
        echo "Upstream-only files (in vendored packages) may need to be adopted."
    fi
}

main "$@"
