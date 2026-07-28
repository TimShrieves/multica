// @vitest-environment jsdom

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, cleanup } from "@testing-library/react";
import { ActorAvatar } from "@multica/ui/components/common/actor-avatar";

vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({ id: "ws-1" }),
}));

const mockAgents = vi.hoisted(() => ({ current: [] as unknown[] }));
const mockRuntimes = vi.hoisted(() => ({ current: [] as unknown[] }));

// Two list queries feed the icon, told apart by query-key shape:
//   ["workspaces", wsId, "agents"] — agent list (agent → runtime_id)
//   ["runtimes",   wsId, "list"]   — runtime list (runtime → provider)
vi.mock("@tanstack/react-query", async () => {
  const actual = await vi.importActual<typeof import("@tanstack/react-query")>(
    "@tanstack/react-query",
  );
  return {
    ...actual,
    useQuery: (opts: { queryKey: readonly unknown[] }) => {
      const key = opts.queryKey;
      if (key[0] === "workspaces" && key[2] === "agents") {
        return { data: mockAgents.current, isLoading: false };
      }
      if (key[0] === "runtimes") {
        return { data: mockRuntimes.current, isLoading: false };
      }
      return { data: undefined, isLoading: false };
    },
  };
});

import { AgentRuntimeIcon } from "./agent-runtime-icon";

function makeAgent(overrides: Record<string, unknown> = {}) {
  return { id: "agent-1", runtime_id: "rt-1", ...overrides };
}

function makeRuntime(overrides: Record<string, unknown> = {}) {
  return { id: "rt-1", provider: "claude", ...overrides };
}

beforeEach(() => {
  cleanup();
  mockAgents.current = [makeAgent()];
  mockRuntimes.current = [makeRuntime()];
});

describe("AgentRuntimeIcon", () => {
  it("draws the mark of the provider the agent's runtime runs", () => {
    const { container } = render(<AgentRuntimeIcon agentId="agent-1" />);

    // The Claude mark is an inline svg with the brand fill; lucide's generic
    // Bot glyph is stroke-based and carries a lucide-* class instead.
    const svg = container.querySelector("svg");
    expect(svg?.getAttribute("fill")).toBe("#D97757");
    expect(svg?.getAttribute("class")).not.toContain("lucide");
  });

  it("follows the agent to a new runtime instead of a create-time snapshot", () => {
    mockAgents.current = [makeAgent({ runtime_id: "rt-2" })];
    mockRuntimes.current = [makeRuntime(), makeRuntime({ id: "rt-2", provider: "codex" })];

    const { container } = render(<AgentRuntimeIcon agentId="agent-1" />);

    // Codex's mark is currentColor-filled, so it can be told apart from
    // Claude's brand fill without depending on path data.
    expect(container.querySelector("svg")?.getAttribute("fill")).toBe(
      "currentColor",
    );
  });

  it("falls back to the bot glyph when the runtime is gone", () => {
    mockRuntimes.current = [];

    const { container } = render(<AgentRuntimeIcon agentId="agent-1" />);

    expect(container.querySelector("svg")?.getAttribute("class")).toContain(
      "lucide-bot",
    );
  });

  it("falls back to the bot glyph for a provider with no mark of its own", () => {
    mockRuntimes.current = [makeRuntime({ provider: "some-custom-backend" })];

    const { container } = render(<AgentRuntimeIcon agentId="agent-1" />);

    // NOT ProviderLogo's generic Monitor placeholder — that identifies
    // nothing, while a bot at least reads as "an agent".
    expect(container.querySelector("svg")?.getAttribute("class")).toContain(
      "lucide-bot",
    );
  });
});

describe("ActorAvatar fallback wiring", () => {
  it("prefers an uploaded avatar over the runtime icon", () => {
    const { container } = render(
      <ActorAvatar
        name="Lambda"
        initials="LA"
        avatarUrl="https://cdn.example.com/lambda.png"
        isAgent
        fallback={<AgentRuntimeIcon agentId="agent-1" />}
      />,
    );

    expect(container.querySelector("img")?.getAttribute("src")).toBe(
      "https://cdn.example.com/lambda.png",
    );
    expect(container.querySelector("svg")).toBeNull();
  });

  it("draws the runtime icon in place of the generic bot when there is no avatar", () => {
    const { container } = render(
      <ActorAvatar
        name="Lambda"
        initials="LA"
        avatarUrl={null}
        isAgent
        fallback={<AgentRuntimeIcon agentId="agent-1" />}
      />,
    );

    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("svg")?.getAttribute("fill")).toBe("#D97757");
  });

  it("keeps the generic bot for an agent whose caller passes no fallback", () => {
    const { container } = render(
      <ActorAvatar name="Lambda" initials="LA" avatarUrl={null} isAgent />,
    );

    expect(container.querySelector("svg")?.getAttribute("class")).toContain(
      "lucide-bot",
    );
  });
});
