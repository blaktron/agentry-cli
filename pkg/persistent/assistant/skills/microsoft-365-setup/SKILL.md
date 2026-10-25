---
name: microsoft-365-setup
description: Walk the person through connecting their Microsoft account (work or school, or a personal Outlook.com, Hotmail or Live account) to you, with an Entra app of their own, so you can read their Outlook mail, calendar, contacts and OneDrive. Use when the person asks you to reach their Microsoft or Outlook email or calendar, when your Microsoft tools answer "No accounts found", or when they need their administrator to approve the app.
license: MIT
---

# Set up Microsoft 365 and Outlook.com

You reach a Microsoft account through the Microsoft 365 server. It signs in
with an app the person registers in Microsoft Entra, because until Agentry's
own app is available the sign-in needs one of theirs. You cannot do any of
this yourself. Each step is the person's, in their browser or in Agentry, and
you explain one step at a time, waiting for them to say it is done. Never
ask them to paste a code, a token, a password or a secret into the chat.
None of it is needed here, and a secret pasted into a chat is no longer
secret.

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

Ask which kind of account it is.

- **A work or school account** (an address their organization gives them):
  tell them that their organization's administrator will most likely have to
  approve the app before the sign-in works. Most organizations let nobody
  but an administrator consent to an app reading mail, and many also stop
  their people registering apps or signing in by a code. Offer to write the
  request for them to send to their administrator (see "Asking the
  administrator" below). They can register the app first and send the
  request with its id, or ask the administrator to register it for them.
- **A personal account** (Outlook.com, Hotmail, Live): Microsoft lets an app
  be registered only in a directory, so they first need a free Azure account
  (https://azure.microsoft.com/free/), made with that same personal account.
  That gives them a directory to register the app in. It costs nothing, and
  no administrator is involved.

## Register the app (in their browser)

1. **Register the app.** At https://entra.microsoft.com/, under *Identity →
   Applications → App registrations*, they choose **New registration** and
   give it any name (such as "My assistant"). If they are told they cannot
   register applications, their administrator has to register it for them:
   offer the request.
2. **Who may sign in.** Under *Supported account types*:
   - a personal account: **Personal Microsoft accounts only**;
   - a work or school account: **Accounts in this organizational directory
     only**, then note the *Directory (tenant) ID* on the app's *Overview*
     for later.
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
   certificate is needed: do not make one. For a work or school account, this
   is when the administrator grants consent (the **Grant admin consent**
   button on this page is theirs).

From the app's *Overview* they will need the **Application (client) ID**,
and for a work or school app the **Directory (tenant) ID**. Both are on the
page; they do not need to tell you either.

## Give the app to Agentry and sign in

Most people use Agentry Desktop: give these steps first. Give all of them
before they stop you, because you cannot answer while you are stopped; your
conversation carries on when they start you again.

### In Agentry Desktop

1. **Stop you**: the **Stop** button at the top of your page. The set-up is
   refused while you run.
2. **Switch Microsoft on**: **Configure**, then the **Mail and calendar**
   section. If there is no *Microsoft 365 and Outlook.com* card there,
   **Add a provider**, then **Add Microsoft**. A click on the card switches
   it on and asks them to agree; it then reads *Needs setting up*.
3. **Open the set-up**: on the card, the **⋯** menu, then **Set up**. (The
   menu offers Set up only once the card is on.)
4. **The app**: they paste the Application (client) ID into **Application
   (client) ID**. Under **Account kind** they choose *A personal account*,
   *A work or school account*, or, for an app limited to their organization,
   *One organization (its tenant id)* and paste the Directory (tenant) ID.
   Then **Keep the app**.
5. **Sign in**: their Microsoft address in **Your Microsoft account**, then
   **Sign in with Microsoft**. The panel shows a code and their browser opens
   Microsoft's page (https://www.microsoft.com/link). They enter the code
   there, sign in with that account and accept the permissions. Microsoft may
   say the app is unverified: it is their own app, which Microsoft has not
   reviewed. The panel then says which account is signed in.
6. **Start you again**: **Start** at the top of your page.

### In a terminal

If they do not use Agentry Desktop, or its set-up fails, the same steps are
commands in a terminal on this computer:

    agentry agent stop assistant
    agentry agent configure assistant --microsoft-client <the application id>

For an app limited to one organization they add `--microsoft-tenant <the
directory (tenant) id>` to the second command. Then:

    agentry agent configure assistant --microsoft-sign-in <their Microsoft address>

The terminal shows a code and opens Microsoft's page
(https://www.microsoft.com/link); they enter it there, sign in and accept the
permissions, and the terminal says which account is signed in. Then:

    agentry agent grant assistant server:ms365 on
    agentry agent start assistant

Either way, the sign-in asks only for reading unless *Allow changes* is on.

## Asking the administrator

When the person wants the request, ask them what you need to write it: their
organization's name if they want it named, the app's name, its Application
(client) ID if they have registered it, and whether they want changes or
sending. Never ask for a password, a secret or the sign-in code. Then write a
short message in their voice, for them to copy and send, that says:

- what they are asking for: admin consent for an app they registered, or that
  the administrator registers one for them, so their personal assistant on
  their own computer can read their Outlook mail, calendar, contacts and
  OneDrive;
- the app's name and Application (client) ID, when they have them;
- the delegated Microsoft Graph permissions from step 5 that they need, and
  that it acts only as them and sees only what they can already see;
- that it is read only unless they allow changes, that it has no client
  secret, and that its sign-in is kept encrypted on their own computer;
- what the administrator does: in the Microsoft Entra admin center, under
  *App registrations*, the app, *API permissions*, **Grant admin consent for
  (the organization)**; and if a Conditional Access policy blocks sign-in by
  device code, or app registration is turned off, an exception for this app
  or for them.

Keep it to what the administrator needs to decide, and let the person edit it
before they send it.

## Allowing changes later

In Agentry Desktop, the card's switches *Microsoft 365: allow changes* and
*Microsoft 365: allow sending mail* (sending only with changes); in a
terminal, `agentry agent grant assistant server:ms365:writes on` and
`server:ms365:send on`. Each switch asks for their agreement. A sign-in made
read only does not cover changes, so after switching changes on they stop you
and sign in again. A work or school account may also need the administrator
to consent to the wider permissions.

## When it does not work

- "Need admin approval", "Approval required" or *AADSTS65001*: their
  organization's administrator has to consent to the app. Offer the request.
- *AADSTS7000218* or "the app is not set up for public client flows": step 4.
- *AADSTS700016*, *AADSTS50020* or "the account does not exist in the
  directory": the app's supported account types (step 2) do not include the
  account they signed in with, or the tenant they gave Agentry is another
  organization's.
- "device code flow is blocked", or a Conditional Access message: their
  organization does not allow this sign-in. Only its administrator can allow
  it; offer the request.
- "read the server's accounts" after a sign-in that Microsoft accepted: an
  older Agentry. They update Agentry and sign in again.
- Your tools answer "No accounts found": the sign-in or the switch is not
  done.
