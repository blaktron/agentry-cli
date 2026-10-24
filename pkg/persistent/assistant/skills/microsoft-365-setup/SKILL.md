---
name: microsoft-365-setup
description: Walk the person through connecting their Microsoft account (work or school, or a personal Outlook.com, Hotmail or Live account) to you, with an Entra app of their own, so you can read their Outlook mail, calendar, contacts and OneDrive. Use when the person asks you to reach their Microsoft or Outlook email or calendar, or when your Microsoft tools answer "No accounts found".
license: MIT
---

# Set up Microsoft 365 and Outlook.com

You reach a Microsoft account through the Microsoft 365 server, signed in
with an app the person registers in Microsoft Entra: until Agentry's own app
is available, the sign-in needs one of theirs. You cannot do any of this
yourself. Each step is the person's, in their browser or their terminal, and
you explain it one step at a time, waiting for them to say it is done. Never
ask them to paste a code, a token, a password or a secret into the chat: none
of it is needed here, and a secret pasted into a chat is no longer secret.

## What they get

Read only, unless they allow changes: mail (folders, messages, attachments),
calendar (events and their attendees), contacts and OneDrive (find and read
files). With **Allow changes** on: drafts, moving, filing and deleting mail,
making, changing and cancelling events and answering invitations, and
changing contacts; a calendar change can email the people invited. With
**Allow sending mail** on too: sending, replying and forwarding. Teams,
SharePoint, OneNote, To Do and any change to OneDrive are not served. Tell
them this before they start, so they can stop if it is not what they need.

## Before they start

- **A work or school account** whose organization manages it: the
  organization may not let its people register apps or sign in by device
  code. If step 1 is refused, or the sign-in later says device code is
  blocked, only their administrator can allow it.
- **A personal account** (Outlook.com, Hotmail, Live): Microsoft lets an app
  be registered only in a directory, so they first need a free Azure account
  (https://azure.microsoft.com/free/), made with that same personal account.
  That gives them a directory to register the app in. It costs nothing.

## The steps

1. **Register the app.** At https://entra.microsoft.com/, under *Identity →
   Applications → App registrations*, they choose **New registration** and
   give any name (such as "My assistant").
2. **Who may sign in.** Under *Supported account types*:
   - a personal account: **Personal Microsoft accounts only**;
   - a work or school account: **Accounts in this organizational directory
     only** (then note the *Directory (tenant) ID* on the app's *Overview*;
     step 6 needs it).
3. **The platform.** Under *Redirect URI* (or later *Authentication → Add a
   platform*) they choose **Mobile and desktop applications** and tick
   `https://login.microsoftonline.com/common/oauth2/nativeclient`, then
   **Register**.
4. **Allow public client flows.** Under *Authentication → Advanced settings*
   they set **Allow public client flows** to **Yes** and save. Without it the
   sign-in by code is refused.
5. **The permissions.** Under *API permissions → Add a permission → Microsoft
   Graph → Delegated permissions* they add `User.Read`, `Mail.Read`,
   `Calendars.Read`, `Contacts.Read`, `Files.Read` and `MailboxSettings.Read`;
   for changes also `Mail.ReadWrite`, `Calendars.ReadWrite` and
   `Contacts.ReadWrite`; for sending also `Mail.Send`. No client secret or
   certificate is needed: do not make one.
6. **Give the app to Agentry.** With you stopped (`agentry agent stop
   assistant`), from the app's *Overview* they copy the **Application
   (client) ID** and, in a terminal on this computer, run:

       agentry agent configure assistant --microsoft-client <the application id>

   For an app limited to one organization (step 2), they add
   `--microsoft-tenant <the directory (tenant) id>`.
7. **Sign in.** Then:

       agentry agent configure assistant --microsoft-sign-in <their Microsoft address>

   The terminal shows a code and opens https://microsoft.com/devicelogin;
   they enter the code there, sign in with that account, and accept the
   permissions. Microsoft may say the app is unverified: it is their own app,
   which Microsoft has not reviewed. The terminal says which account is
   signed in. This asks only for reading unless *Allow changes* is on.
8. **Switch it on**, and start you again:

       agentry agent grant assistant server:ms365 on
       agentry agent start assistant

In Agentry Desktop the same is in your Configuration, under **Mail and
calendar**.

## Allowing changes later

`agentry agent grant assistant server:ms365:writes on` allows changes, and
`server:ms365:send` sending as well (only with changes). A sign-in made read
only does not cover changes, so after switching changes on they stop you and
run step 7 again. Each switch asks for their agreement.

## When it does not work

- *AADSTS7000218* or "the app is not set up for public client flows": step 4.
- *AADSTS700016*, *AADSTS50020* or "the account does not exist in the
  directory": the app's supported account types (step 2) do not include the
  account they signed in with, or the tenant (step 6) is another
  organization's.
- "device code flow is blocked" or a Conditional Access message: their
  organization does not allow this sign-in; only its administrator can.
- *AADSTS65001* or a consent refused: a permission in step 5 needs an
  administrator's consent in their organization.
- Your tools answer "No accounts found": steps 7 and 8 are not done.
