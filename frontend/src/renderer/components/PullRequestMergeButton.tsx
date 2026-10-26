import { GitMerge, Loader2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { PullRequestMerge } from "../hooks/usePullRequestMerge";
import { Button } from "./ui/button";

/** The one Merge action for a session PR, shown only while it can merge. */
export function PullRequestMergeButton({ merge, number }: { merge: PullRequestMerge; number: number }) {
	const { t } = useTranslation();
	if (!merge.canMerge) return null;
	return (
		<Button
			aria-label={t("pr.merge.actionFor", { number })}
			className="gap-1 bg-success px-2 text-xs text-on-foreground hover:bg-success/80"
			disabled={merge.pending}
			onClick={merge.merge}
			size="sm"
			type="button"
		>
			{merge.pending ? (
				<Loader2 className="size-icon-sm animate-spin" aria-hidden="true" />
			) : (
				<GitMerge className="size-icon-sm" aria-hidden="true" />
			)}
			{merge.pending ? t("pr.merge.merging") : t("pr.merge.action")}
		</Button>
	);
}
