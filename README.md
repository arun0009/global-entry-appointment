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

Pick your enrollment center once; get a push within ~1 minute when a slot opens ([ntfy](https://ntfy.sh) and/or browser/PWA). Free, open source (MIT), no signup — your ntfy topic and/or this device only. Stops after 30 alerts or 30 days.

## Quick start

1. Optional: [ntfy](https://ntfy.sh) on [iOS](https://apps.apple.com/app/ntfy/id1625396347) or [Android](https://play.google.com/store/apps/details?id=io.heckel.ntfy) — create a unique topic.
2. [getglobalentryalerts.com](https://getglobalentryalerts.com/) — pick center, **ntfy** and/or **This device**, submit.

**This device** — allow notifications when prompted:

- **iPhone / iPad** (iOS 16.4+, Safari) — **Share** → **Add to Home Screen**, then open from that icon. Required; Safari does not support push in a normal tab.
- **Android** (Chrome) — allow in the browser tab. Optional: **⋮** → **Install app** or **Add to Home screen**.
- **Mac** (Safari 16+, macOS 13+) — allow in Safari; works in a regular tab.
- **Desktop** (Chrome, Edge, Firefox) — allow in the browser; works in a regular tab.

## How it works

We check CBP's public scheduler every minute for your center. If a slot appears before your latest date, you get ntfy and/or web push; then the subscription ends after 30 alerts or 30 days.

<details>
<summary><strong>For developers</strong></summary>

<br>

Go · Lambda · MongoDB Atlas · [ntfy.sh](https://ntfy.sh) · Web Push (VAPID) · CDK. Needs [AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html), [CDK](https://docs.aws.amazon.com/cdk/latest/guide/work-with-cdk-typescript.html), [Docker](https://www.docker.com/get-started/).

```bash
git clone https://github.com/arun0009/global-entry-appointment.git && cd global-entry-appointment
# export AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION, AWS_ACCOUNT
```

`env.json`: `MONGODB_PASSWORD`, `RECAPTCHA_SECRET_KEY`, optional VAPID keys (`npx web-push generate-vapid-keys`). `VAPID_SUBJECT` = bare email, not `mailto:…` (Apple 403 otherwise).

<details>
<summary>MongoDB indexes</summary>

<br>

```js
db.subscriptions.createIndex({ location: 1, ntfyTopic: 1 }, { unique: true, partialFilterExpression: { ntfyTopic: { $gt: "" } } });
db.subscriptions.createIndex({ location: 1, "webPushSubscriptions.endpoint": 1 }, { unique: true, partialFilterExpression: { "webPushSubscriptions.0": { $exists: true } } });
db.subscriptions.createIndex({ lastNotifiedAt: 1, latestDate: 1 });
db.subscriptions.createIndex({ createdAt: 1 });
db.subscriptions.createIndex({ latestDate: 1 });
```

Drop a legacy non-partial unique on `(location, ntfyTopic)` if present.

</details>

`make develop` · `make invoke` · `make test` · `make css` · `make deploy` · `make destroy`

`make css` after Tailwind/HTML edits (`npm install` once). reCAPTCHA: register `getglobalentryalerts.com` and `*.github.io`.

</details>

## Support

Free to use. If this saved you a refresh war, a coffee or a [star](https://github.com/arun0009/global-entry-appointment/stargazers) is always welcome.

[![Buy me a coffee](https://img.buymeacoffee.com/button-api/?text=Buy%20me%20a%20coffee&emoji=&slug=arun0009&button_colour=5F7FFF&font_colour=ffffff&font_family=Inter&outline_colour=000000&coffee_colour=FFDD00?b)](https://www.buymeacoffee.com/arun0009)

## License

MIT © 2026 [Arun Gopalpuri](https://github.com/arun0009)
