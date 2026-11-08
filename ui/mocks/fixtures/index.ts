// Readable names for the API fixtures (scenarios.gen.ts) the stories, tests
// and MSW handlers use.
import * as api from "./scenarios.gen";

export const clusterList = api.listClusters;
export const clusterListDenied = api.listClustersDenied;
export const clusterListPartial = api.listClustersPartial;
export const failingOverOverview = api.getClusterBilling;
export const healthyOverview = api.getClusterOrders;
export const clusterForbidden = api.getClusterForbidden;
export const clusterNotFound = api.getClusterNotFound;
export const clusterUnchecked = api.getClusterUnchecked;
export const instances = api.listInstancesBilling;
export const events = api.listEventsBilling;
export const diagnostics = api.getDiagnosticsBilling;
export const triage = api.getTriageBilling;
export const session = api.getSession;
export const vocabulary = api.getVocabulary;
export const admin = api.getAdmin;
export const backups = api.getBackupsOrders;
/** billing backs up with pgBackRest (Dalibo): a full → differential → incremental chain, a failed incremental, the agent's window and one finding. */
export const backupsPgBackRest = api.getBackupsBilling;
export const restoreCheck = api.getRestoreCheckOrders;
export const replication = api.getReplicationOrders;
export const storage = api.getStorageOrders;
/** A fork build that reports its volumes: forecasts, autoGrow, WAL piling up while archiving fails. */
export const storageForecasts = api.getStorageBilling;
export const objects = api.getObjectsOrders;
export const metrics = api.getMetricsOrders;
export const topology = api.getTopologyOrders;
export const history = api.listHistoryOrders;
export const revision = api.getRevisionOrders;
export const revisionNotFound = api.getRevisionNotFound;
export const logTail = api.getLogTailOrders;
export const comparison = api.compareClusters;
export const comparisonForbidden = api.compareClustersForbidden;
export const comparisonNotFound = api.compareClustersNotFound;
export const logTailForbidden = api.getLogTailForbidden;
export const operationsOperator = api.listOperationsOrders;
export const operationsViewer = api.listOperationsBilling;
export const operationRequested = api.requestOperationBackup;
export const operationRefused = api.requestOperationRefused;
export const operationBadConfirmation = api.requestOperationBadConfirmation;
export const operationRunning = api.getOperation;
export const accessRequests = api.listAccessRequests;
export const accessRequestsReadOnly = api.listAccessRequestsReadOnly;
export const accessRequestsRequester = api.listAccessRequestsRequester;
export const accessApproved = api.approveAccessRequest;
export const accessDenied = api.denyAccessRequest;
export const accessRevoked = api.revokeAccessRequest;
export const accessCreated = api.createAccessRequest;
export const accessCreateInvalid = api.createAccessRequestInvalid;
export const accessApproveSelf = api.approveAccessRequestSelf;
export const accessDenyNoNote = api.denyAccessRequestNoNote;
/** One request of the access list golden, by name (the goldens cover every phase and the four-eyes rule). */
function accessItem(name: string) {
  const r = (api.listAccessRequests.items ?? []).find((i) => i.name === name);
  if (!r) throw new Error(`no access request ${name} in listAccessRequests.json`);
  return r;
}
/** Pending, names the reader: approve refused by the four-eyes rule, withdraw (self-deny) allowed. */
export const accessMinePending = accessItem("operator-m4n7t");
/** Pending, someone else, a Cluster named: the reader may approve or deny. */
export const accessToDecide = accessItem("operator-k2x9p");
/** Approved but its RoleBinding is gone: reported, never recreated. */
export const accessBindingMissing = accessItem("viewer-p0o9i");
/** Approved for a group the reader is in: revocable. */
export const accessGroupGranted = accessItem("viewer-q8w3d");
/** Denied, with the decider's note. */
export const accessDeniedWithNote = accessItem("operator-w3e4r");
export const accessExpired = accessItem("operator-a9s8d");
export const databaseRoleCreated = api.createDatabaseRole;
export const databaseRoleUnavailable = api.createDatabaseRoleUnavailable;
export const databaseRoleExists = api.createDatabaseRoleExists;
/** The pack enabled: wraparound overdue, a lock chain, a regression, unused/invalid/duplicate indexes. */
export const insightEnabled = api.getInsightOrders;
/** The pack not enabled: every panel unavailable, the one-line spec change to opt in. */
export const insightNotEnabled = api.getInsightBilling;
export const insightBlocking = api.getInsightBlockingOrders;
export const fleetStatements = api.getFleetStatements;
export const fleetStatementsDenied = api.getFleetStatementsDenied;
export const timeline = api.getTimelineBilling;
export const timelineFiltered = api.getTimelineBillingFiltered;
export const timelineBadWindow = api.getTimelineBadWindow;
export const digests = api.listDigests;
export const digestsDenied = api.listDigestsDenied;
export const digest = api.getDigest;
export const digestNotFound = api.getDigestNotFound;
/** Three connected assistants: one consented with the write tools on, one never used yet. */
export const grants = api.listGrants;
export const grantsEmpty = api.listGrantsEmpty;
/** MCP is off on this console: the assistant endpoints answer 404 with the reason. */
export const grantsNotServed = api.listGrantsNotServed;
export const grantNotFound = api.revokeGrantNotFound;
export const grantsRevokeAllNotServed = api.revokeAllGrantsNotServed;
export const assistantSetupNotServed = api.getAssistantSetupNotServed;
/** The session of a console without MCP: features.mcp false, so no link to the assistants page. */
export const sessionMcpOff = api.getSessionMcpOff;
/** "Connect an assistant" as internal/console/assistant renders it: read-only, OAuth only. */
export const assistantSetup = api.assistantSetupOauthOnly;
/** The write tools on: read and operate. */
export const assistantSetupOperate = api.assistantSetupOperate;
/** Token sign-in on: each client also gets its static-token (break-glass) form. */
export const assistantSetupTokenLogin = api.assistantSetupTokenLogin;
export const assistantSkill = api.assistantSkillRead;
export const assistantAgents = api.assistantAgentsRead;
