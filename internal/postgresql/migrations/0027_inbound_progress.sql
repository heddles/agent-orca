-- 0027: what each channel message shows about its request (docs/chat-channels.md,
-- "Progress reactions"), so a later change knows what to take away. Empty for
-- messages recorded before this.
ALTER TABLE inbound_messages ADD COLUMN IF NOT EXISTS progress TEXT NOT NULL DEFAULT '';
