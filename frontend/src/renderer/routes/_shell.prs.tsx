import { createFileRoute } from "@tanstack/react-router";
import { PullRequestsOverview } from "../components/PullRequestsOverview";

export const Route = createFileRoute("/_shell/prs")({ component: PullRequestsOverview });
