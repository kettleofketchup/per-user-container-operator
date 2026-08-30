#!/usr/bin/env python3
"""Reapplies the maxItems bound controller-gen cannot express on
NetworkSpec.WorkspaceEgress[].To.

The CEL rule on NetworkSpec (api/v1alpha1/peruserapp_types.go) nests two
unbounded arrays -- workspaceEgress itself, and each rule's `to` peers --
and the API server's CEL cost estimator refuses to install the CRD at all
without a maxItems bound on BOTH. controller-gen bounds the outer array
from a +kubebuilder:validation:MaxItems marker on WorkspaceEgress, since
that field is declared in this repo. The inner `to` field is declared on
the upstream networkingv1.NetworkPolicyEgressRule type, which
controller-gen has no marker to reach, so this script reapplies that one
bound by hand after every `make manifests` run.

Idempotent: running it twice leaves the file unchanged the second time.
See api/v1alpha1/crd_generated_test.go for the CI-gated assertion that
catches a missing bound if this script is ever skipped or this anchor
logic drifts from what controller-gen emits.
"""
import sys

CRD_PATH = "config/crd/apps.kettleofketchup_peruserapps.yaml"
MAX_ITEMS_LINE_SUFFIX = "maxItems: 20\n"


def find_key_line(lines, key, indent, start=0):
    target = " " * indent + key
    for i in range(start, len(lines)):
        if lines[i] == target:
            return i
    return None


def find_next_sibling(lines, indent, start):
    """First line after `start` whose indentation is <= indent (i.e. the
    next sibling key or an enclosing scope's key), or len(lines)."""
    for i in range(start, len(lines)):
        line = lines[i]
        if line.strip() == "":
            continue
        this_indent = len(line) - len(line.lstrip(" "))
        if this_indent <= indent:
            return i
    return len(lines)


def patch(lines):
    network_line = find_key_line(lines, "network:\n", 14)
    if network_line is None:
        sys.exit("patch-crds: could not find the 'network:' property")
    network_end = find_next_sibling(lines, 14, network_line + 1)

    workspace_egress_line = find_key_line(lines, "workspaceEgress:\n", 18, network_line)
    if workspace_egress_line is None or workspace_egress_line >= network_end:
        sys.exit("patch-crds: could not find 'network.workspaceEgress'")
    workspace_egress_end = find_next_sibling(lines, 18, workspace_egress_line + 1)
    if workspace_egress_end > network_end:
        workspace_egress_end = network_end

    # workspaceEgress's item schema (one NetworkPolicyEgressRule) has two
    # sibling array properties, "ports" and "to" -- both atomic lists, so a
    # bare "type: array" / "x-kubernetes-list-type: atomic" pair matches
    # both. Anchor on the "to:" property key itself to target the right one.
    to_line = find_key_line(lines, "to:\n", 24, workspace_egress_line)
    if to_line is None or to_line >= workspace_egress_end:
        sys.exit("patch-crds: could not find network.workspaceEgress[].to")
    to_end = find_next_sibling(lines, 24, to_line + 1)
    if to_end > workspace_egress_end:
        to_end = workspace_egress_end

    # Within the "to" property's own schema, find its "type: array" line:
    # indent 26, immediately followed by "x-kubernetes-list-type: atomic"
    # at the same indent (the marker controller-gen stamps on every array
    # field it treats as atomic).
    inner_index = None
    for i in range(to_line, to_end):
        line = lines[i]
        indent = len(line) - len(line.lstrip(" "))
        if (
            indent == 26
            and line.strip() == "type: array"
            and i + 1 < to_end
            and lines[i + 1] == " " * 26 + "x-kubernetes-list-type: atomic\n"
        ):
            inner_index = i
            break
    if inner_index is None:
        sys.exit("patch-crds: could not find network.workspaceEgress[].to's array schema")

    # controller-gen emits object keys alphabetically; "maxItems" sorts
    # before "type", so the matching position is immediately BEFORE this
    # line -- mirroring where the marker-driven outer bound already lands.
    if lines[inner_index - 1] == " " * 26 + MAX_ITEMS_LINE_SUFFIX:
        return False
    lines.insert(inner_index, " " * 26 + MAX_ITEMS_LINE_SUFFIX)
    return True


def main():
    with open(CRD_PATH) as f:
        lines = f.readlines()
    changed = patch(lines)
    if changed:
        with open(CRD_PATH, "w") as f:
            f.writelines(lines)
    print(f"patch-crds: {'patched' if changed else 'already up to date'}")


if __name__ == "__main__":
    main()
