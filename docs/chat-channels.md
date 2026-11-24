# Chat channels: Slack, Mattermost, Microsoft Teams, Google Chat, Webex

People ask a published agent a question in a chat app; the agent runs on the message and answers in the thread. This page is the setup guide for the five chat adapters. The shared behaviour (allow-lists, modes, ack-first, threading, reply formatting, security model) is the chat contract in [inbound-channels.md](inbound-channels.md#chat-contract).

All adapters answer as a trigger actor: the chat user is not an agent-orca user, the run has no invoker, and invoker-mode MCP servers fail closed. **Nothing was tried against a real Slack workspace, Mattermost server, Microsoft Teams tenant, Google Chat space or Webex organisation.** Every adapter is tested only against `httptest` fakes of the platform and its identity service, built from the vendors' developer documentation (JWTs are minted with locally generated RSA keys). Protocol details that the documentation did not settle are listed per platform under "Not verified against the real service" below; expect to fix some of them on first contact.

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

## Microsoft Teams

A bot registered with the Bot Framework; Teams calls the **messaging endpoint**. Personal chats, group chats and team channels.

1. Azure portal, create an **Azure Bot** (or Teams Developer Portal, Tools, Bot management). A single-tenant app registration is the safe choice; multi-tenant works too.
2. **Configuration**, Messaging endpoint = the receive URL (save the channel first so it exists). **Channels**: add Microsoft Teams.
3. Copy the **Microsoft App ID**. In the app registration, **Certificates & secrets**, create a client secret and copy its *value*. For a single-tenant app note the **Directory (tenant) ID**.
4. Secret keys (`tenantId` only for a single-tenant registration; leave it out for multi-tenant):

   ```bash
   kubectl -n team-a create secret generic teams-creds \
     --from-literal=appId=11111111-2222-3333-4444-555555555555 \
     --from-literal=appPassword=... --from-literal=tenantId=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee
   kubectl -n team-a label secret teams-creds agentorca.io/inbound-secret=true
   ```

5. Make a Teams app (Developer Portal, or a `manifest.json`) whose bot id is the App ID, with scopes `personal`, `team` and `groupChat`; upload it (Apps, Manage your apps, Upload a custom app) and add it to a team or chat. Only for the `channel` mode add the resource-specific consent permission `ChannelMessage.Read.Group` (the bot then receives every channel message of the team, not just mentions).
6. Settings: optional `typingIndicator`. Ids for the allow-lists: users are **Entra object ids** (GUIDs), groups are **tenant ids**, channels are **conversation ids** (`19:...@thread.tacv2` for a channel, `19:...@thread.v2` for a group chat, `a:...` for a personal chat).

How it behaves. *Inbound:* the bearer JWT must be signed (RS256) with a key of the Bot Framework key set (`https://login.botframework.com/v1/.well-known/keys`, cached, refetched on an unknown `kid` at most once a minute) that is **endorsed for `msteams`**, with issuer `https://api.botframework.com`, audience the app id, `exp` required and one minute of leeway; its `serviceurl` claim must equal the activity's `serviceUrl`, and that URL must be https, on a Microsoft host (`*.trafficmanager.net`, `*.botframework.com`, `*.microsoft.com`, `*.microsoft.us`, `*.skype.com`, `*.azure.com`) and not an IP. `channelId` must be `msteams`. Messages are keyed by tenant, conversation and activity id. A personal chat is `direct` and one flat conversation; `@bot` in a channel or group chat is `mention`; the bot's `<at>` tag is removed from the text and other mentions become `@name`; an unmentioned channel message is `channel` mode (refused unless enabled). Bots (`28:` ids, `role: bot`) are ignored. *Outbound:* the answer is posted to `{serviceUrl}/v3/conversations/{conversation}/activities/{activity}` with an OAuth2 client-credentials token for `https://api.botframework.com/.default` from `login.microsoftonline.com/{tenantId}` (single-tenant) or `botframework.com` (multi-tenant). The token is cached per connection until five minutes before it expires, refreshed on a 401, and never logged. The conversation reference (service URL, conversation id, activity id, bot id) is held only in the pending reply's `reply_target`. Markdown with `<at>`, `@all` and friends neutralised, at most 4 messages of at most 20000 characters and 27000 bytes each (Teams allows about 28 KB).

Not verified against the real service: that the real Bot Framework key set carries an `endorsements` member listing `msteams` (if every request is refused with "the key is not endorsed for Teams", that is the place to look); the exact claim name of the service URL (`serviceurl`, `serviceUrl` are both read); that a channel conversation id with `;messageid=` is accepted as is in the reply URL (it is path-escaped); the Microsoft service host list (a sovereign-cloud service URL outside it is refused); the typing activity. Not built: a token for the Bot Framework Emulator, user-assigned managed identity as the bot identity, proactive messages, adaptive cards, file attachments, mentions by the agent. Anyone who captures a Bot Framework token can replay it with another body until it expires (the token is not bound to the body); the sender allow-list and the message-id de-duplication are what limits that.

## Google Chat

A Chat app with an **HTTP endpoint** (the Chat app event format, not the Workspace add-on one). Direct messages, `@mention` in spaces, slash commands.

1. In a Google Cloud project enable the **Google Chat API**; note the **project number**.
2. Create a **service account** and a JSON key for it. No project role is needed.
3. Secret key `serviceAccountKey` = that JSON file:

   ```bash
   kubectl -n team-a create secret generic gchat-creds --from-file=serviceAccountKey=./key.json
   kubectl -n team-a label secret gchat-creds agentorca.io/inbound-secret=true
   ```

4. **Chat API, Configuration**: untick "Build this Chat app as a Workspace add-on" (the add-on event format is not understood), set name and description, tick "Receive 1:1 messages" and "Join spaces and group conversations". **Connection settings**: HTTP endpoint URL = the receive URL. **Authentication Audience**: *Project Number* or *HTTP endpoint URL*, and enter the same in the channel's settings: `audienceType` (`projectNumber` or `appUrl`) and `audience` (the project number, or the exact URL). Set the visibility, save. Optional slash command `/agent`.
5. Ids for the allow-lists: users `users/123...`, groups are the **email domain** of the sender (`example.com`), channels are space names `spaces/AAAA...`.

How it behaves. *Inbound:* with `projectNumber` the bearer token is a JWT signed by `chat@system.gserviceaccount.com` (keys from `https://www.googleapis.com/service_accounts/v1/jwk/chat@system.gserviceaccount.com`), issuer that account, audience the project number; with `appUrl` it is a Google ID token (keys from `https://www.googleapis.com/oauth2/v3/certs`, issuer `https://accounts.google.com`, audience the endpoint URL) whose verified `email` must be `chat@system.gserviceaccount.com`. RS256 only, `exp` required, one minute of leeway. `MESSAGE` events become messages keyed by the message's resource name; `ADDED_TO_SPACE` and everything else are answered with `{}` and ignored (the app does not greet). A DM is `direct` and one flat conversation; a space message is `mention` (Google Chat only delivers mentions and commands there), in the message's thread; a slash command is `command` and uses `argumentText`. The app's own mention is removed using the `USER_MENTION` annotations (indices read as code points, then as UTF-16 units; if neither lands on an `@` the text falls back to `argumentText`). Bot senders are ignored. *Outbound:* the app answers as itself: a JWT signed with the service account key (scope `https://www.googleapis.com/auth/chat.bot`, audience the pinned `https://oauth2.googleapis.com/token`; the key file's own `token_uri` is ignored) is exchanged for an access token (JWT bearer grant), cached per connection until five minutes before expiry and never logged. Messages are created with `POST https://chat.googleapis.com/v1/spaces/X/messages?messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD` and `thread.name`; a DM is answered flat. Chat markup (`*bold*`, `_italic_`, `~strike~`, `<url|label>`, bullets), `<users/all>`, `<users/ID>` and `@all` neutralised, at most 5 messages of at most 4000 characters.

Not verified against the real service: the exact claims of the two token kinds (both are written from the documentation), the unit of annotation indices, that `spaces.messages.create` with only the `chat.bot` scope may reply in an existing thread by name, and that a service account with no project role can be added to spaces. Not built: the Workspace add-on event format (and its token), the synchronous response (the app answers `{}` and posts later), `threadKey`, cards, dialogs, slash command ids, a working indicator, `channel` mode (Google Chat does not send a space message that does not mention the app), `spaces.list` in the test.

## Webex

A Webex **bot** and a webhook for new messages. Direct spaces, and group spaces where the bot is mentioned.

1. developer.webex.com, My Webex Apps, **Create a Bot**; copy the **access token** (shown once).
2. Choose a long random **webhook secret**.
3. Secret keys `botToken` and `webhookSecret`:

   ```bash
   kubectl -n team-a create secret generic webex-creds --from-literal=botToken=... --from-literal=webhookSecret=...
   kubectl -n team-a label secret webex-creds agentorca.io/inbound-secret=true
   ```

4. Create the webhook with the bot's token (the channel's setup steps show this with the URL filled in); set no filter:

   ```bash
   curl -X POST https://webexapis.com/v1/webhooks -H "Authorization: Bearer $BOT_TOKEN" -H "Content-Type: application/json" \
     -d '{"name":"agent-orca","targetUrl":"<receive URL>","resource":"messages","event":"created","secret":"'$WEBHOOK_SECRET'"}'
   ```

5. Add the bot to a space or message it. In a group space @mention it. Ids for the allow-lists: users are **email addresses**, groups are **email domains**, channels are Webex **room ids**.

How it behaves. *Inbound:* `X-Spark-Signature` must equal the hex HMAC-SHA1 of the raw body under the webhook secret (constant time). SHA-1 is what Webex signs with; `verify.VerifyHMACSHA1` exists for this one use. The signature does not cover a timestamp header, so the webhook's `data.created` must be within the last hour and not more than five minutes ahead, and messages are keyed by the Webex message id. The webhook holds ids only: after the acknowledgement (`Enrich`) the adapter reads `GET /v1/people/me` (once per connection and token) and `GET /v1/messages/{id}` with the bot token, checks that the message's id and room are those of the webhook, ignores the bot's own messages and other bots (addresses ending `@webex.bot`), and in a group space requires the bot's id in `mentionedPeople` (otherwise the message is refused as `mode_not_enabled`). The bot's name is removed from the start of the text. A message in a thread (`parentId`) joins that thread's conversation. A direct space is one flat conversation. If the message cannot be fetched after the client's retries it is recorded as ignored (`bot_message`): a core enricher cannot ask for a retry, and Webex has been answered already. *Outbound:* `POST /v1/messages` with `roomId`, `parentId` and `markdown` as the bot; Markdown with `<@all>`, `<@personEmail:..>`, `@all` and friends neutralised, at most 5 messages of at most 7439 bytes (cut by bytes, so non-Latin text is split earlier).

Not verified against the real service: the webhook payload field names and the `X-Spark-Signature` format (hex, no prefix), the `mentionedPeople` array in `GET /v1/messages/{id}`, how a mention is written into `text` (the adapter strips the bot's display name or nickname from the start), and that a reply to a reply's id with `parentId` threads correctly (the adapter always uses the thread's first message). Webex's developer pages could not be read when this was written; the protocol is from the task description and general knowledge of the API. Not built: attachments, Webex for Government (another API host), webhook creation from agent-orca, mentions in a direct space, `channel` mode (a bot never receives unmentioned group messages), a working indicator.

## The five adapters compared

| | Slack | Mattermost | Microsoft Teams | Google Chat | Webex |
|---|---|---|---|---|---|
| Request verification | HMAC-SHA256 `v0:ts:body`, 5 min window | shared token in the body (constant time) | RS256 JWT, Bot Framework keys endorsed for Teams, iss/aud/exp, serviceUrl claim = activity | RS256 JWT (project number) or Google ID token with the Chat account email (endpoint URL) | HMAC-SHA1 of the body, `data.created` within an hour |
| Modes | direct, mention, command, channel | mention, command, channel | direct, mention, channel (RSC) | direct, mention, command | direct, mention |
| Threading | thread of the message; DM flat | `root_id`, from a lookup | channel thread = conversation id; chats flat | `thread.name`; DM flat | `parentId`, from a lookup; direct flat |
| Reply dialect | mrkdwn | markdown | markdown | Chat markup (`mrkdwn-raw`) | markdown |
| Reply limit | 3500 chars x 5 | 4000 chars x 5 | 20000 chars / 27000 bytes x 4 | 4000 chars x 5 | 7439 bytes x 5 |
| Outbound auth | bot token | bot token | OAuth2 client credentials, cached | service-account JWT grant, cached | bot token |
| Secret keys | `botToken`, `signingSecret` | `baseUrl`, `botToken`, `tokens` | `appId`, `appPassword`, `tenantId` | `serviceAccountKey` | `botToken`, `webhookSecret` |
| Group for allow-list | workspace id | team id | tenant id | email domain | email domain |
| Working indicator | reaction | none | typing | none | none |
| Not built | Socket Mode | websocket, DMs | emulator, managed identity, cards, files | add-on format, sync reply, cards | attachments, Government host |

## Limits and what is not built

- Replicas share no replay memory; idempotency is by message id in the database.
- A reply that fails after posting some parts is retried whole, so a part can appear twice.
- Rejection notices are never sent: a refused message is recorded and dropped.
- Attachments, edits, reactions as input, interactive components, Socket Mode, the Mattermost websocket, and what each platform section above lists.
- Messages that fail to be recorded after the acknowledgement are lost (see the contract); for Webex this includes a message whose fetch failed.
- `requireLinkedUser` (mapping a chat user to an agent-orca user) is future work.
