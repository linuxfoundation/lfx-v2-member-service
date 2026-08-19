<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Service Helm Chart

This document owns member-service-specific chart behavior. Shared chart
conventions live in `lfx-v2-helm`; deployed values, the Salesforce
ExternalSecret, IRSA role, region, and chart pins live in `lfx-v2-argocd`.

- **Chart path**: `charts/lfx-v2-member-service/`
- **Service type**: Salesforce-backed membership API with local NATS KV
  caches that also publishes indexer and FGA-sync messages on the write path.
- **HTTPRoute paths** (`templates/httproute.yaml`): `/b2b_orgs` (Exact),
  `/b2b_orgs/`, `/project_memberships/`, `/key_contacts` (Exact),
  `/key_contacts/`, `/admin/`, `/_memberships/`.
  `/debug/vars`, `/livez`, and `/readyz` are mounted by the binary but are
  not exposed by this HTTPRoute.
- **NATS KV buckets**: `membership-cache`, `member-service-cache`,
  `org-settings`, `pubsub-state`. `org-settings` is authoritative
  access-control state and `pubsub-state` holds the CDC consumer's replay
  cursors; neither must carry a production TTL.
- **CDC consumer Deployment** (`templates/deployment-consumer.yaml`, gated by
  `consumer.enabled`): a separate single-replica Deployment running
  `RUN_MODE=consumer` with the Recreate strategy so at most one Salesforce
  Pub/Sub CDC consumer is active. Endpoint comes from
  `consumer.pubsubEndpoint`; `SF_ORG_ID` must be set or the consumer
  fails fast at startup.
- **Heimdall auth** (`templates/ruleset.yaml`): per-object `auditor`/`writer`
  checks on `b2b_org:{uid}` and `project_membership:{membership_uid}`;
  `POST /b2b_orgs` and `POST /admin/reindex` check `member` on
  `team:{{ .Values.app.globalOrgAdminTeamName }}`. The team name defaults to
  `global_org_admin` and is consistent across environments.
- **Salesforce secret**: the chart references the pre-existing Kubernetes
  Secret named by `values.yaml` at `salesforce.secrets.name`
  (`lfx-v2-member-service-salesforce` by default). The chart does not create
  the ExternalSecret.
- **Service account**: `serviceaccount.yaml` renders optional annotations from
  `serviceAccount.annotations`; IRSA annotation values are supplied by
  deployment configuration, not hardcoded in this chart.
- **Env wiring**: standard service env plus Salesforce credential refs from
  `salesforce.secrets.keys`.
