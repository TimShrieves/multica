"use client";

import { useQuery } from "@tanstack/react-query";
import { Bot } from "lucide-react";
import { useCurrentWorkspace } from "@multica/core/paths";
import { agentListOptions } from "@multica/core/workspace/queries";
import { runtimeListOptions } from "@multica/core/runtimes/queries";
import { hasProviderLogo, ProviderLogo } from "../../runtimes/components/provider-logo";

/**
 * The default face of an agent with no uploaded avatar: the mark of the
 * provider it runs on.
 *
 * Falls back to the generic bot glyph when the provider has no mark of its own
 * — `ProviderLogo` answers an unknown slug with a `Monitor` placeholder, which
 * identifies nothing, whereas a bot at least reads as "an agent".
 */
export function ProviderAvatarIcon({
  provider,
  className = "h-full w-full",
}: {
  provider: string | null | undefined;
  className?: string;
}) {
  if (!hasProviderLogo(provider)) {
    return <Bot className={className} />;
  }
  return <ProviderLogo provider={provider} className={className} />;
}

/**
 * {@link ProviderAvatarIcon} for a saved agent, resolving the provider through
 * the runtime the agent is bound to.
 *
 * Derived at render time rather than stamped onto the agent row at create time
 * (MUL-5399, which replaced a random emoji default): rebinding an agent to
 * another runtime has to move its face with it, and a persisted snapshot
 * wouldn't.
 *
 * Mount this only where it will actually be drawn. Passing it as
 * `ActorAvatar`'s `fallback` does exactly that: the base renders the node only
 * when there is no avatar image, so agents with real avatars never pay for the
 * runtime query.
 */
export function AgentRuntimeIcon({
  agentId,
  className = "h-full w-full",
}: {
  agentId: string;
  className?: string;
}) {
  const provider = useAgentRuntimeProvider(agentId);
  return <ProviderAvatarIcon provider={provider} className={className} />;
}

/**
 * Resolves an agent id to the provider slug of its bound runtime, or `""` while
 * either directory is still loading / the runtime is gone. Both queries are
 * workspace-wide and shared, so this is a cache read for every caller after the
 * first.
 *
 * Reads the workspace off the route rather than through `useWorkspaceId()`,
 * which throws when there is none. This is a leaf glyph mounted from ~30 avatar
 * sites; it should degrade to the generic bot rather than take a tree down —
 * the same reason `AgentStatusDot` reaches for `useCurrentWorkspace`.
 */
export function useAgentRuntimeProvider(agentId: string): string {
  const wsId = useCurrentWorkspace()?.id;
  const { data: agents = [] } = useQuery({
    ...agentListOptions(wsId ?? ""),
    enabled: !!wsId,
  });
  const { data: runtimes = [] } = useQuery({
    ...runtimeListOptions(wsId ?? ""),
    enabled: !!wsId,
  });

  const agent = agents.find((a) => a.id === agentId);
  if (!agent) return "";
  return runtimes.find((r) => r.id === agent.runtime_id)?.provider ?? "";
}
