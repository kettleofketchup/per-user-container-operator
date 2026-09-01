# Fixture workspace image for the E2E harness (test/e2e/kind-up.sh).
#
# Neither consumer CR exists yet at this point in the plan (the first consumer CR is Task
# 14, the second is Task 17), and the consumer image is private and
# unreachable from this public repo's CI. So the E2E fixtures in this
# directory run a small, public, kind-loadable image instead: a non-root,
# unprivileged-port HTTP server with /bin/sh, which is everything assertions
# 1, 2, 3, 5 and 6 of the plan's Testing spec need from a workspace.
#
# nginxinc/nginx-unprivileged is used (not plain nginx) because the fixture
# PerUserApp pins podSecurityContext.runAsNonRoot: true with no capability to
# bind a privileged port, and plain nginx's master process needs root (or
# CAP_NET_BIND_SERVICE) to bind :80.
#
# The baked seed corpus below exists for Task 14 Step 4, which asserts a
# first-start workspace has a file at the fixed path
# /workspace/samples/sample.txt: cp -an <storage.seed.from> <staging>/ over a
# source that does not exist succeeds silently and copies nothing, so without
# this bake the workspace comes up empty and that assertion fails with no
# diagnostic pointing at the image. This task's own fixture CRs do not set
# storage.seed; they exist so a later dispatch's CR can point
# storage.seed.from at /opt/puc-e2e-seed/samples and land the corpus at
# <mountPath>/samples/sample.txt after the seed init container's `cp -an`.
#
# fixture-workspace-nginx.conf replaces the image's own default.conf to
# serve from /workspace (the per-user PVC mount, writable by the fixture's
# pinned uid/fsGroup 101) instead of the image's root-owned
# /usr/share/nginx/html. Added for task-13b-brief.md assertion 3, which
# needs an HTTP-observable difference between two workspace pods to prove
# which one a given routing decision actually reached -- see that file's own
# doc comment and this dispatch's report.
FROM nginxinc/nginx-unprivileged:alpine
COPY fixture-workspace-seed/samples /opt/puc-e2e-seed/samples
COPY fixture-workspace-nginx.conf /etc/nginx/conf.d/default.conf
