<h1 align="center">Global Entry Appointment Scanner</h1>

<p align="center">
   <img src="./docs/favicon.png" alt="Global Entry"/><img src="./docs/favicon.png" alt="Global Entry"/><img src="./docs/favicon.png" alt="Global Entry"/>
</p>

**🚀 Instantly Get Notified When Global Entry Appointments Open Near You!**

Tired of endlessly checking for Global Entry interview availability? 

**This free and open-source tool scans appointment slots every minute** and sends real-time push notifications right to your phone.**

✨ **Absolutely FREE**  
⚡ **Super Simple – Just 2 Steps!**  
🔔 **Get Notified Instantly with Ntfy App**

---

### 🔧 How It Works

Every minute, an AWS Lambda function checks for open [Global Entry](https://www.cbp.gov/travel/trusted-traveler-programs/global-entry) appointments. If an available slot is found at your selected location, you'll get a push notification via [Ntfy](https://ntfy.sh/).

✅ No login required  
✅ No account creation  
✅ No phone number required  
✅ No spam — auto-unsubscribes after 7 days (1 week) unless you resubscribe

---

### 📲 **Get Started in 2 Easy Steps**

#### 1. **Install the Free Ntfy App & subscribe to a topic (create your own random one)**
- [📱 App Store (iOS)](https://apps.apple.com/app/ntfy/id1625396347)
- [🤖 Play Store (Android)](https://play.google.com/store/apps/details?id=io.heckel.ntfy&hl=en_US)

#### 2. **Subscribe **

[Global Entry Appointment Subscribe](https://arun0009.github.io/global-entry-appointment/)

✅ That’s it! You’ll now receive alerts when appointments become available for the next 7 days.

### ❌ Unsubscribe Anytime

To unsubscribe before the 7 days are up:

[Global Entry Appointment Unsubscribe](https://arun0009.github.io/global-entry-appointment/?subscriptions=unsubscribe)

### ☕ Like This Project?

If this tool saved you hours of frustration, consider buying me a coffee to support ongoing development:

<a href="https://www.buymeacoffee.com/arun0009" target="_blank"><img src="https://www.buymeacoffee.com/assets/img/custom_images/orange_img.png" alt="Buy Me A Coffee" style="height: 41px !important;width: 174px !important;box-shadow: 0px 3px 2px 0px rgba(190, 190, 190, 0.5) !important;-webkit-box-shadow: 0px 3px 2px 0px rgba(190, 190, 190, 0.5) !important;" ></a>

### 👨‍💻 Developers: Want to Contribute or Run It Yourself?

Prerequisites:  
1. [AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)    
2. [AWS CDK](https://docs.aws.amazon.com/cdk/latest/guide/work-with-cdk-typescript.html):  
3. `npm install -g aws-cdk`
4. [Docker](https://www.docker.com/get-started/)

#### Set AWS Credentials

```bash
export AWS_ACCESS_KEY_ID=YOUR_AWS_ACCESS_KEY_ID
export AWS_SECRET_ACCESS_KEY=YOUR_AWS_SECRET_ACCESS_KEY
export AWS_REGION=YOUR_AWS_REGION
export AWS_ACCOUNT=YOUR_AWS_ACCOUNT_ID
```

#### Setup Env

Add your MongoDB password in file called `env.json`:

```json
{
  "Parameters": {
    "MONGODB_PASSWORD": "<your-password>"
  }
}
```

#### Run Locally
```bash
make develop
make invoke
```

#### Deploy to AWS
```bash
make deploy
```

#### Destroy Stack
```bash
make destroy
```

### 📄 License

MIT © 2025 Arun
