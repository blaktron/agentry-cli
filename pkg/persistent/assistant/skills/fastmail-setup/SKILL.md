---
name: fastmail-setup
description: Walk the person through connecting their Fastmail account to you with a Fastmail API token of their own, so you can read their Fastmail mail, contacts and calendars. Use when the person asks you to reach their Fastmail email or calendar, or when your Fastmail tools answer that they are not authorised.
license: MIT
---

# Set up Fastmail

You reach a Fastmail account through Fastmail's own MCP server, run at
Fastmail, with an API token the person makes in their Fastmail settings. You
cannot do any of this yourself. Each step is the person's, in their browser
or their terminal, and you explain it one step at a time, waiting for them to
say it is done. Never ask them to paste the token into the chat: it is not
needed here, and a token pasted into a chat is no longer secret.

## What they get

What the token allows, and nothing more: the server can do in their account
whatever its scopes permit. With **Read** alone: their mail, contacts and
calendars, read only. With **Make changes** too: filing, flagging and
deleting mail, drafts, and changes to contacts and events. With **Send**:
sending mail as them. Attachments are not served. Advise Read only unless
they want you to change or send things; they can make a new token later.

## Before they start

API tokens are not offered on Fastmail's Basic plan. If they cannot find the
setting in step 1, their plan may not include it.

## The steps

1. **Make the token.** In Fastmail, under *Settings → Privacy & Security*, in
   the API tokens section (*Connected apps & API tokens*), they choose to make
   a new API token, of the **MCP** type, give it a name (such as "My
   assistant") and choose its access: **Read only** unless they want changes
   or sending. Fastmail shows the token once: they keep it for step 2 and
   nowhere else.
2. **Give the token to Agentry.** With you stopped (`agentry agent stop
   assistant`), in a terminal on this computer, they run:

       agentry agent configure assistant --fastmail-token

   and paste the token when it asks (it is not shown). Fastmail is asked
   whether it accepts the token before it is kept, in their system keychain;
   a token Fastmail refuses is not kept.
3. **Switch it on**, and start you again:

       agentry agent grant assistant server:url-api-fastmail-com-mcp on
       agentry agent start assistant

In Agentry Desktop the same is in your Configuration, under **Mail and
calendar**.

## Changing what you can do

Their token's scopes are the only gate: to allow changes or sending later,
they make a new token with that access and run step 2 again, which replaces
the old one; they then delete the old token in Fastmail. To take Fastmail
away, `agentry agent configure assistant --no-fastmail` removes the token
from this computer, and deleting it in Fastmail revokes it everywhere.

## When it does not work

- "the token was refused by Fastmail": it was mistyped, deleted in Fastmail, or not
  of the MCP type. They make a new one (step 1) and run step 2 again.
- Your tools answer that they are not authorised: the token was deleted or
  revoked in Fastmail since; step 1 and 2 again.
- A change is refused: the token is Read only. That is the gate working; a
  new token with **Make changes** allows it, if they want that.
