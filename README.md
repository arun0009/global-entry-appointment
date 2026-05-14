<div align="center">

# Global Entry Appointment Alerts

Free, real-time push notifications when Global Entry appointment slots open up at your enrollment center.

[**getglobalentryalerts.com →**](https://getglobalentryalerts.com/)

[![Live](https://img.shields.io/website?url=https%3A%2F%2Fgetglobalentryalerts.com&up_message=live&down_message=offline&label=site)](https://getglobalentryalerts.com/)
[![GitHub Stars](https://img.shields.io/github/stars/arun0009/global-entry-appointment?style=social)](https://github.com/arun0009/global-entry-appointment/stargazers)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

</div>

---

## What it does

Stop refreshing the CBP website endlessly. Pick your enrollment center once, and the service watches it for you. When a slot opens up, you get a push within about a minute via the free [ntfy](https://ntfy.sh) app and/or **browser notifications** (installable PWA on Android and iPhone).

- **100% free** — no ads, no signup, no personal data.
- **Open source** — MIT licensed.
- **Real-time** — checks every minute via CBP's public scheduler API.
- **No accounts** — bring your own ntfy topic and/or allow web push on this device; no email or phone needed.
- **Auto-expires** — 30 alerts or 30 days, whichever comes first.

## Quick start

1. (Optional) Install ntfy: [iOS](https://apps.apple.com/app/ntfy/id1625396347) · [Android](https://play.google.com/store/apps/details?id=io.heckel.ntfy) and create a unique topic (e.g. `global-entry-alerts-arun`).
2. Open [getglobalentryalerts.com](https://getglobalentryalerts.com/), pick your enrollment center, choose **ntfy** and/or **This device** (browser / PWA push), then submit.

That's it — you'll get a notification when slots open up. On **iPhone**, add the site to your Home Screen first for the most reliable web push (iOS 16.4+).

## How it works

A scheduled AWS Lambda runs every minute. For each subscribed location, it queries CBP's public scheduler API. If a slot is available before your latest acceptable date, it sends ntfy and/or a **Web Push** to registered browsers. Subscriptions auto-expire after 30 alerts or 30 days, with a final "we're done" message.

Stack: Go on AWS Lambda + MongoDB Atlas + [ntfy.sh](https://ntfy.sh) + optional Web Push (VAPID), deployed via AWS CDK.

## For developers

### Prerequisites

- [AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)
- [AWS CDK](https://docs.aws.amazon.com/cdk/latest/guide/work-with-cdk-typescript.html)
- [Docker](https://www.docker.com/get-started/)

### Setup

```bash
git clone https://github.com/arun0009/global-entry-appointment.git
cd global-entry-appointment

export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
export AWS_REGION=...
export AWS_ACCOUNT=...
```

Create `env.json`:

```json
{
  "Parameters": {
    "MONGODB_PASSWORD": "your-mongodb-password",
    "RECAPTCHA_SECRET_KEY": "your-recaptcha-secret-key",
    "VAPID_PUBLIC_KEY": "your-vapid-public-key",
    "VAPID_PRIVATE_KEY": "your-vapid-private-key",
    "VAPID_SUBJECT": "you@example.com"
  }
}
```

Generate a VAPID key pair (install [web-push](https://www.npmjs.com/package/web-push) globally or use `npx web-push generate-vapid-keys`). Set `VAPID_SUBJECT` to a **bare email** (e.g. `you@example.com`) or an `https://` URL you control. The push library turns a bare email into `mailto:…` in the VAPID JWT; if you set `VAPID_SUBJECT` to `mailto:…` yourself, the JWT ends up as `mailto:mailto:…` and **Apple Web Push returns 403**. If `VAPID_PUBLIC_KEY` / `VAPID_PRIVATE_KEY` are omitted, the API still works for **ntfy-only** subscriptions; browser push is disabled until both keys are set.

> If you serve the static site from both `getglobalentryalerts.com` and `*.github.io`, register both hostnames in [reCAPTCHA admin](https://www.google.com/recaptcha/admin). The API already allows both origins via CORS.

### MongoDB indexes

For production performance, create these once on the `subscriptions` collection. If you previously created a unique index on `(location, ntfyTopic)` only, replace it with the partial index below so multiple **web-only** rows (empty `ntfyTopic`) are allowed.

```js
// Drop legacy unique index if it exists (name may differ — check db.subscriptions.getIndexes())
// db.subscriptions.dropIndex("location_1_ntfyTopic_1");

// Unique when an ntfy topic is present (non-empty string)
db.subscriptions.createIndex(
  { location: 1, ntfyTopic: 1 },
  { unique: true, partialFilterExpression: { ntfyTopic: { $gt: "" } } }
);

// At most one subscription document per push endpoint per location
db.subscriptions.createIndex(
  { location: 1, "webPushSubscriptions.endpoint": 1 },
  { unique: true, partialFilterExpression: { "webPushSubscriptions.0": { $exists: true } } }
);

// Cron query: subscriptions due for a notification check
db.subscriptions.createIndex({ lastNotifiedAt: 1, latestDate: 1 });

// Daily expiration sweep
db.subscriptions.createIndex({ createdAt: 1 });
db.subscriptions.createIndex({ latestDate: 1 });
```

### Commands

```bash
make develop    # local development
make invoke     # invoke lambda locally
make test       # run tests (requires Docker)
make css        # rebuild docs/styles.css from tailwind.css (only if you change styles)
make deploy     # deploy to AWS
make destroy    # tear down stack
```

The static site at `docs/` ships a precompiled `docs/styles.css` (Tailwind v4) — no CDN, no in-browser compilation. If you edit `tailwind.css` or add new utility classes to `docs/index.html`, run `make css` and commit the regenerated stylesheet. `npm install` once to pull the dev dependencies.

## Support

Free to use. If this saved you a refresh war, a coffee or a [star](https://github.com/arun0009/global-entry-appointment/stargazers) is always welcome.

[![Buy me a coffee](https://img.buymeacoffee.com/button-api/?text=Buy%20me%20a%20coffee&emoji=&slug=arun0009&button_colour=5F7FFF&font_colour=ffffff&font_family=Cookie&outline_colour=000000&coffee_colour=FFDD00?b)](https://www.buymeacoffee.com/arun0009)

## License

MIT © 2026 [Arun Gopalpuri](https://github.com/arun0009)
