# Microsoft app registration (maintainer setup)

This file is the one-time checklist for registering the default
`suchi` public-client app that ships with the project. It's for
the project maintainer only — end users who bring their own
tenant follow the same steps under their own Azure account and
plug the resulting client ID into `INGEST_IMAP_OAUTH_CLIENT_ID_MICROSOFT`.
Read the user-facing walkthrough in
`docs/guides/mail-intake-oauth.mdx` first if you haven't already.

Steps:

1. Sign in to https://portal.azure.com → **Microsoft Entra ID** →
   **App registrations** → **New registration**.
2. **Name**: `suchi`.
3. **Supported account types**: **Accounts in any organizational
   directory (any Microsoft Entra ID tenant — Multitenant) and
   personal Microsoft accounts (e.g., Skype, Xbox)**. This is the
   `AzureADandPersonalMicrosoftAccount` audience. Anything narrower
   locks out personal `@outlook.com` / `@hotmail.com` users.
4. **Redirect URI**: leave empty. Device-code flow is a public
   client and does not use a redirect.
5. Click **Register**.
6. In the new app: **Authentication** → **Advanced settings** →
   **Allow public client flows** → **Yes** → **Save**. Without
   this, MSAL device-code returns
   `AADSTS7000218: The request body must contain the following
   parameter: 'client_assertion' or 'client_secret'`.
7. **API permissions** → **Add a permission**. Do **not** pick
   Microsoft Graph — Graph's IMAP scope does not authorize
   `outlook.office365.com` IMAP. Instead choose **APIs my
   organization uses**, search for **Office 365 Exchange Online**,
   pick it, then **Delegated permissions** → check
   **IMAP.AccessAsUser.All** → **Add permissions**.
8. Same **Add a permission** flow: **Microsoft Graph** →
   **Delegated permissions** → check **offline_access** → add.
   This is what lets MSAL refresh silently on every poll instead
   of prompting the user every hour.
9. (Optional, tenant deployments only.) Click **Grant admin
   consent for <tenant>**. Personal-account users don't need this
   — they'll consent individually when they hit the device-code
   screen — but granting it once suppresses the prompt for every
   user in your tenant.
10. Back on the app's **Overview** page, copy the **Application
    (client) ID** GUID. Paste it into
    `core/ingest/emailwatch/oauth/microsoft.go` as the value of
    `DefaultClientID`.

This is a one-time setup. The client ID is a public identifier,
not a secret, and is safe to commit to the source tree — device-code
flow does not use a client secret.
