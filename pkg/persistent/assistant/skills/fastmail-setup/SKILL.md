---
name: fastmail-setup
description: Walk the person through connecting their Fastmail account to you with a Fastmail API token of their own, so you can read their Fastmail mail, contacts and calendars. Use when the person asks you to reach their Fastmail email or calendar, or when your Fastmail tools answer that they are not authorised.
license: MIT
---

# Set up Fastmail

You reach a Fastmail account through Fastmail's own MCP server, run at
Fastmail, with an API token the person makes in their Fastmail settings. You
cannot do any of this yourself. Each step is the person's, in their browser
or in Agentry, and you explain it one step at a time, waiting for them to
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
setting, their plan may not include it.

## Make the token (in Fastmail)

In Fastmail, under *Settings → Privacy & Security*, in the API tokens section
(*Connected apps & API tokens*), they choose to make a new API token, of the
**MCP** type, give it a name (such as "My assistant") and choose its access:
**Read only** unless they want changes or sending. Fastmail shows the token
once: they keep it for the next step and nowhere else.

## Give the token to Agentry

Most people use Agentry Desktop: give these steps first. Give all of them
before they stop you, because you cannot answer while you are stopped; your
conversation carries on when they start you again.

### In Agentry Desktop

1. **Stop you**: the **Stop** button at the top of your page.
2. **Open the set-up**: **Configure**, then the **Mail and calendar**
   section. If there is no *Fastmail* card there, **Add a provider**, then
   **Add Fastmail**. On the card, the **⋯** menu, then **Set up**.
3. **The token**: they paste it into **Your Fastmail API token**, then **Keep
   the token**. Fastmail is asked whether it accepts the token before it is
   kept, in their system keychain; a token Fastmail refuses is not kept.
4. **Switch it on**: a click on the *Fastmail* card, which asks them to
   agree.
5. **Start you again**: **Start** at the top of your page.

### In a terminal

If they do not use Agentry Desktop, or its set-up fails, the same steps are
commands in a terminal on this computer:

    agentry agent stop assistant
    agentry agent configure assistant --fastmail-token

They paste the token when it asks (it is not shown); it is checked with
Fastmail and kept as in the desktop. Then:

    agentry agent grant assistant server:url-api-fastmail-com-mcp on
    agentry agent start assistant

## Changing what you can do

Their token's scopes are the only gate: to allow changes or sending later,
they make a new token with that access and give it to Agentry again (in
Agentry Desktop, the card's Set up, **Replace the token**), which replaces the
old one; they then delete the old token in Fastmail. To take Fastmail away,
**Remove the token** in the same panel (or `agentry agent configure assistant
--no-fastmail`) removes it from this computer, and deleting it in Fastmail
revokes it everywhere.

## When it does not work

- "the token was refused by Fastmail": it was mistyped, deleted in Fastmail,
  or not of the MCP type. They make a new one and give it to Agentry again.
- Your tools answer that they are not authorised: the token was deleted or
  revoked in Fastmail since; a new token, given to Agentry again.
- A change is refused: the token is Read only. That is the gate working; a
  new token with **Make changes** allows it, if they want that.
