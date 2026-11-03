import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useState, useSyncExternalStore } from "react";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage } from "../lib/api-client";

export type McpServerRow = components["schemas"]["MCPServerPayload"];
export type McpAccountStatus = components["schemas"]["MCPServerAccountStatus"];
export type McpTestResult = components["schemas"]["MCPServerTestResponse"];
export type McpOperation = components["schemas"]["MCPConnectOperation"];
export type McpPropagation = components["schemas"]["MCPPropagation"];
export type AddMcpServerInput = components["schemas"]["AddMCPServerRequest"];

/** What an action is about: a subscription, a session's own account, a project. */
export type McpTarget = { name: string; accountId?: string; sessionId?: string; projectId?: string };

const mcpServersQueryKey = ["mcp-servers"] as const;
const sessionMcpServersKey = (sessionId: string) => ["mcp-servers", "session", sessionId] as const;

function unwrap<T>({ data, error }: { data?: T; error?: unknown }): T {
	if (error) throw new Error(apiErrorMessage(error));
	return data as T;
}

/** The catalog. A check starts in the daemon if the last one is stale; while it
 * runs the list shows the last reading, and this polls until it lands. */
export function useMcpServers(enabled = true) {
	return useQuery({
		queryKey: mcpServersQueryKey,
		queryFn: async () => unwrap(await apiClient.GET("/api/v1/mcp/servers", { params: { query: {} } })),
		enabled,
		refetchInterval: (query) => (query.state.data?.checking ? 2_000 : false),
	});
}

/** What a session's running agent actually has right now. */
export function useSessionMcpServers(sessionId: string | undefined, enabled = true) {
	return useQuery({
		queryKey: sessionMcpServersKey(sessionId ?? ""),
		queryFn: async () => unwrap(await apiClient.GET("/api/v1/sessions/{sessionId}/mcp-servers", {
			params: { path: { sessionId: sessionId ?? "" }, query: {} },
		})),
		enabled: enabled && Boolean(sessionId),
		refetchInterval: 15_000,
	});
}

export function useMcpActions() {
	const queryClient = useQueryClient();
	const invalidate = useCallback(() => queryClient.invalidateQueries({ queryKey: mcpServersQueryKey }), [queryClient]);
	return {
		refresh: async () => {
			unwrap(await apiClient.GET("/api/v1/mcp/servers", { params: { query: { refresh: true } } }));
			await invalidate();
		},
		add: async (body: AddMcpServerInput) => {
			const out = unwrap(await apiClient.POST("/api/v1/mcp/servers", { body }));
			await invalidate();
			return out.applied;
		},
		remove: async (name: string) => {
			const out = unwrap(await apiClient.DELETE("/api/v1/mcp/servers/{serverName}", { params: { path: { serverName: name } } }));
			await invalidate();
			return out.applied;
		},
		setKey: async (name: string, headers: Record<string, string>) => {
			const out = unwrap(await apiClient.PATCH("/api/v1/mcp/servers/{serverName}", {
				params: { path: { serverName: name } }, body: { headers },
			}));
			await invalidate();
			return out.applied;
		},
		approve: async (target: McpTarget) => {
			const out = unwrap(await apiClient.POST("/api/v1/mcp/approve", { body: target }));
			await invalidate();
			return out.applied;
		},
		test: async (target: McpTarget) => {
			const out = unwrap(await apiClient.POST("/api/v1/mcp/test", { body: target }));
			await invalidate();
			return out;
		},
		reconnect: async (sessionId: string, name: string) => {
			const out = unwrap(await apiClient.POST("/api/v1/sessions/{sessionId}/mcp-servers/{serverName}/reconnect", {
				params: { path: { sessionId, serverName: name } },
			}));
			queryClient.setQueryData(sessionMcpServersKey(sessionId), out);
			return out;
		},
	};
}

export function mcpOperationFinished(op: McpOperation | undefined): boolean {
	return op !== undefined && op.state !== "waiting" && op.state !== "waiting_external";
}

// A sign-in is kept outside any component: leaving the page must not lose one
// waiting on the browser, and the chat notice and Settings see the same one.
type McpConnectEntry = { operation?: McpOperation; error?: string; starting?: boolean };
const connects = new Map<string, McpConnectEntry>();
const listeners = new Set<() => void>();
let version = 0;

function setConnect(key: string, entry: McpConnectEntry) {
	connects.set(key, entry);
	version += 1;
	for (const listener of listeners) listener();
}

function connectKey(target: McpTarget): string {
	return JSON.stringify([target.name, target.sessionId ?? "", target.accountId ?? "", target.projectId ?? ""]);
}

function subscribe(listener: () => void) {
	listeners.add(listener);
	return () => listeners.delete(listener);
}

/** A test's only way back to a clean slate. */
export function resetMcpConnectsForTest() {
	connects.clear();
	version += 1;
}

const MCP_CONNECT_POLL_MS = 1_500;

/** Connect one server: opens the browser through the daemon, then follows the
 * sign-in until it finishes and refreshes what depends on it. */
export function useMcpConnect(target: McpTarget) {
	const queryClient = useQueryClient();
	const key = connectKey(target);
	useSyncExternalStore(subscribe, () => version);
	const entry = connects.get(key) ?? {};
	const id = entry.operation?.id;
	const running = id !== undefined && !mcpOperationFinished(entry.operation);

	useEffect(() => {
		if (!running || !id) return;
		let stopped = false;
		const timer = window.setInterval(() => {
			void (async () => {
				try {
					const out = unwrap(await apiClient.GET("/api/v1/mcp/connect/{operationId}", { params: { path: { operationId: id } } }));
					if (stopped) return;
					setConnect(key, { operation: out.operation });
					if (mcpOperationFinished(out.operation)) {
						void queryClient.invalidateQueries({ queryKey: mcpServersQueryKey });
					}
				} catch (error) {
					if (!stopped) setConnect(key, { error: error instanceof Error ? error.message : String(error) });
				}
			})();
		}, MCP_CONNECT_POLL_MS);
		return () => {
			stopped = true;
			window.clearInterval(timer);
		};
	}, [id, key, queryClient, running]);

	const start = useCallback(async () => {
		setConnect(key, { starting: true });
		try {
			const out = unwrap(await apiClient.POST("/api/v1/mcp/connect", { body: target }));
			setConnect(key, { operation: out.operation });
		} catch (error) {
			setConnect(key, { error: error instanceof Error ? error.message : String(error) });
		}
		// eslint-disable-next-line react-hooks/exhaustive-deps -- the key names the target
	}, [key]);

	const cancel = useCallback(async () => {
		if (!id) return;
		try {
			const out = unwrap(await apiClient.POST("/api/v1/mcp/connect/{operationId}/cancel", { params: { path: { operationId: id } } }));
			setConnect(key, { operation: out.operation });
		} catch (error) {
			setConnect(key, { error: error instanceof Error ? error.message : String(error) });
		}
	}, [id, key]);

	const clear = useCallback(() => setConnect(key, {}), [key]);

	return { operation: entry.operation, error: entry.error, busy: Boolean(entry.starting) || running, start, cancel, clear };
}

/** Whichever test last ran on a row, with a busy flag. */
export function useMcpTest() {
	const actions = useMcpActions();
	const [result, setResult] = useState<McpTestResult>();
	const [error, setError] = useState<string>();
	const [busy, setBusy] = useState(false);
	const run = useCallback(async (target: McpTarget) => {
		setBusy(true);
		setError(undefined);
		setResult(undefined);
		try {
			setResult(await actions.test(target));
		} catch (reason) {
			setError(reason instanceof Error ? reason.message : String(reason));
		} finally {
			setBusy(false);
		}
		// eslint-disable-next-line react-hooks/exhaustive-deps -- actions is rebuilt each render
	}, []);
	return { result, error, busy, run };
}
