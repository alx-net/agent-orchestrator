import { createFileRoute } from "@tanstack/react-router";
import { AgentsOverview } from "../components/AgentsOverview";

export const Route = createFileRoute("/_shell/agents")({ component: AgentsOverview });
