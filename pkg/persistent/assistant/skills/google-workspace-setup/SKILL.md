---
name: google-workspace-setup
description: Walk the person through connecting their Google account to you, with a Google Cloud OAuth client of their own, so you can read their Gmail, Calendar, Drive, Docs and Sheets. Use when the person asks you to reach their Google account, email or calendar, or when your Google Workspace tools answer that nobody is signed in.
license: MIT
---

# Set up Google Workspace

You reach Google Workspace through gog's server, signed in with the
person's own Google Cloud OAuth client: Google gives an app no way to sign
in without one. You cannot do any of this yourself. Each step is the
person's, in their browser or in Agentry, and you explain it one step at
a time, waiting for them to say it is done. Never ask them to paste the
client file, a code, a token or a password into the chat: none of it is
needed here, and a secret pasted into a chat is no longer secret.

## What they get

Read only, unless they allow changes: Gmail (search, read messages, threads,
drafts and labels), Drive (search, file details), Docs (read), Sheets (read a
range) and Calendar (list events). With **Allow changes** on: write Docs and
Sheets, make and update Gmail drafts, label, archive, mark read or unread,
trash and restore mail. With **Allow sending mail** on too: send mail and
drafts. Contacts, Tasks, Slides, Forms and changes to the calendar are not
served. Tell them this before they start, so they can stop if it is not what
they need.

## Make the client (in Google Cloud)

1. **A Google Cloud project.** At https://console.cloud.google.com/, signed in
   with the Google account they want you to reach, they make a new project
   (any name, such as "My assistant").
2. **The APIs.** In *APIs & Services → Library* they enable each API they
   want: Gmail API, Google Calendar API, Google Drive API, Google Docs API,
   Google Sheets API. Fewer is fine; you will lack what they leave out.
3. **The consent screen.** In *Google Auth Platform* (*OAuth consent screen*
   on older consoles) they choose **Get started**, give an app name and their
   email, choose the audience **External**, and finish. Under *Audience →
   Test users* they add their own Google address.
4. **The client.** Under *Clients* they choose **Create client**, the type
   **Desktop app**, any name, then **Create**, and **Download JSON**. The file
   stays on their computer. A *Web application* client will not work.
5. **Publish.** Under *Audience* they choose **Publish app**. Why: while the
   app is in Testing, Google ends its sign-ins after 7 days, and they would
   sign in again every week. Published but not verified by Google, the app
   shows "Google hasn't verified this app" at sign-in. That warning is about
   their own app, which only they use: they choose *Advanced*, then *Go to
   (their app's name)*. Google limits an unverified app to 100 users, which a
   person's own app never reaches.
## Give the client to Agentry and sign in

Most people use Agentry Desktop: give these steps first. Give all of them
before they stop you, because you cannot answer while you are stopped; your
conversation carries on when they start you again.

### In Agentry Desktop

1. **Stop you**: the **Stop** button at the top of your page. The set-up is
   refused while you run.
2. **Switch Google on**: **Configure**, then the **Mail and calendar**
   section. If there is no *Google Workspace (gog)* card there, **Add a
   provider**, then **Add Google**. A click on the card switches it on and
   asks them to agree; it then reads *Needs setting up*.
3. **Open the set-up**: on the card, the **⋯** menu, then **Set up**. (The
   menu offers Set up only once the card is on.)
4. **The client**: **Choose the client file…** and pick the file they
   downloaded in Google Cloud step 4. Agentry hands it to gog, which keeps its
   secret in an encrypted keyring on its own volume; the keyring's password
   goes into their system keychain. They can delete the downloaded file
   afterwards.
5. **Sign in**: their Google address in **Your Google account**, then **Sign
   in with Google**. Their browser opens Google's consent: they choose the
   account, accept the warning of Google Cloud step 5, and allow access.
   Google sends the browser back to Agentry on their own computer, and the
   panel then says which account is signed in.
6. **Start you again**: **Start** at the top of your page.

### In a terminal

If they do not use Agentry Desktop, or its set-up fails, the same steps are
commands in a terminal on this computer:

    agentry agent stop assistant
    agentry agent configure assistant --google-client <the downloaded file>
    agentry agent configure assistant --google-sign-in <their Google address>

The second command hands the file to gog as the desktop does. The third opens
Google's consent in their browser, and the terminal says which account is
signed in. Then:

    agentry agent grant assistant server:gog on
    agentry agent start assistant

Either way, the sign-in asks for read-only access unless *Allow changes* is
on.

## Allowing changes later

In Agentry Desktop, the cards *Google Workspace: allow changes* and *Google
Workspace: allow sending mail* (sending only with changes); in a terminal,
`agentry agent grant assistant server:gog:writes on` and `server:gog:send
on`. Each switch asks for their agreement. A sign-in made read only does not
cover changes, so after switching changes on they stop you and sign in
again.

## When it does not work

- *access_denied* at Google: their address is not a test user (Google Cloud
  step 3) and the app is not published (Google Cloud step 5).
- *invalid_client*: the file is not the client of this project, or the client
  was deleted; download it again (Google Cloud step 4), give it to Agentry
  and sign in again.
- *redirect_uri_mismatch*: the client is a Web application's; make a Desktop
  app client (Google Cloud step 4).
- Your tools answer that nobody is signed in: the sign-in or the switch is
  not done.
- After 7 days the sign-in stopped working: the app is still in Testing;
  publish it (Google Cloud step 5) and sign in again.
