-- Drop the generated emoji avatars (MUL-5399).
--
-- Agents created without an uploaded avatar used to be stamped with a random
-- `emoji:<glyph>` sentinel at CreateAgent time. That default is gone: an agent
-- with no avatar now renders the icon of the runtime it is bound to, derived at
-- render time so it stays correct when the agent is rebound.
--
-- The `emoji:` prefix is unambiguous and was only ever written by that server
-- default — the avatar upload control only ever produces an uploaded file URL,
-- and no UI or API path let a user pick an emoji. So every row matched here is
-- a machine-generated placeholder, never a choice anyone made, and clearing it
-- loses no user intent.
UPDATE agent
SET avatar_url = NULL
WHERE avatar_url LIKE 'emoji:%';
