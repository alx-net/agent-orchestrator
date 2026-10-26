import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { apiErrorMessage } from "../lib/api-client";
import { clientForSessionHost } from "../lib/host-clients";
import { prCanMerge } from "../lib/pr-display";
import { usesPreviewWorkspaceData as usePreviewData } from "../lib/preview-mode";
import { useCloudCp } from "./useCloudCp";
import { sessionScmSummaryQueryKey, type SessionPRReference, type SessionPRSummary } from "./useSessionScmSummary";
import { workspaceQueryKeyForHost } from "./useWorkspaceQuery";

export interface PullRequestMerge {
	/** The PR is open, green, approved enough and pinned to a head commit. */
	canMerge: boolean;
	merge: () => void;
	pending: boolean;
	error: string | null;
}

/**
 * Merging one session PR, from the inspector's PR card or the chat's merge box:
 * the daemon (or AO Cloud) merges at the head commit the person saw, then the
 * session's PR summary and the board catch up.
 */
export function usePullRequestMerge({
	cloudOrgId,
	hostId,
	pr,
	sessionId,
}: {
	cloudOrgId?: string;
	hostId?: string;
	pr: SessionPRSummary;
	sessionId: string;
}): PullRequestMerge {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const workspaceKey = workspaceQueryKeyForHost(hostId);
	const { client: cloudClient, baseUrl: cloudBaseUrl } = useCloudCp();
	const mergePr = useMutation({
		mutationFn: async () => {
			if (usePreviewData) return;
			if (cloudOrgId) {
				await cloudClient.mergePullRequest(cloudOrgId, sessionId, pr.number, pr.url, pr.headSha);
				return;
			}
			const { data, error } = await clientForSessionHost(hostId).POST("/api/v1/prs/{id}/merge", {
				params: { path: { id: String(pr.number) } },
				body: { prUrl: pr.url, expectedHeadSha: pr.headSha },
			});
			if (error) throw new Error(apiErrorMessage(error, t("pr.merge.failed", { number: pr.number })));
			return data;
		},
		onSuccess: async (result) => {
			if (!cloudOrgId && result?.state === "merged") {
				queryClient.setQueryData<{ prs: SessionPRSummary[]; linkedPrs: SessionPRReference[] }>(
					sessionScmSummaryQueryKey(sessionId, hostId),
					(current) => current ? {
						...current,
						prs: current.prs.map((cached) => cached.url === pr.url ? {
							...cached,
							state: result.state,
							stateChangedAt: result.stateChangedAt,
							updatedAt: result.stateChangedAt,
						} : cached),
					} : current,
				);
				await queryClient.invalidateQueries({ queryKey: workspaceKey });
				return;
			}
			await Promise.all([
				queryClient.invalidateQueries({ queryKey: cloudOrgId ? ["cloud-session-scm-summary", cloudBaseUrl, cloudOrgId, sessionId] : sessionScmSummaryQueryKey(sessionId, hostId) }),
				queryClient.invalidateQueries({ queryKey: workspaceKey }),
			]);
		},
	});
	return {
		canMerge: prCanMerge(pr) && Boolean(pr.url && pr.headSha),
		merge: () => mergePr.mutate(),
		pending: mergePr.isPending,
		error: mergePr.error instanceof Error ? mergePr.error.message : null,
	};
}
