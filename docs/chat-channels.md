# Chat channels: Slack and Mattermost

People ask a published agent a question in a chat app; the agent runs on the message and answers in the thread. This page is the setup guide for the two chat adapters. The shared behaviour (allow-lists, modes, ack-first, threading, reply formatting, security model) is the chat contract in [inbound-channels.md](inbound-channels.md#chat-contract).

Both adapters answer as a trigger actor: the chat user is not an agent-orca user, the run has no invoker, and invoker-mode MCP servers fail closed. Nothing was tried against a real Slack workspace or a real Mattermost server; the adapters are tested against fakes of their APIs.

## Common steps

1. Publish the agent.
2. An administrator creates the Secret in the agent's namespace and labels it. The credentials never go through the API or the database:

   ```bash
   kubectl -n team-a create secret generic chat-creds --from-literal=botToken=... --from-literal=signingSecret=...
   kubectl -n team-a label secret chat-creds agentorca.io/inbound-secret=true
   ```

3. On the agent page, tab **Channels**, **Add a channel**, choose the platform, name the Secret, fill the settings and list who may write. The channel card then shows the receive URL and the platform's setup steps with that URL filled in. **Test connection** shows the bot and the workspace.
4. The receive URL is `https://<your agent-orca host>/inbound/<kind>/<connection id>`. The platform must be able to reach it; the UI proxy forwards `/inbound/*` without a session.

**Who may write.** Nobody by default. List user ids (`allowSenders`), workspace ids (`allowGroups`) and/or channel ids (`allowChannels`); see the contract for how they combine. By default the agent answers direct messages, mentions and slash commands. Answering every message of a channel (`modes` containing `channel`) needs the channels on the list.

## Slack

Events API over HTTPS. Socket Mode is not built.

1. api.slack.com/apps, Create New App, From scratch, pick the workspace.
2. **OAuth & Permissions**, Bot Token Scopes: `app_mentions:read`, `chat:write`, `im:history` (direct messages), `commands` (slash command); `channels:history` and `groups:history` only if you enable the `channel` mode; `reactions:write` for the optional eyes reaction. Install to the workspace and copy the **Bot User OAuth Token** (`xoxb-...`; a user token is refused).
3. **Basic Information**: copy the **Signing Secret**.
4. Secret keys: `botToken`, `signingSecret` (as above).
5. In agent-orca add the channel. Settings: `teamId` (the workspace id, `T...`; the Test button shows it; events from other workspaces are ignored) and optionally `workingReaction`. Ids: users `U...` (profile, Copy member ID), channels `C...` (channel details), direct messages `D...`.
6. **Event Subscriptions**: on, Request URL = the receive URL (Slack sends a challenge, which agent-orca answers after verifying the signature, so save the channel first), subscribe the bot to `app_mention`, `message.im` and, for the `channel` mode, `message.channels` (and `message.groups`).
7. **App Home**: tick "Allow users to send Slash commands and messages from the messages tab".
8. Optional **Slash Commands**: create `/agent` with the same Request URL. The bot must be a member of the channel where the command is used, because the answer is posted with `chat.postMessage`; the command itself answers the user at once with a private "Working on it".
9. Invite the bot to a channel (`/invite @name`).

How it behaves: a request is accepted only with a valid `X-Slack-Signature` (`v0` HMAC of `v0:<timestamp>:<body>`, five minute window, constant time). A message is identified by workspace, channel and message timestamp, so Slack's retries (`X-Slack-Retry-Num`) and the double delivery of a mentioning message (`app_mention` plus `message.channels`) start one run. Messages with a `bot_id` or any `subtype` (edits, joins, other bots), and the bot's own, are ignored. The text has its leading `@bot` removed and Slack's markup decoded. The answer is posted in the thread of the message (a direct message is one flat conversation), as `mrkdwn`, with `link_names` and unfurling off and every mention neutralised, split into at most five messages of at most 3500 characters.

## Mattermost

Outgoing webhooks and slash commands. The bot-account websocket (and with it direct messages) is not built.

1. **Integrations, Bot Accounts**: create a bot, copy its access token. Add the bot to the team and to each channel it should answer in.
2. **Integrations, Outgoing Webhooks**: add one for a channel with the callback URL = the receive URL and a trigger word (for example `@agent`, triggered at the start of the message), or no trigger word to receive every post of the channel (the `channel` mode). Copy its token. And/or **Slash Commands**: request URL = the receive URL, method POST, copy its token.
3. Secret keys:

   ```bash
   kubectl -n team-a create secret generic mm-creds \
     --from-literal=baseUrl=https://chat.example.com \
     --from-literal=botToken=... \
     --from-literal=tokens=$'webhook-token\ncommand-token' \
     --from-literal=allowPrivateAddresses=true   # only for a server on a private address
   kubectl -n team-a label secret mm-creds agentorca.io/inbound-secret=true
   ```

   The server address is in the Secret, not in the connection, so a builder cannot point the bot's token at another host. `allowPrivateAddresses=true` lifts the SSRF guard for this connection's calls (a self-hosted server is usually private); `insecure=true` permits an `http` address.
4. In agent-orca add the channel. The optional setting `teamId` restricts it to one team. Ids are Mattermost's 26-character ids.
5. If Mattermost is on a private network and agent-orca too, set the server's `AllowedUntrustedInternalConnections` to include agent-orca's host.

How it behaves: the token in the request (form or JSON) is compared in constant time with the tokens in the Secret. A webhook does not say which thread a post is in or who wrote it, so before the message enters the pipeline (in the background step, after the acknowledgement) the adapter reads the post and the bot account: a post that has a `root_id` joins that thread's conversation, and posts by webhooks or bots, system posts, and the bot's own are ignored. If that lookup fails the message simply starts its own conversation. The answer is posted with `POST /api/v4/posts` as the bot with `root_id` (so it lands in the thread), in markdown, with `@all`, `@channel`, `@here` and user mentions neutralised, split into at most five posts of at most 4000 characters.

## Limits and what is not built

- Replicas share no replay memory; idempotency is by message id in the database.
- A reply that fails after posting some parts is retried whole, so a part can appear twice.
- Rejection notices are never sent: a refused message is recorded and dropped.
- Attachments, edits, reactions as input, interactive components, Socket Mode, the Mattermost websocket.
- `requireLinkedUser` (mapping a chat user to an agent-orca user) is future work.
