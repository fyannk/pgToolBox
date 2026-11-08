// Mutations over PgAccessRequest: pessimistic. Each call waits
// for the server's answer (the updated request); the list is then refetched,
// and it is also refetched on the stream's `clusters` invalidation (live.ts).
// apiFetch adds the CSRF header.

import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  approveAccessRequest,
  createAccessRequest,
  denyAccessRequest,
  getListAccessRequestsQueryKey,
  revokeAccessRequest,
} from "./generated/endpoints/access/access";
import type { AccessRequest, AccessRequestCreate } from "./generated/model";

export type AccessVerb = "approve" | "deny" | "revoke";
export type AccessDecisionInput = { verb: AccessVerb; note?: string };

const call = { approve: approveAccessRequest, deny: denyAccessRequest, revoke: revokeAccessRequest };

export function useAccessMutations() {
  const qc = useQueryClient();
  const settle = () => qc.invalidateQueries({ queryKey: getListAccessRequestsQueryKey() });
  const create = useMutation({
    mutationFn: ({ namespace, body }: { namespace: string; body: AccessRequestCreate }) =>
      createAccessRequest(namespace, body),
    onSuccess: settle,
  });
  const decide = useMutation({
    mutationFn: ({ request, decision }: { request: AccessRequest; decision: AccessDecisionInput }) =>
      call[decision.verb](request.namespace, request.name, decision.note ? { note: decision.note } : {}),
    onSuccess: settle,
  });
  return { create, decide };
}
