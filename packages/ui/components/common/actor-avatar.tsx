"use client";

import { useState, useEffect, type ReactNode } from "react";
import { Bot, Users } from "lucide-react";
import { cn } from "@multica/ui/lib/utils";
import {
  AVATAR_SIZE_PX,
  DEFAULT_AVATAR_SIZE,
  type AvatarSize,
} from "@multica/ui/lib/avatar-size";
import { MulticaIcon } from "./multica-icon";

interface ActorAvatarProps {
  name: string;
  initials: string;
  avatarUrl?: string | null;
  isAgent?: boolean;
  isSystem?: boolean;
  isSquad?: boolean;
  size?: AvatarSize;
  className?: string;
  /**
   * Glyph to draw when the actor has no avatar image, in place of the built-in
   * type icon. `packages/ui` can't reach the workspace directories, so the
   * caller resolves the identity-specific mark and hands it in — the agent
   * surfaces pass the icon of the runtime the agent is bound to
   * (`AgentRuntimeIcon` in packages/views). Rendered inside a square box scaled
   * to the avatar, so pass something that fills its container (`h-full w-full`).
   */
  fallback?: ReactNode;
}

function ActorAvatar({
  name,
  initials,
  avatarUrl,
  isAgent,
  isSystem,
  isSquad,
  size = DEFAULT_AVATAR_SIZE,
  className,
  fallback,
}: ActorAvatarProps) {
  const [imgError, setImgError] = useState(false);
  const px = AVATAR_SIZE_PX[size];

  useEffect(() => {
    setImgError(false);
  }, [avatarUrl]);

  // Every actor — member, agent, squad, or system — renders as a circle. This
  // is the single source of truth for avatar shape; the upload editors mirror
  // it (packages/views/common/avatar-upload-control.tsx).
  return (
    <div
      data-slot="avatar"
      className={cn(
        "inline-flex shrink-0 items-center justify-center font-medium overflow-hidden",
        (!avatarUrl || imgError) && "bg-muted text-muted-foreground",
        className,
        // rounded-full stays last so a call-site `className` can never override
        // the circle — avatar shape is a hard invariant, not a per-site choice.
        "rounded-full"
      )}
      style={{ width: px, height: px, fontSize: px * 0.45 }}
    >
      {avatarUrl && !imgError ? (
        <img
          src={avatarUrl}
          alt={name}
          className="h-full w-full object-cover"
          onError={() => setImgError(true)}
        />
      ) : fallback ? (
        <span
          className="inline-flex items-center justify-center"
          style={{ width: px * 0.55, height: px * 0.55 }}
        >
          {fallback}
        </span>
      ) : isSystem ? (
        <MulticaIcon noSpin style={{ width: px * 0.55, height: px * 0.55 }} />
      ) : isAgent ? (
        <Bot style={{ width: px * 0.55, height: px * 0.55 }} />
      ) : isSquad ? (
        <Users style={{ width: px * 0.55, height: px * 0.55 }} />
      ) : (
        initials
      )}
    </div>
  );
}

export { ActorAvatar, type ActorAvatarProps };
