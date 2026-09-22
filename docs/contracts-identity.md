# Identity API contract

All timestamps are Unix milliseconds. JSON requests use `Content-Type: application/json`. Authenticated mutations send `X-CSRF-Token` from the login response or `GET /api/me`; cookies are HttpOnly and same-origin. Passwords must contain at least 8 Unicode characters and no more than 72 UTF-8 bytes, must not equal the full account email or its local part, and must not contain a run of 6 or more ASCII digits. Registration, member recovery, admin user edits, and the local admin-password command use the same server-side policy. Public signup uses a numeric QQ mailbox. `role` is `member` or `admin` (`user` accepted as a member input alias); status is `active` or `suspended`.

- `POST /api/auth/register`: `{email,password,inviteCode?,code?,agree:true,turnstileToken?}`. Returns `{user,csrfToken}` and sets session cookie. Invitation consumption and code validation are transactional. Registration verification does not imply free-tool verification and vice versa.
- `POST /api/auth/login`: `{email,password,agree?,turnstileToken?}`. Returns `{user,csrfToken}`.
- `POST /api/auth/logout`: `{}`. Deletes session.
- `GET /api/me`: `{user,csrfToken}`; anonymous returns null user and empty token, HTTP 200.
- `POST /api/auth/email/send`: `{purpose:'register',email,turnstileToken?}` or authenticated `{purpose:'verify'}`. Aliases `registration`/`account` accepted. Actual SMTP delivery, 10 minute expiry, 60 second resend delay, 5 incorrect attempts. Never returns code. Response `{ok,expiresIn:600,retryAfter:60}`.
- `POST /api/auth/email/verify`: `{code}`. Returns `{ok,user}`.
- `GET /api/settings`: public feature/limit settings only. Includes `smtpReady`. Does not expose SMTP host, sender, credentials, Turnstile secret.
- `GET /api/admin/settings`: public settings plus sanitized `smtpConfig` and `turnstileSecretConfigured`.
- `PUT|PATCH /api/admin/settings`: direct partial setting map. Feature keys follow prototype, email keys `registrationEmailVerificationRequired`, `freeToolsRequireVerifiedEmail` (prototype aliases accepted). `smtpConfig:{host,port,sender,name,encryption:'tls'|'starttls',secret}`: name is sender display name, login username is sender email. Empty secret retains existing encrypted credential. Changes reset successful SMTP test. `turnstileSiteKey`, `turnstileSecret` accepted. `limits` requires all 4 rules (`deploy`,`relay`,`dd`,`fingerprint`) each `{minutes:1..1440,count:1..100}`. Validation policies require enabled, successfully tested SMTP; disable policies before changing SMTP or use SMTP test with a replacement config.
- `POST /api/admin/smtp/test`: `{recipient?,smtpConfig?}`. Defaults recipient to admin email. Real send; marks tested only if settings are unchanged during send. Optional SMTP config is saved atomically after successful delivery, allowing safe replacement while verification policies are active.
- `GET /api/admin/users`: `{users:[User]}`. Password hashes never returned.
- `POST /api/admin/users`: `{email,password,role?,status?,balanceCents?,expiresAt?,trafficTotal?,trafficUsed?,rateMbps?,reason?}`. Returns `{user}`. Manually created users start unverified; adjusting money needs a reason.
- `PATCH|PUT /api/admin/users/:id`: same optional fields. Empty password retains old hash. Email change clears verification; email/password/role/status changes invalidate sessions. Money adjustment creates ledger entry; entitlement changes increment version to protect against old payment callbacks. Last/current admin protections enforced.
- `DELETE /api/admin/users/:id`: deletes identity and revokes sessions; audit and financial records retained.
- `POST /api/admin/users/:id/password`: `{password,reason?}`; revokes all user sessions.
- `GET /api/admin/invitations`: `{invitations:[{id,code,maxUses,uses:[{user,at}],enabled,archived,createdAt,note,batch}]}`.
- `POST /api/admin/invitations`: `{quantity:1..100,maxUses:1..10000,note?,batch?,enabled?}`. Returns `{invitations}`; codes generated cryptographically.
- `PATCH /api/admin/invitations/:id`: `{maxUses?,enabled?,archived?,note?}`.
- `DELETE /api/admin/invitations/:id`: archives/disables so use history remains.
- `POST /api/admin/invitations/batch`: `{ids,action:'enable'|'disable'|'delete'|'export'}`; atomic for 1–1000 selected IDs, returns `{invitations,count}` with full codes. Creation also accepts `count` as a `quantity` alias; edits accept PUT as well as PATCH.

`Config.TrustedProxyCIDRs` is a string slice of explicit CIDRs. Default is empty (ignore forwarding headers); a trusted peer can supply X-Forwarded-For, which is walked right to left until the first untrusted hop. SMTP failures are categorized in administrator audit without preserving raw SMTP responses or credentials.

Bootstrap admin is created only from configured `ADMIN_EMAIL` and `ADMIN_PASSWORD`, only when there is no admin. No demo account or fixed verification code exists. Other modules must call `safeUser` before returning a `User` because `PasswordHash` is included in SQL persistence serialization.
