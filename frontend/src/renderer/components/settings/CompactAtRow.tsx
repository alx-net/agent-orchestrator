import { useTranslation } from "react-i18next";
import { useSettings, useUpdateCompactAt } from "../../hooks/useSettings";
import { SettingsOptionMenu, type SettingsOption } from "./SettingsOptionMenu";
import { SettingsRow } from "./SettingsRow";

/** The sizes offered, in tokens. The daemon's own default is 350K. */
const COMPACT_AT_SIZES = [200_000, 350_000, 500_000];
const DEFAULT_COMPACT_AT = 350_000;
/** Leaves compaction to the repository's and the user's Claude Code settings. */
const REPOSITORY_DEFAULT = 0;

function sizeLabel(tokens: number) {
	return `${Math.round(tokens / 1000)}K`;
}

/**
 * Where the Claude Code chats AO launches compact their conversation.
 * Daemon-owned: the daemon passes it to each agent it starts or resumes.
 */
export function CompactAtRow() {
	const { t } = useTranslation();
	const { settings, isLoading } = useSettings();
	const { update, saving, error } = useUpdateCompactAt();
	const current = settings?.compactAt;
	// A size set outside this menu (the API) still shows as the selection.
	const sizes = current && !COMPACT_AT_SIZES.includes(current)
		? [...COMPACT_AT_SIZES, current].sort((a, b) => a - b)
		: COMPACT_AT_SIZES;
	const options = [
		...sizes.map((tokens) => ({
			value: String(tokens),
			label: tokens === DEFAULT_COMPACT_AT ? t("settings.compactAt.default", { size: sizeLabel(tokens) }) : sizeLabel(tokens),
		})),
		{ value: String(REPOSITORY_DEFAULT), label: t("settings.compactAt.repositoryDefault") },
	] satisfies SettingsOption<string>[];

	return (
		<SettingsRow
			label={t("settings.compactAt.label")}
			description={error ? <span role="alert" className="text-error">{error}</span> : t("settings.compactAt.description")}
		>
			<SettingsOptionMenu
				aria-label={t("settings.compactAt.label")}
				value={String(current ?? DEFAULT_COMPACT_AT)}
				options={options}
				onChange={(value) => update(Number(value))}
				// A daemon that predates the setting cannot store it.
				disabled={isLoading || saving || current === undefined}
			/>
		</SettingsRow>
	);
}
