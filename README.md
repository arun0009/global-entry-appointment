<div align="center">

# 🌍 Global Entry Appointment Scanner

<img src="https://img.shields.io/badge/Status-Live-brightgreen?style=for-the-badge" alt="Status"/>
<img src="https://img.shields.io/badge/Cost-FREE-blue?style=for-the-badge" alt="Cost"/>
<img src="https://img.shields.io/badge/Setup-2_Steps-orange?style=for-the-badge" alt="Setup"/>

### 🚀 **Never Miss a Global Entry Appointment Again!**

**Stop refreshing the CBP website endlessly.** Get instant push notifications the moment appointment slots open up near you.

[**🔔 Get Started in 2 Minutes →**](https://arun0009.github.io/global-entry-appointment/)

</div>

---

## ✨ **Why Choose This Tool?**

<div style="display: flex; justify-content: space-between; gap: 20px;">

  <div style="flex: 1; padding: 20px; background: #f9f9f9; border-radius: 10px; box-shadow: 0 4px 8px rgba(0,0,0,0.1);">
    <h3>🎯 Smart & Simple</h3>
    <ul>
      <li>⚡ <b>Real-time scanning</b> every minute</li>
      <li>📱<b>Instant notifications</b> to your phone</li>
      <li>🔒<b>No personal data</b> required</li>
    </ul>
  </div>

  <div style="flex: 1; padding: 20px; background: #f9f9f9; border-radius: 10px; box-shadow: 0 4px 8px rgba(0,0,0,0.1);">
    <h3>💰 Completely Free</h3>
    <ul>
      <li>✅ No login or account needed</li>
      <li>✅ No phone number required</li>
      <li>✅ No hidden fees or subscriptions</li>
      <li>✅ Open source & transparent</li>
    </ul>
  </div>

</div>

## 📲 **Get Started in 2 Simple Steps**

### **Step 1: Install the Ntfy App**
Download the free notification app on your phone:

<p align="center">
  <a href="https://apps.apple.com/app/ntfy/id1625396347">
    <img src="https://img.shields.io/badge/Download-App_Store-000000?style=flat-square&logo=apple&logoColor=white" alt="App Store" height="50"/>
  </a>
  <a href="https://play.google.com/store/apps/details?id=io.heckel.ntfy">
    <img src="https://img.shields.io/badge/Download-Google_Play-3DDC84?style=flat-square&logo=google-play&logoColor=white" alt="Google Play" height="50"/>
  </a>
</p>

### **Step 2: Set Up Your Alerts**
Visit our web app and configure your preferred locations:

<p align="center">
<a href="https://arun0009.github.io/global-entry-appointment">
<img src="https://img.shields.io/badge/🔔_Setup_Alerts-Visit_Web_App-blue?style=for-the-badge&logoColor=white" alt="Setup Alerts" height="60"/>
</a>
</p>

**That's it!** You'll receive notifications for the next 30 days (auto-unsubscribes).

---

## 🛠️ **How It Works**

Our AWS Lambda function continuously monitors Global Entry appointment availability. When slots open up at your selected locations, you get an instant push notification via [Ntfy](https://ntfy.sh/).

---

## 💝 **Love This Project?**

<div align="center">

### **Help Keep This Service Running Free!**

Your support helps cover AWS costs and keeps this tool free for everyone. ☕

<p>
<a href="https://www.buymeacoffee.com/arun0009" target="_blank">
<img src="https://img.shields.io/badge/☕_Buy_Me_A_Coffee-Support_Project-FFDD00?style=for-the-badge&logo=buy-me-a-coffee&logoColor=black" alt="Buy Me A Coffee" height="60"/>
</a>
</p>

**🌟 Star this repo** if it saved you time!

<p>
<a href="https://github.com/arun0009/global-entry-appointment/stargazers">
<img src="https://img.shields.io/github/stars/arun0009/global-entry-appointment?style=social" alt="GitHub stars"/>
</a>
</p>

</div>

---

## 🔧 **For Developers**

### **Prerequisites for AWS Deployment**
- [AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)
- [AWS CDK](https://docs.aws.amazon.com/cdk/latest/guide/work-with-cdk-typescript.html)
- [Docker](https://www.docker.com/get-started/)

```bash
# Clone the repository
git clone https://github.com/arun0009/global-entry-appointment.git
cd global-entry-appointment
```

### **Environment Setup**

```bash
# Set AWS credentials
export AWS_ACCESS_KEY_ID=YOUR_AWS_ACCESS_KEY_ID
export AWS_SECRET_ACCESS_KEY=YOUR_AWS_SECRET_ACCESS_KEY
export AWS_REGION=YOUR_AWS_REGION
export AWS_ACCOUNT=YOUR_AWS_ACCOUNT_ID
```

Create `env.json`:
```json
{
  "Parameters": {
    "MONGODB_PASSWORD": "your-mongodb-password"
  }
}
```

### **Commands**

```bash
# Local development
make develop
make invoke

# Deploy to AWS
make deploy

# Clean up
make destroy
```

---

## 📄 **License**

MIT © 2025 - Made with ❤️ for the Global Entry community

---

<div align="center">

**🔥 Stop waiting. Start getting notified.**

[**Get Started Now →**](https://arun0009.github.io/global-entry-appointment/)

</div>
